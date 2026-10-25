package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestFIFO 验证打开 FIFO 不会在类型检查之前阻塞。
func TestFIFO(t *testing.T) {
	s, root := testStore(t)
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage(context.Background(), ImportSelection{Root: root, Paths: []string{"pipe"}}); err == nil {
		t.Fatal("FIFO 未拒绝")
	}
}

// TestConcurrentStage 验证并发快照分别发布且互不清理对方资源。
func TestConcurrentStage(t *testing.T) {
	s, root := testStore(t)
	writeTestFile(t, root, "file", []byte("data"), 0644)
	results := make(chan Baseline, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			b, err := s.Stage(context.Background(), ImportSelection{Root: root, Paths: []string{"file"}})
			if err != nil {
				t.Error(err)
				return
			}
			results <- b
		})
	}
	wg.Wait()
	close(results)
	ids := make(map[BaselineID]bool)
	var digest string
	for b := range results {
		if ids[b.ID] || digest != "" && digest != b.Digest {
			t.Fatal("并发快照身份重复或内容不一致")
		}
		ids[b.ID], digest = true, b.Digest
	}
	if len(ids) != 4 {
		t.Fatal("并发快照未全部发布")
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "staging"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("存在临时资源: %v %v", entries, err)
	}
}

// TestStorageErrorClassification 验证真实读取故障不会被误分类或泄露绝对路径。
func TestStorageErrorClassification(t *testing.T) {
	pathErr := &os.PathError{Op: "read", Path: "/private/store/baselines/b_secret/bundle.tar", Err: syscall.EIO}
	got := classifyBundleError(pathErr)
	if !errors.Is(got, syscall.EIO) || errors.Is(got, ErrCorruptBaseline) || strings.Contains(got.Error(), "/private") {
		t.Fatalf("存储错误分类或脱敏错误: %v", got)
	}
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, ErrCorruptBaseline, ErrLimitExceeded, ErrInvalidSelection} {
		if !errors.Is(classifyBundleError(err), ErrCorruptBaseline) {
			t.Fatalf("格式或截断错误未分类: %v", err)
		}
	}
	joined := safeError("复制文件", "file", errors.Join(pathErr, &os.PathError{Op: "close", Path: "/private/other", Err: syscall.ENOSPC}))
	if !errors.Is(joined, syscall.EIO) || !errors.Is(joined, syscall.ENOSPC) || strings.Contains(joined.Error(), "/private") {
		t.Fatalf("多错误脱敏丢失原因: %v", joined)
	}
}

// TestLongSourceRoot 验证 Host 元数据不错误占用空导入包的精确限额。
func TestLongSourceRoot(t *testing.T) {
	s, root := testStore(t)
	for range 48 {
		root = filepath.Join(root, strings.Repeat("x", 40))
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	s.limits.MaxArchiveBytes = 2048
	b, err := s.Stage(context.Background(), ImportSelection{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	if len(readImport(t, s, b.ID)) != 2048 {
		t.Fatal("空包大小不符合约定")
	}
}
