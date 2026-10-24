package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"safeagent/internal/workspacebundle"
	"strings"
	"sync"
	"testing"
)

// testLimits 给测试提供显式且较小的资源边界。
func testLimits() Limits {
	return Limits{MaxFiles: 16, MaxFileBytes: 4096, MaxTotalBytes: 8192, MaxPathBytes: 256, MaxArchiveBytes: 32768}
}

// testStore 创建独立的 Linux 工作区测试存储。
func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("安全复制尚未验证当前平台")
	}
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "store"), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	return store, source
}

// writeTestFile 写入测试源内容，失败时立即结束场景。
func writeTestFile(t *testing.T, root, relative string, content []byte, mode os.FileMode) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, content, mode); err != nil {
		t.Fatal(err)
	}
}

// readImport 读取并关闭独立导入句柄。
func readImport(t *testing.T, s *Store, id BaselineID) []byte {
	t.Helper()
	r, err := s.OpenImport(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	closeErr := r.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	return data
}

// TestStageSnapshot 验证中文、二进制、权限和源目录变化不会改变已发布包。
func TestStageSnapshot(t *testing.T) {
	s, root := testStore(t)
	writeTestFile(t, root, "子目录/中文.txt", []byte{0, 1, 255}, 0644)
	writeTestFile(t, root, "run.sh", []byte("echo hello\n"), 0755)
	selection := ImportSelection{Root: root, Paths: []string{"子目录/中文.txt", "run.sh"}}
	b, err := s.Stage(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	original := readImport(t, s, b.ID)
	m, err := ValidateBundle(context.Background(), bytes.NewReader(original), testLimits())
	if err != nil || len(m.Entries) != 2 || !m.Entries[0].Executable || m.Entries[1].Executable {
		t.Fatalf("清单错误: %+v, %v", m, err)
	}
	r := tar.NewReader(bytes.NewReader(original))
	files := make(map[string][]byte)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name], err = io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(files["files/子目录/中文.txt"], []byte{0, 1, 255}) {
		t.Fatal("归档文件内容错误")
	}
	second, err := s.Stage(context.Background(), selection)
	if err != nil || second.ID == b.ID || second.Digest != b.Digest || !bytes.Equal(original, readImport(t, s, second.ID)) {
		t.Fatalf("归档不确定: %+v %v", second, err)
	}
	writeTestFile(t, root, "run.sh", []byte("changed"), 0755)
	if !bytes.Equal(original, readImport(t, s, b.ID)) || bytes.Contains(original, []byte(root)) {
		t.Fatal("源修改影响基线或归档泄露源路径")
	}
	reopened, err := NewStore(s.dataDir, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Lookup(context.Background(), b.ID)
	if err != nil || got != b {
		t.Fatalf("恢复基线失败: %+v %v", got, err)
	}
}

// TestEmptyAndGolden 验证空工作区包的固定字节摘要。
func TestEmptyAndGolden(t *testing.T) {
	s, root := testStore(t)
	b, err := s.Stage(context.Background(), ImportSelection{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	data := readImport(t, s, b.ID)
	sum := sha256.Sum256(data)
	const expected = "2b084dd5f8c4fcc0c74154dba3239485b3b18c57a77580421f0535802c03c906"
	if hex.EncodeToString(sum[:]) != expected {
		t.Fatalf("黄金字节摘要: %x", sum)
	}
}

// TestSelectionBoundaries 验证不可信文件路径及精确容量边界。
func TestSelectionBoundaries(t *testing.T) {
	for _, paths := range [][]string{{"../x"}, {"/x"}, {"a//b"}, {"a\\b"}, {".git/config"}, {"a", "A"}, {"a", "a/b"}, {"A", "a/b"}, {"NUL"}, {"a."}, {"a/../b"}, {"a\x00b"}} {
		if _, err := workspacebundle.ValidateSelection(paths, testLimits()); !errors.Is(err, ErrInvalidSelection) {
			t.Fatalf("未拒绝路径 %q: %v", paths, err)
		}
	}
	s, root := testStore(t)
	writeTestFile(t, root, "a", make([]byte, 4096), 0644)
	writeTestFile(t, root, "b", make([]byte, 4096), 0644)
	if _, err := s.Stage(context.Background(), ImportSelection{Root: root, Paths: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "c", []byte{1}, 0644)
	if _, err := s.Stage(context.Background(), ImportSelection{Root: root, Paths: []string{"a", "b", "c"}}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("未拒绝总容量超限: %v", err)
	}
	writeTestFile(t, root, "large", make([]byte, 4097), 0644)
	if _, err := s.Stage(context.Background(), ImportSelection{Root: root, Paths: []string{"large"}}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("未拒绝单文件超限: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "staging"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("失败后存在临时文件: %v %v", entries, err)
	}
}

// TestLinksAndErrorPrivacy 验证文件与目录链接拒绝和源错误脱敏。
func TestLinksAndErrorPrivacy(t *testing.T) {
	s, root := testStore(t)
	writeTestFile(t, root, "dir/file", []byte("data"), 0644)
	for name, target := range map[string]string{"link": "dir/file", "dirlink": "dir"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(root, "dir/file"), filepath.Join(root, "hard")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"link", "dirlink/file", "hard", "dir/file"} {
		if _, err := s.Stage(context.Background(), ImportSelection{Root: root, Paths: []string{p}}); err == nil || strings.Contains(err.Error(), root) {
			t.Fatalf("链接未拒绝或泄露路径: %s %v", p, err)
		}
	}
	_, err := s.Stage(context.Background(), ImportSelection{Root: root, Paths: []string{"missing"}})
	if !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), root) {
		t.Fatalf("源错误语义错误: %v", err)
	}
	_, err = s.Stage(context.Background(), ImportSelection{Root: filepath.Join(root, "dirlink"), Paths: []string{"file"}})
	if err == nil {
		t.Fatal("根目录链接未拒绝")
	}
}

// TestCorruption 验证归档内容、扩展头、截断、填充与尾部数据均受检查。
func TestCorruption(t *testing.T) {
	s, root := testStore(t)
	writeTestFile(t, root, "中文.txt", []byte("payload"), 0644)
	b, err := s.Stage(context.Background(), ImportSelection{Root: root, Paths: []string{"中文.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	original := readImport(t, s, b.ID)
	sum := sha256.Sum256(original)
	const expected = "7df39bf7173c79c69a716b9d72dd75d45165bbad4c23825f0d574786f37e17cf"
	if hex.EncodeToString(sum[:]) != expected {
		t.Fatalf("PAX 黄金字节摘要: %x", sum)
	}
	for _, index := range []int{0, 512, 1024, 1536, 2048, len(original) - 1} {
		bad := bytes.Clone(original)
		bad[index] ^= 1
		if _, err := ValidateBundle(context.Background(), bytes.NewReader(bad), testLimits()); err == nil {
			t.Fatalf("未检查位置 %d", index)
		}
	}
	for _, bad := range [][]byte{original[:len(original)-1], append(bytes.Clone(original), 0)} {
		if _, err := ValidateBundle(context.Background(), bytes.NewReader(bad), testLimits()); err == nil {
			t.Fatal("接受截断或额外尾部")
		}
	}
	path := filepath.Join(s.dataDir, "baselines", string(b.ID), "bundle.tar")
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenImport(context.Background(), b.ID); !errors.Is(err, ErrCorruptBaseline) {
		t.Fatalf("损坏归档未报告: %v", err)
	}
}

// gatedContext 在文件首次读取前用屏障暂停，确定性触发源变更或取消。
type gatedContext struct {
	// Context 提供真实上下文的取消信号。
	context.Context
	// calls 仅由复制所在协程访问，定位 Stage 预检后的首次读取。
	calls int
	// ready 通知测试源元数据已读取。
	ready chan struct{}
	// resume 允许测试修改源文件后继续复制。
	resume chan struct{}
}

// Err 在第二次检查上下文时等待测试屏障。
func (g *gatedContext) Err() error {
	g.calls++
	if g.calls == 2 {
		close(g.ready)
		<-g.resume
	}
	return g.Context.Err()
}

// TestSourceChangeAndMidCopyCancel 用确定性屏障验证取消和源文件并发修改。
func TestSourceChangeAndMidCopyCancel(t *testing.T) {
	for name, cancelCopy := range map[string]bool{"源内容变化": false, "复制中取消": true} {
		t.Run(name, func(t *testing.T) {
			s, root := testStore(t)
			writeTestFile(t, root, "file", []byte("old"), 0644)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gate := &gatedContext{Context: ctx, ready: make(chan struct{}), resume: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				_, err := s.Stage(gate, ImportSelection{Root: root, Paths: []string{"file"}})
				done <- err
			}()
			<-gate.ready
			if cancelCopy {
				cancel()
			} else {
				writeTestFile(t, root, "file", []byte("new content"), 0644)
			}
			close(gate.resume)
			want := ErrSourceChanged
			if cancelCopy {
				want = context.Canceled
			}
			if err := <-done; !errors.Is(err, want) {
				t.Fatalf("预期 %v，得到 %v", want, err)
			}
			for _, name := range []string{"staging", "baselines"} {
				entries, err := os.ReadDir(filepath.Join(s.dataDir, name))
				if err != nil || len(entries) != 0 {
					t.Fatalf("失败后残留 %s: %v %v", name, entries, err)
				}
			}
		})
	}
}

// TestArchiveLimit 验证完整包限额包含头部、填充及结束块。
func TestArchiveLimit(t *testing.T) {
	s, root := testStore(t)
	b, err := s.Stage(context.Background(), ImportSelection{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int64{0, -1} {
		limits := testLimits()
		limits.MaxArchiveBytes = b.ArchiveBytes + delta
		store, err := NewStore(filepath.Join(t.TempDir(), "store"), limits)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Stage(context.Background(), ImportSelection{Root: root})
		if delta == 0 && err != nil || delta < 0 && !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("归档精确边界 %d: %v", delta, err)
		}
	}
}

// TestConcurrentReadersAndCancellation 验证多个读者互不影响及取消后停止读取。
func TestConcurrentReadersAndCancellation(t *testing.T) {
	s, root := testStore(t)
	writeTestFile(t, root, "a", []byte("content"), 0644)
	b, err := s.Stage(context.Background(), ImportSelection{Root: root, Paths: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	expected := readImport(t, s, b.ID)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r, err := s.OpenImport(context.Background(), b.ID)
			if err != nil {
				t.Error(err)
				return
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, expected) {
				t.Errorf("并发读取错误: %v", err)
			}
		})
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	r, err := s.OpenImport(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cancel()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("读取未响应取消: %v", err)
	}
	if _, err := s.Stage(ctx, ImportSelection{Root: root}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stage 未响应取消: %v", err)
	}
}
