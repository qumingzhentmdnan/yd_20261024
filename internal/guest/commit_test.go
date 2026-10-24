package guest

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"safeagent/internal/guest/runner"
	"safeagent/internal/protocol"
)

// commitBoot 以真实持续进程及显式准备结果构造提交点测试。
func commitBoot(t *testing.T) (*Boot, *connection, protocol.StartSessionParams) {
	t.Helper()
	b := testBoot(t)
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if uid == 0 {
		uid = 1000
		gid = 1000
	}
	b.config.Runner = runner.Config{Path: "/bin/sh", Args: []string{"-c", "trap 'exit 0' TERM; while :; do sleep 1; done"}, Dir: "/tmp", UID: uid, GID: gid}
	prepared := b.config.Content
	b.prepared = &prepared
	b.preparations[prepared.PreparedID] = prepared
	id, _ := protocol.NewUUID()
	p := protocol.StartSessionParams{OperationID: id, SessionID: "session", PreparedID: prepared.PreparedID, WorkspaceRef: prepared.WorkspaceRef, WorkspaceDigest: prepared.WorkspaceDigest, Rows: 24, Cols: 80}
	c := &connection{boot: b}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := b.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return b, c, p
}

// TestCancellationBeforeCreation 固定预留后的取消失败，同一操作重试不得创建进程。
func TestCancellationBeforeCreation(t *testing.T) {
	b, c, p := commitBoot(t)
	entered := make(chan struct{})
	b.beforeCreate = func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.operate(ctx, protocol.MethodStartSession, p); done <- err }()
	<-entered
	cancel()
	if err := <-done; err == nil {
		t.Fatal("取消未失败")
	}
	b.beforeCreate = nil
	_, err := c.operate(context.Background(), protocol.MethodStartSession, p)
	var e *protocol.Error
	if !errors.As(err, &e) || e.Code != protocol.CodeCancelled {
		t.Fatal(err)
	}
	if len(b.sessions) != 0 {
		t.Fatal("取消后创建了进程")
	}
}

// TestCreatedProcessCannotRollback 验证真实创建后即使取消，仍提交唯一 session 且重试返回原结果。
func TestCreatedProcessCannotRollback(t *testing.T) {
	b, c, p := commitBoot(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	b.afterCreate = func() { close(entered); <-release }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan any, 1)
	go func() {
		v, err := c.operate(ctx, protocol.MethodStartSession, p)
		if err != nil {
			done <- err
		} else {
			done <- v
		}
	}()
	<-entered
	cancel()
	other := p
	other.OperationID, _ = protocol.NewUUID()
	other.SessionID = "other"
	_, err := c.operate(context.Background(), protocol.MethodStartSession, other)
	var business *protocol.Error
	if !errors.As(err, &business) || business.Code != protocol.CodeFailedPrecondition {
		t.Fatal(err)
	}
	close(release)
	value := <-done
	info, ok := value.(protocol.SessionInfo)
	if !ok {
		t.Fatal(value)
	}
	b.afterCreate = nil
	replay, err := c.operate(context.Background(), protocol.MethodStartSession, p)
	if err != nil || replay.(protocol.SessionInfo).SessionID != info.SessionID || len(b.sessions) != 1 {
		t.Fatal(replay, err)
	}
}

// TestStopWaitsForExitCommit 验证Runner退出已知但Boot提交被暂停时Stop不能提前成功。
func TestStopWaitsForExitCommit(t *testing.T) {
	b, c, p := commitBoot(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	b.beforeExitCommit = func() { close(entered); <-release }
	if _, err := c.operate(context.Background(), protocol.MethodStartSession, p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := c.execute(ctx, protocol.MethodStopSession, &protocol.StopSessionParams{SessionID: p.SessionID, GraceMs: 20, ForceAfterTimeout: true})
		done <- err
	}()
	<-entered
	select {
	case err := <-done:
		t.Fatal("Stop早于退出提交", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	exit := b.sessions[p.SessionID].info.Exit
	b.mu.Unlock()
	if exit == nil {
		t.Fatal("Stop成功但未提交exit")
	}
}
