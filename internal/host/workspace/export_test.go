package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"safeagent/internal/protocol"
	"sort"
	"testing"
)

// exportFixture 模拟Guest返回的冻结文件，不能代表真实VM验收。
type exportFixture struct {
	// files 记录Guest文件内容。
	files map[string][]byte
	// corrupt 控制哈希不一致负例。
	corrupt bool
}

// ListChanges 返回有序测试清单。
func (f exportFixture) ListChanges(ctx context.Context, p protocol.ListChangesParams) (protocol.ChangePage, error) {
	entries := []Entry{}
	for path, data := range f.files {
		entries = append(entries, Entry{Path: path, Size: int64(len(data)), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data))})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return protocol.ChangePage{Entries: entries}, nil
}

// ReadChangeChunk 模拟有界读取及内容被替换。
func (f exportFixture) ReadChangeChunk(ctx context.Context, p protocol.ReadChangeChunkParams) (protocol.ChangeChunk, error) {
	data := f.files[p.Path]
	if f.corrupt {
		return protocol.ChangeChunk{Data: []byte("bad")}, nil
	}
	return protocol.ChangeChunk{Data: data[p.Offset:min(int64(len(data)), p.Offset+protocol.ExportChunkBytes)]}, nil
}

// exportStore 创建有一个基线文件的真实Host文件系统夹具。
func exportStore(t *testing.T) (*Store, Baseline, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "private"), Limits{MaxFiles: 100, MaxFileBytes: 1 << 20, MaxTotalBytes: 2 << 20, MaxPathBytes: 1024, MaxArchiveBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := store.Stage(context.Background(), ImportSelection{Root: root, Paths: []string{"file"}})
	if err != nil {
		t.Fatal(err)
	}
	return store, baseline, root
}

// TestReviewRestoreApprove 验证冻结、重启恢复、批准ID及新增修改删除。
func TestReviewRestoreApprove(t *testing.T) {
	for _, files := range []map[string][]byte{{"file": []byte("new"), "dir/new": []byte("added")}, {}} {
		store, baseline, root := exportStore(t)
		review, err := store.CollectReview(context.Background(), baseline.ID, exportFixture{files: files})
		if err != nil {
			t.Fatal(err)
		}
		if err = review.Apply(context.Background(), "wrong"); err == nil {
			t.Fatal("未批准也写回")
		}
		restored, err := store.LoadReview(context.Background(), review.ID())
		if err != nil {
			t.Fatal(err)
		}
		changes := restored.Changes()
		changes[0].Path = "../escape"
		if err = restored.Apply(context.Background(), restored.ID()); err != nil {
			t.Fatal(err)
		}
		for p, data := range files {
			actual, err := os.ReadFile(filepath.Join(root, p))
			if err != nil || string(actual) != string(data) {
				t.Fatalf("写回%s: %q %v", p, actual, err)
			}
		}
		if len(files) == 0 {
			if _, err = os.Stat(filepath.Join(root, "file")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
	}
}

// TestReviewConflictAndLinks 验证宿主并发编辑与父目录链接不会被覆盖。
func TestReviewConflictAndLinks(t *testing.T) {
	for _, variant := range []string{"edit", "symlink", "new-conflict"} {
		t.Run(variant, func(t *testing.T) {
			store, baseline, root := exportStore(t)
			review, err := store.CollectReview(context.Background(), baseline.ID, exportFixture{files: map[string][]byte{"file": []byte("new"), "dir/new": []byte("added")}})
			if err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "edit":
				err = os.WriteFile(filepath.Join(root, "file"), []byte("host"), 0644)
			case "symlink":
				err = os.Symlink(t.TempDir(), filepath.Join(root, "dir"))
			case "new-conflict":
				err = os.Mkdir(filepath.Join(root, "dir"), 0755)
				if err == nil {
					err = os.WriteFile(filepath.Join(root, "dir/new"), []byte("host"), 0644)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = review.Apply(context.Background(), review.ID()); !errors.Is(err, ErrConflict) {
				t.Fatalf("应冲突: %v", err)
			}
			actual, _ := os.ReadFile(filepath.Join(root, "file"))
			expected := "old"
			if variant == "edit" {
				expected = "host"
			}
			if string(actual) != expected {
				t.Fatal("冲突导致部分覆盖")
			}
		})
	}
}

// TestReviewRejectsGuestData 验证不可信Guest的路径与内容哈希。
func TestReviewRejectsGuestData(t *testing.T) {
	for _, fixture := range []exportFixture{{files: map[string][]byte{"../escape": []byte("bad")}}, {files: map[string][]byte{"file": []byte("new")}, corrupt: true}} {
		store, baseline, _ := exportStore(t)
		if _, err := store.CollectReview(context.Background(), baseline.ID, fixture); err == nil {
			t.Fatal("接受了损坏Guest数据")
		}
	}
}

// TestReviewGeneratedExcluded 验证生成配置的修改和删除均不会回写宿主。
func TestReviewGeneratedExcluded(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(filepath.Join(t.TempDir(), "private"), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := store.Stage(context.Background(), ImportSelection{Root: root, Generated: map[string][]byte{".codex/config.toml": []byte("model='test'")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, files := range []map[string][]byte{{".codex/config.toml": []byte("model='changed'")}, {}} {
		review, err := store.CollectReview(context.Background(), baseline.ID, exportFixture{files: files})
		if err != nil {
			t.Fatal(err)
		}
		if len(review.Changes()) != 0 {
			t.Fatal("生成配置进入导出")
		}
	}
}
