package workspacebundle_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"safeagent/internal/host/workspace"
	"safeagent/internal/workspacebundle"
)

// sample 通过真实Host生成规范中文、二进制和可执行文件归档。
func sample(t *testing.T) []byte {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"中文.bin": {0, 255, 1}, "empty": {}, "nested/data": []byte("nested"), "run.sh": []byte("#!/bin/sh\nexit 0\n")} {
		mode := os.FileMode(0644)
		if name == "run.sh" {
			mode = 0755
		}
		if err := os.WriteFile(filepath.Join(source, name), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	store, err := workspace.NewStore(filepath.Join(root, "store"), workspacebundle.GuestLimits())
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.Stage(context.Background(), workspace.ImportSelection{Root: source, Paths: []string{"中文.bin", "empty", "run.sh", "nested/data"}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenImport(context.Background(), base.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestExtractBoundaries 验证单遍规范提取、返回摘要、尾部与容量边界。
func TestExtractBoundaries(t *testing.T) {
	data := sample(t)
	for _, scenario := range []string{"valid", "trailing", "limit", "corrupt", "cancel", "existing", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			tree := t.TempDir()
			fd, err := os.Open(tree)
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			input := bytes.Clone(data)
			limits := workspacebundle.GuestLimits()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "trailing":
				input = append(input, 1)
			case "limit":
				limits.MaxFileBytes = 1
			case "corrupt":
				input[bytes.Index(input, []byte{0, 255, 1})] ^= 1
			case "cancel":
				cancel()
			case "existing":
				os.WriteFile(filepath.Join(tree, "empty"), []byte("keep"), 0600)
			case "symlink":
				os.Symlink(t.TempDir(), filepath.Join(tree, "nested"))
			}
			m, err := workspacebundle.Extract(ctx, bytes.NewReader(input), limits, fd, os.Getuid(), os.Getgid())
			if scenario != "valid" {
				if err == nil {
					t.Fatal("非法输入成功")
				}
				if scenario == "limit" && !errors.Is(err, workspacebundle.ErrLimitExceeded) {
					t.Fatal(err)
				}
				if scenario == "existing" {
					value, _ := os.ReadFile(filepath.Join(tree, "empty"))
					if string(value) != "keep" {
						t.Fatal("已有文件被覆盖")
					}
				}
				return
			}
			if err != nil || len(m.Entries) != 4 {
				t.Fatal(m, err)
			}
			output, err := os.ReadFile(filepath.Join(tree, "中文.bin"))
			if err != nil || !bytes.Equal(output, []byte{0, 255, 1}) {
				t.Fatal(output, err)
			}
			info, err := os.Stat(filepath.Join(tree, "run.sh"))
			if err != nil || info.Mode().Perm() != 0755 {
				t.Fatal(info, err)
			}
		})
	}
}
