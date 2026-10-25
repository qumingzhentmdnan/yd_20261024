package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Apply 仅接受冻结内容ID，先检查全部宿主冲突，再逐文件原子替换。
func (r *Review) Apply(ctx context.Context, approvedID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if approvedID != r.ID() {
		return fmt.Errorf("未批准当前变更集")
	}
	lock, err := os.OpenFile(filepath.Join(r.store.dataDir, "export.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("其他导出正在运行: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	record, err := readBoundedFile(ctx, filepath.Join(r.store.dataDir, "baselines", string(r.record.BaselineID), "host.json"), maxHostMetadataBytes)
	if err != nil {
		return err
	}
	var host hostRecord
	if err = json.Unmarshal(record, &host); err != nil {
		return err
	}
	root, err := openSourceRoot(host.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	// 对所有文件完成预检后才创建目录或替换文件；冲突不会静默覆盖。
	for _, c := range r.record.Changes {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = checkVersion(root, c.Path, c.Before); err != nil {
			return err
		}
	}
	for i, c := range r.record.Changes {
		if err = ctx.Err(); err != nil {
			return fmt.Errorf("已应用%d个文件: %w", i, err)
		}
		if err = applyChange(root, c); err != nil {
			return fmt.Errorf("已应用%d个文件，%s失败: %w", i, c.Path, err)
		}
	}
	return nil
}

// checkVersion 拒绝路径链接、宿主新增、宿主删除和内容或权限冲突。
func checkVersion(root *os.File, relative string, expected *FileVersion) error {
	f, info, err := openSourceFile(root, relative)
	if errors.Is(err, os.ErrNotExist) {
		if expected == nil {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrConflict, relative)
	}
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrConflict, relative, err)
	}
	defer f.Close()
	if expected == nil || info.Size() != int64(len(expected.Data)) {
		return fmt.Errorf("%w: %s", ErrConflict, relative)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(len(expected.Data))+1))
	if err != nil {
		return err
	}
	if !equalVersion(expected, &FileVersion{Data: data, Executable: info.Mode().Perm()&0111 != 0}) {
		return fmt.Errorf("%w: %s", ErrConflict, relative)
	}
	return nil
}

// changeParent 逐层以无链接目录句柄进入授权目录，新建目录仅用于新增文件。
func changeParent(root *os.File, relative string, create bool) (int, error) {
	fd, err := unix.Openat(int(root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	parent := path.Dir(relative)
	if parent == "." {
		return fd, nil
	}
	for _, part := range strings.Split(parent, "/") {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(fd, part, 0755); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(fd)
				return -1, err
			}
			next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// applyChange 固定父目录后复查当前版本，使用同目录临时文件替换，绝不跟随目标链接。
func applyChange(root *os.File, c Change) error {
	parent, err := changeParent(root, c.Path, c.After != nil)
	if err != nil {
		return err
	}
	parentFile := os.NewFile(uintptr(parent), "export-parent")
	defer parentFile.Close()
	name := path.Base(c.Path)
	if err = checkVersion(parentFile, name, c.Before); err != nil {
		return err
	}
	temp := newID(".safeagent-new-")
	if c.After != nil {
		fd, err := unix.Openat(parent, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err != nil {
			return err
		}
		defer unix.Unlinkat(parent, temp, 0)
		f := os.NewFile(uintptr(fd), temp)
		_, writeErr := f.Write(c.After.Data)
		mode := os.FileMode(0644)
		if c.After.Executable {
			mode = 0755
		}
		if writeErr == nil {
			writeErr = f.Chmod(mode)
		}
		if writeErr == nil {
			writeErr = f.Sync()
		}
		if err = errors.Join(writeErr, f.Close()); err != nil {
			return err
		}
	}
	backup := ""
	if c.Before != nil {
		// 先原子移走旧文件再验证被移走的实际内容，避免check/rename窗口覆盖宿主编辑。
		backup = newID(".safeagent-backup-")
		if err = unix.Renameat2(parent, name, parent, backup, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		if err = checkVersion(parentFile, backup, c.Before); err != nil {
			restoreErr := unix.Renameat2(parent, backup, parent, name, unix.RENAME_NOREPLACE)
			return errors.Join(err, restoreErr)
		}
	}
	if c.After != nil {
		// 新版发布使用NOREPLACE，期间任何宿主新写入都保留；失败的旧版留在备份名。
		if err = unix.Renameat2(parent, temp, parent, name, unix.RENAME_NOREPLACE); err != nil {
			if backup != "" {
				restoreErr := unix.Renameat2(parent, backup, parent, name, unix.RENAME_NOREPLACE)
				return fmt.Errorf("%w: %s；恢复备份%s: %v", ErrConflict, c.Path, backup, errors.Join(err, restoreErr))
			}
			return fmt.Errorf("%w: %s: %v", ErrConflict, c.Path, err)
		}
	}
	// 保留旧inode对应备份；宿主编辑器可能仍持有旧FD，不能删除其并发写入。
	// .safeagent-backup-*由用户确认后清理，失败时也保留以避免数据损失。
	return unix.Fsync(parent)
}
