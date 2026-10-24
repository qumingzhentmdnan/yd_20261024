package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// openSourceRoot 从根目录逐组件打开授权目录，全程拒绝符号链接。
func openSourceRoot(root string) (*os.File, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrInvalidSelection
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if root != "/" {
		for _, part := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
			next, openErr := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			syscall.Close(fd)
			if openErr != nil {
				return nil, safeError("打开源目录", "", openErr)
			}
			fd = next
		}
	}
	return os.NewFile(uintptr(fd), "source"), nil
}

// openSourceFile 通过授权目录句柄打开普通文件，避免路径检查与打开分离。
func openSourceFile(root *os.File, relative string) (*os.File, os.FileInfo, error) {
	parts := strings.Split(relative, "/")
	fd, err := syscall.Openat(int(root.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	for i, part := range parts {
		flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= syscall.O_DIRECTORY
		}
		next, openErr := syscall.Openat(fd, part, flags, 0)
		syscall.Close(fd)
		if openErr != nil {
			return nil, nil, safeError("打开源文件", relative, openErr)
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), relative)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, safeError("检查源文件", relative, err)
	}
	if !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink > 1 {
		f.Close()
		return nil, nil, fmt.Errorf("%w: 只允许无硬链接的普通文件", ErrInvalidSelection)
	}
	return f, info, nil
}

// sourceChanged 比较同一已打开文件复制前后的大小及修改时间。
func sourceChanged(before, after os.FileInfo) bool {
	first := before.Sys().(*syscall.Stat_t)
	second := after.Sys().(*syscall.Stat_t)
	return first.Size != second.Size || first.Mtim != second.Mtim || first.Ctim != second.Ctim
}
