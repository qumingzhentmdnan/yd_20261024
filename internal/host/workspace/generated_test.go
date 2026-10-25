package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestGeneratedConfiguration 验证公开配置只进入快照且重启后仍可查询导出排除路径。
func TestGeneratedConfiguration(t *testing.T) {
	s, root := testStore(t)
	writeTestFile(t, root, "main.go", []byte("package main\n"), 0644)
	const name = ".codex/config.toml"
	const config = "model = \"custom-model\"\n"
	baseline, err := s.Stage(t.Context(), ImportSelection{Root: root, Paths: []string{"main.go"}, Generated: map[string][]byte{name: []byte(config)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".codex")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("源项目出现生成目录: %v", err)
	}
	reopened, err := NewStore(s.dataDir, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	paths, err := reopened.GeneratedPaths(t.Context(), baseline.ID)
	if err != nil || !slices.Equal(paths, []string{name}) {
		t.Fatalf("恢复排除列表: %v %v", paths, err)
	}
	archive := tar.NewReader(bytes.NewReader(readImport(t, reopened, baseline.ID)))
	found := false
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "files/"+name {
			data, err := io.ReadAll(archive)
			if err != nil || string(data) != config {
				t.Fatal("生成配置内容不一致", err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("导入包缺少生成配置")
	}
}

// TestGeneratedConfigurationBoundaries 验证生成配置不能覆盖源文件、选择路径、链接或突破容量。
func TestGeneratedConfigurationBoundaries(t *testing.T) {
	for _, scenario := range []string{"selected", "existing-unselected", "symlink-parent", "traversal", "limit"} {
		t.Run(scenario, func(t *testing.T) {
			s, root := testStore(t)
			selection := ImportSelection{Root: root, Generated: map[string][]byte{".codex/config.toml": []byte("public")}}
			switch scenario {
			case "selected":
				selection.Paths = []string{".codex/config.toml"}
			case "existing-unselected":
				writeTestFile(t, root, ".codex/config.toml", []byte("user-content"), 0600)
			case "symlink-parent":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, ".codex")); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				selection.Generated = map[string][]byte{"../escape": []byte("public")}
			case "limit":
				selection.Generated[".codex/config.toml"] = make([]byte, testLimits().MaxFileBytes+1)
			}
			if _, err := s.Stage(context.Background(), selection); err == nil {
				t.Fatal("不安全的生成配置被接受")
			}
		})
	}
}
