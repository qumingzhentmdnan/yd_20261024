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
