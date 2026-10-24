package runner

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"safeagent/internal/protocol"
)

// testProcess 启动明确忽略 TERM 的固定非 root PTY 程序。
func testProcess(t *testing.T) *Process {
	t.Helper()
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if uid == 0 {
		uid = 1000
		gid = 1000
	}
	p, err := Start(Config{Path: "/bin/sh", Args: []string{"-c", "trap '' TERM; printf READY; while :; do sleep 1; done"}, Dir: "/tmp", UID: uid, GID: gid}, "session", 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, 5)
	if _, err = io.ReadFull(p, ready); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p.Stop(ctx, time.Millisecond, true)
		p.Close()
		p.Reap(ctx)
	})
	return p
}

// TestKillReasonCannotDowngrade 验证 KILL 后即使并发停止路径请求 TERM，固定退出仍为 forced。
func TestKillReasonCannotDowngrade(t *testing.T) {
	p := testProcess(t)
	if err := p.Signal(true); err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	exit, err := p.Wait(ctx)
	if err != nil || exit.Signal != "SIGKILL" || exit.Reason != protocol.ExitReasonForced {
		t.Fatal(exit, err)
	}
}

// TestCancelledWriteThenNewInput 验证取消回调完全结束后，后续输入不会继承过期 deadline。
func TestCancelledWriteThenNewInput(t *testing.T) {
	p := testProcess(t)
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p.Write(ctx, []byte("old"))
		ctx2, stop := context.WithTimeout(context.Background(), time.Second)
		_, err := p.Write(ctx2, []byte("new"))
		stop()
		if err != nil {
			t.Fatal(err)
		}
	}
}
