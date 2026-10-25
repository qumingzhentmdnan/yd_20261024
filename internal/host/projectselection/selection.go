// Package projectselection 为用户提供 Git 文件默认选择与敏感文件提示。
package projectselection

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// File 描述候选文件，敏感文件始终默认取消选择。
type File struct {
	// Path 是相对项目根目录的文件路径。
	Path string `json:"path"`
	// Selected 仅对 Git 跟踪且无敏感提示的文件为真。
	Selected bool `json:"selected"`
	// Warning 解释敏感路径或内容；空值表示未检出。
	Warning string `json:"warning,omitempty"`
}

// List 读取 Git 的跟踪与未忽略文件，拒绝符号链接和非普通文件。
func List(ctx context.Context, root string) ([]File, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("项目路径必须为绝对路径")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if canonical != filepath.Clean(root) {
		return nil, fmt.Errorf("项目路径不能包含符号链接")
	}
	top, err := gitOutput(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(string(top)) != root {
		return nil, fmt.Errorf("请选择 Git 仓库根目录")
	}
	tracked, err := gitOutput(ctx, root, "ls-files", "-z", "--cached")
	if err != nil {
		return nil, err
	}
	extra, err := gitOutput(ctx, root, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	directory, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	files := []File{}
	seen := map[string]bool{}
	for group, data := range [][]byte{tracked, extra} {
		for _, path := range strings.Split(string(data), "\x00") {
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			full := filepath.Join(root, path)
			resolved, err := filepath.EvalSymlinks(full)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			if resolved != full {
				continue
			}

			fd, err := unix.Openat2(int(directory.Fd()), path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) {
				continue
			}
			if err != nil {
				return nil, err
			}
			file := os.NewFile(uintptr(fd), path)
			info, err := file.Stat()
			if err != nil {
				file.Close()
				return nil, err
			}
			if !info.Mode().IsRegular() {
				file.Close()
				continue
			}
			warning := SensitivePath(path)
			if warning == "" {
				data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
				if err != nil {
					file.Close()
					return nil, err
				}
				if len(data) > 1<<20 {
					warning = "文件超过1MiB，未扫描全部内容，默认不导入"
				} else if strings.Contains(string(data), "PRIVATE KEY-----") || strings.Contains(string(data), "aws_secret_access_key") {
					warning = "内容疑似包含密钥"
				}
			}
			file.Close()

			if len(files) >= 4096 {
				return nil, fmt.Errorf("项目候选超过4096文件，请缩小范围")
			}
			files = append(files, File{Path: path, Selected: group == 0 && warning == "", Warning: warning})
		}
	}
	return files, nil
}

// SensitivePath 标记常见凭据路径，不能保证识别全部秘密。
func SensitivePath(path string) string {
	lower := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(lower)
	if base == ".npmrc" || base == ".pypirc" || base == ".netrc" || base == ".git-credentials" || base == "auth.json" || base == ".credentials.json" || lower == ".docker/config.json" || lower == ".kube/config" || strings.Contains("/"+lower, "/.azure/") || base == ".env" || strings.HasPrefix(base, ".env.") || base == "credentials" || base == "id_rsa" || base == "id_ed25519" || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.HasPrefix(lower, ".git/") || strings.Contains("/"+lower, "/.aws/") || strings.Contains("/"+lower, "/.ssh/") || strings.Contains("/"+lower, "/.config/gcloud/") {
		return "路径可能包含凭据，默认不导入"
	}
	return ""
}

// gitOutput 对仓库提供的文件索引设置字节上限，不启用fsmonitor外部命令。
func gitOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false", "-C", root}, args...)...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(pipe, (4<<20)+1))
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	if len(data) > 4<<20 {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("Git候选输出超过4MiB")
	}
	if err = cmd.Wait(); err != nil {
		return nil, err
	}
	return data, nil
}
