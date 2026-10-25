package guest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"syscall"

	"golang.org/x/sys/unix"
	"safeagent/internal/protocol"
	"safeagent/internal/workspacebundle"
)

// exportWorkspace 只在工作负载组已清空后读取工作区，拒绝链接与特殊文件。
func (c *connection) exportWorkspace(ctx context.Context, params any) (any, error) {
	b := c.boot
	b.exportMu.Lock()
	defer b.exportMu.Unlock()
	b.mu.Lock()
	ready := b.importState == importCommitted && b.config.WorkspaceRoot != nil && !b.reserved
	for _, s := range b.sessions {
		ready = ready && s.info.GroupState == protocol.GroupStateEmpty
	}
	if ready {
		b.exportStarted = true
	}
	b.mu.Unlock()
	if !ready {
		return nil, protocol.Failure(protocol.CodeFailedPrecondition)
	}
	root, err := os.OpenRoot(b.config.WorkspaceRoot.Name() + "/project")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	switch p := params.(type) {
	case *protocol.ReadChangeChunkParams:
		f, info, err := exportFile(root, p.Path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if p.Offset > info.Size() {
			return nil, protocol.Failure(protocol.CodeInvalidArgument)
		}
		data := make([]byte, min(int64(protocol.ExportChunkBytes), info.Size()-p.Offset))
		_, err = f.ReadAt(data, p.Offset)
		return protocol.ChangeChunk{Data: data}, err
	case *protocol.ListChangesParams:
		entries := b.exportEntries
		if entries == nil {
			entries = []workspacebundle.Entry{}
			limits := workspacebundle.GuestLimits()
			var total int64
			err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if err = ctx.Err(); err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				if len(path) > limits.MaxPathBytes || len(entries) >= limits.MaxFiles {
					return workspacebundle.ErrLimitExceeded
				}
				f, info, err := exportFile(root, path)
				if err != nil {
					return err
				}
				defer f.Close()
				h := sha256.New()
				n, err := io.Copy(h, io.LimitReader(f, limits.MaxFileBytes+1))
				if err != nil {
					return err
				}
				total += n
				if n > limits.MaxFileBytes || total > limits.MaxTotalBytes {
					return workspacebundle.ErrLimitExceeded
				}
				entries = append(entries, workspacebundle.Entry{Path: path, Size: n, Executable: info.Mode().Perm()&0111 != 0, Digest: fmt.Sprintf("sha256:%x", h.Sum(nil))})
				return nil
			})
			if err != nil {
				return nil, err
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
			b.exportEntries = entries
		}
		if p.Cursor > len(entries) {
			return nil, protocol.Failure(protocol.CodeInvalidArgument)
		}
		end := min(p.Cursor+8, len(entries))
		next := end
		if next == len(entries) {
			next = 0
		}
		return protocol.ChangePage{Entries: entries[p.Cursor:end], Next: next}, nil
	}
	panic("已解码的导出参数类型错误")
}

// exportFile 在目录约束内无链接打开普通文件，拒绝硬链接及特殊文件。
func exportFile(root *os.Root, path string) (*os.File, os.FileInfo, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, protocol.Failure(protocol.CodeInvalidArgument)
	}
	f, err := root.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err = f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		f.Close()
		return nil, nil, protocol.Failure(protocol.CodeInvalidArgument)
	}
	return f, info, nil
}
