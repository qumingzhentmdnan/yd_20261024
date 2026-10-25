package projectselection

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// git 在临时测试仓库运行显式 Git 命令。
func git(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git: %s %v", output, err)
	}
}

// TestList 验证默认只选跟踪文件，敏感内容取消选择且不跟随链接。
func TestList(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	for path, data := range map[string]string{"main.go": "package main", ".env": "SECRET=x", "cert.txt": "-----BEGIN PRIVATE KEY-----", "extra.txt": "extra", ".gitignore": "ignored\n", "ignored": "ignore"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "add", "main.go", ".env", "cert.txt", ".gitignore")
	if err := os.Symlink("main.go", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	files, err := List(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]File{}
	for _, f := range files {
		got[f.Path] = f
	}
	if !got["main.go"].Selected || got[".env"].Selected || got["cert.txt"].Selected || got["extra.txt"].Selected {
		t.Fatalf("incorrect defaults: %+v", files)
	}
	if got[".env"].Warning == "" || got["cert.txt"].Warning == "" {
		t.Fatal("missing warning")
	}
	if _, ok := got["link"]; ok {
		t.Fatal("included symlink")
	}
	if _, ok := got["ignored"]; ok {
		t.Fatal("included ignored file")
	}
}

// TestListAcceptsTrailingSlash 验证用户输入仓库路径时允许结尾分隔符。
func TestListAcceptsTrailingSlash(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("readme"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "README.md")
	if _, err := List(context.Background(), root+string(os.PathSeparator)); err != nil {
		t.Fatalf("带结尾分隔符的仓库路径应可读取: %v", err)
	}
}

// TestListAcceptsRepositorySubdirectory 验证仓库内的项目子目录可以作为工作区。
func TestListAcceptsRepositorySubdirectory(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	subdir := filepath.Join(root, "service")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "main.go"), []byte("package service"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "service/main.go")
	files, err := List(context.Background(), subdir)
	if err != nil || len(files) != 1 || files[0].Path != "main.go" {
		t.Fatalf("仓库子目录读取失败: files=%+v err=%v", files, err)
	}
}
