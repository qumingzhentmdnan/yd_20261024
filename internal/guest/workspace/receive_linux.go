// Package workspace 接收规范工作区包并准备原子发布，不拥有VM生命周期。
package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"golang.org/x/sys/unix"
	"safeagent/internal/workspacebundle"
	"safeagent/internal/workspacewire"
)

// Receive 将归档与提取树放在root私有外层，发布回调由Boot串行提交真实记录。
func Receive(ctx context.Context, port net.Conn, root *os.File, begin workspacewire.BeginMeta, commit func(workspacewire.Imported, func() error) error) (result error) {
	limits := workspacebundle.GuestLimits()
	if begin.ArchiveBytes > limits.MaxArchiveBytes {
		return workspacebundle.ErrLimitExceeded
	}
	relative := ".import-" + rand.Text()
	if err := unix.Mkdirat(int(root.Fd()), relative, 0700); err != nil {
		return err
	}
	defer func() { result = errors.Join(result, removeDirectory(int(root.Fd()), relative)) }()
	dir, err := unix.Openat(int(root.Fd()), relative, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if err = unix.Mkdirat(dir, "tree", 0755); err != nil {
		return err
	}
	fd, err := unix.Openat(dir, "bundle.tar", unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	archive := os.NewFile(uintptr(fd), "bundle.tar")
	defer archive.Close()
	total, _ := ctx.Deadline()
	if err = workspacewire.Write(port, workspacewire.Frame{Kind: workspacewire.Ready}, workspacewire.Deadline(total, workspacewire.Budget)); err != nil {
		return err
	}
	var received int64
	for {
		frame, e := workspacewire.Read(port, workspacewire.Deadline(total, workspacewire.Budget))
		if e != nil {
			return e
		}
		if frame.Kind == workspacewire.End {
			if received != begin.ArchiveBytes {
				return workspacewire.ErrFrame
			}
			break
		}
		if frame.Kind != workspacewire.Data || int64(len(frame.Payload)) > begin.ArchiveBytes-received {
			return workspacewire.ErrFrame
		}
		if _, e = archive.Write(frame.Payload); e != nil {
			return e
		}
		received += int64(len(frame.Payload))
	}
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	treeFD, err := unix.Openat(dir, "tree", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	tree := os.NewFile(uintptr(treeFD), "tree")
	defer tree.Close()
	manifest, err := workspacebundle.Extract(ctx, archive, limits, tree, 1000, 1000)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(manifest)
	if fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != string(begin.WorkspaceDigest) {
		return workspacebundle.ErrCorruptBaseline
	}
	imported := workspacewire.Imported{WorkspaceRef: begin.WorkspaceRef, WorkspaceDigest: begin.WorkspaceDigest, FileCount: len(manifest.Entries)}
	for _, entry := range manifest.Entries {
		imported.TotalBytes += entry.Size
	}
	if err = archive.Close(); err != nil {
		return err
	}
	if err = unix.Unlinkat(dir, "bundle.tar", 0); err != nil {
		return err
	}
	// Boot锁内只执行最终rename与内存登记，不持锁传输或校验。
	if err = commit(imported, func() error { return unix.Renameat2(dir, "tree", int(root.Fd()), "project", unix.RENAME_NOREPLACE) }); err != nil {
		return err
	}
	payload, err := workspacewire.Encode(imported)
	if err != nil {
		return err
	}
	return workspacewire.Write(port, workspacewire.Frame{Kind: workspacewire.Committed, Payload: payload}, workspacewire.Deadline(total, workspacewire.Budget))
}

// removeDirectory 只通过持有的根句柄递归清理本调用创建的私有目录，不跟随链接。
func removeDirectory(parent int, name string) error {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), name)
	entries, err := dir.ReadDir(-1)
	if err != nil {
		dir.Close()
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			err = removeDirectory(fd, entry.Name())
		} else {
			err = unix.Unlinkat(fd, entry.Name(), 0)
		}
		if err != nil {
			dir.Close()
			return err
		}
	}
	return errors.Join(dir.Close(), unix.Unlinkat(parent, name, unix.AT_REMOVEDIR))
}
