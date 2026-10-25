package workspacebundle

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// ownedFile 持有私有树中的新文件与最终所有权。
type ownedFile struct {
	// File 是排他创建且不跟随链接的文件。
	*os.File
	// uid 是最终工作负载用户。
	uid int
	// gid 是最终工作负载用户组。
	gid int
}

// Close 设置最终所有权并释放句柄，错误不被关闭操作覆盖。
func (f *ownedFile) Close() error { return errors.Join(f.Chown(f.uid, f.gid), f.File.Close()) }

// createFile 从可信树句柄逐层创建目录并排他打开普通文件。
func createFile(root int, entry Entry, uid, gid int) (io.WriteCloser, error) {
	fd, err := unix.Dup(root)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(entry.Path, "/")
	for _, part := range parts[:len(parts)-1] {
		err = unix.Mkdirat(fd, part, 0755)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(fd)
			return nil, err
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
		if e = errors.Join(unix.Fchmod(fd, 0755), unix.Fchown(fd, uid, gid)); e != nil {
			unix.Close(fd)
			return nil, e
		}
	}
	mode := uint32(0644)
	if entry.Executable {
		mode = 0755
	}
	target, err := unix.Openat(fd, parts[len(parts)-1], unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	unix.Close(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(target), entry.Path)
	// 显式权限不继承守护进程umask，文件仍处于root私有外层之内。
	if err = f.Chmod(os.FileMode(mode)); err != nil {
		f.Close()
		return nil, err
	}
	return &ownedFile{File: f, uid: uid, gid: gid}, nil
}

// Extract 单遍校验并写入调用方root私有外层中的树，失败产物由调用方整体清理。
func Extract(ctx context.Context, source io.Reader, limits Limits, tree *os.File, uid, gid int) (Manifest, error) {
	root := int(tree.Fd())
	m, err := scan(ctx, source, limits, func(entry Entry) (io.WriteCloser, error) { return createFile(root, entry, uid, gid) })
	if err != nil {
		return m, err
	}
	return m, errors.Join(unix.Fchown(root, uid, gid), unix.Fchmod(root, 0755))
}
