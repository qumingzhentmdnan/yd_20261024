package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestStoreExclusiveWriter 验证第二个写者在取得副作用权限前被拒绝。
func TestStoreExclusiveWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, e := Open(path); e == nil {
		other.Close()
		t.Fatal("second writer accepted")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestStoreFixedExitAndDelete 验证固定退出不被覆盖及删除历史的事务语义。
func TestStoreFixedExitAndDelete(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Insert(ctx, Resource{ID: "one", Backend: "test", Config: []byte(`{}`), State: "created"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := s.BeginRun(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`{"Code":17}`, `{"Code":99}`} {
		if err = s.FinishRun(ctx, Run{ID: "one", Epoch: epoch, Exit: []byte(value)}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.GetRun(ctx, "one", epoch)
	if err != nil || string(r.Exit) != `{"Code":17}` {
		t.Fatalf("fixed result: %s %v", r.Exit, err)
	}
	if err = s.Delete(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetRun(ctx, "one", epoch); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
}

// TestStoreRejectsAliases 验证符号链接和硬链接入口不能绕过独占写者锁。
func TestStoreRejectsAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, kind := range []string{"symlink", "hardlink"} {
		alias := path + "-" + kind
		if kind == "symlink" {
			err = os.Symlink(path, alias)
		} else {
			err = os.Link(path, alias)
		}
		if err != nil {
			t.Fatal(err)
		}
		if other, e := Open(alias); e == nil {
			other.Close()
			t.Fatal("database alias accepted")
		}
		if err = os.Remove(alias); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStoreResourceBinding 验证同一数据库不可绑定另一套后端资源。
func TestStoreResourceBinding(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err = s.Bind(ctx, "qemu:/one"); err != nil {
		t.Fatal(err)
	}
	if err = s.Bind(ctx, "qemu:/two"); err == nil {
		t.Fatal("different resource root accepted")
	}
}
