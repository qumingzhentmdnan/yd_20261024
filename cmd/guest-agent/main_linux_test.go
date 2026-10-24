package main

import (
	"testing"
)

// TestSameBootRestartRejected 验证锁释放后同boot标记仍阻止重建空状态。
func TestSameBootRestartRejected(t *testing.T) {
	root := t.TempDir()
	first, err := acquireBootAt(root)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := acquireBootAt(root)
	if err == nil {
		second.Close()
		t.Fatal("同boot重启被错误允许")
	}
}
