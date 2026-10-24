//go:build linux

package rpc_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"safeagent/internal/host/guest"
	"safeagent/internal/protocol"
)

// readyClient 完成真实服务端握手和固定内容准备。
func readyClient(t *testing.T, c guest.Client, p protocol.PrepareAgentParams, s protocol.StartSessionParams) protocol.StartSessionParams {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	prepared, err := c.PrepareAgent(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	s.PreparedID = prepared.PreparedID
	return s
}

// requireBusinessCode 断言远端稳定业务错误。
func requireBusinessCode(t *testing.T, err error, code protocol.Code) {
	t.Helper()
	var e *protocol.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// TestOperationCapacity 验证操作容量满时原操作重试和查询仍可执行。
func TestOperationCapacity(t *testing.T) {
	c, p, s := newClient(t)
	readyClient(t, c, p, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 1; i < protocol.MaxOperations; i++ {
		id, err := protocol.NewUUID()
		if err != nil {
			t.Fatal(err)
		}
		p.OperationID = id
		if _, err = c.PrepareAgent(ctx, p); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := c.PrepareAgent(ctx, p); err != nil {
		t.Fatal("容量满时重试", err)
	}
	p.OperationID, _ = protocol.NewUUID()
	_, err := c.PrepareAgent(ctx, p)
	requireBusinessCode(t, err, protocol.CodeResourceExhausted)
	if _, err = c.GetState(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestSlowOutputStopAndIncomplete 验证慢消费者不阻塞控制 Stop，排空超时返回独立错误。
func TestSlowOutputStopAndIncomplete(t *testing.T) {
	c, p, s := workloadClient(t, []byte("#!/bin/sh\ntrap 'exit 0' TERM\nwhile :; do printf '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'; done\n"))
	s = readyClient(t, c, p, s)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := c.StartSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	terminal, err := c.AttachTerminal(ctx, s.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if err = c.StopSession(ctx, protocol.StopSessionParams{SessionID: s.SessionID, GraceMs: 100, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.WaitSession(ctx, s.SessionID); err != nil {
		t.Fatal(err)
	}
	// 不消费待交付帧直到独立排空预算结束，然后检查已缓存字节后的明确不完整结果。
	timer := time.NewTimer(2200 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	_, err = io.Copy(io.Discard, terminal)
	if !errors.Is(err, guest.ErrOutputIncomplete) {
		t.Fatalf("want incomplete output, got %v", err)
	}
	if _, err = c.GetState(ctx); err != nil {
		t.Fatal("控制面应保持可用", err)
	}
}

// TestStableStartFailure 验证真实进程创建失败被操作表固定，参数冲突优先于资源查询。
func TestStableStartFailure(t *testing.T) {
	c, p, s := workloadClient(t, []byte("#!/does-not-exist\n"))
	s = readyClient(t, c, p, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.StartSession(ctx, s)
	requireBusinessCode(t, err, protocol.CodeInternal)
	_, err = c.StartSession(ctx, s)
	requireBusinessCode(t, err, protocol.CodeInternal)
	s.PreparedID = "missing"
	_, err = c.StartSession(ctx, s)
	requireBusinessCode(t, err, protocol.CodeConflict)
	state, err := c.GetState(ctx)
	if err != nil || len(state.Sessions) != 0 {
		t.Fatal(state, err)
	}
}

// TestRemainingGroupAfterLeaderExit 验证主进程退出后残留组仍阻止新 Start，授权 KILL 后才能复用资源。
func TestRemainingGroupAfterLeaderExit(t *testing.T) {
	workload := []byte("#!/bin/sh\n( trap '' TERM HUP; printf 'CHILD_READY\\n'; while :; do sleep 1; done ) &\n# 等待子进程先安装信号策略，再让 leader 自行退出。\nsleep 0.1\nexit 7\n")
	c, p, s := workloadClient(t, workload)
	s = readyClient(t, c, p, s)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := c.StartSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	exit, err := c.WaitSession(ctx, s.SessionID)
	if err != nil || exit.Code != 7 {
		t.Fatal(exit, err)
	}
	other := s
	other.OperationID, _ = protocol.NewUUID()
	other.SessionID = "other"
	_, err = c.StartSession(ctx, other)
	requireBusinessCode(t, err, protocol.CodeFailedPrecondition)
	err = c.StopSession(ctx, protocol.StopSessionParams{SessionID: s.SessionID, GraceMs: 30, ForceAfterTimeout: false})
	requireBusinessCode(t, err, protocol.CodeDeadlineExceeded)
	if err = c.StopSession(ctx, protocol.StopSessionParams{SessionID: s.SessionID, GraceMs: 30, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	again, err := c.WaitSession(ctx, s.SessionID)
	if err != nil || again != exit {
		t.Fatal("退出快照不应被组强停改写", again, err)
	}
	if err = c.StopSession(ctx, protocol.StopSessionParams{SessionID: s.SessionID, GraceMs: 30, ForceAfterTimeout: true}); err != nil {
		t.Fatal("历史 Stop", err)
	}
}
