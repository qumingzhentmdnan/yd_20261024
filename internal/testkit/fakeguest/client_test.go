package fakeguest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"safeagent/internal/host/guest"
	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
	"safeagent/internal/testkit/fakeguest"
	"safeagent/internal/testkit/guestcontract"
)

// uuid 返回不同测试操作的合法确定性 UUID，不用于生产随机标识。
func uuid(n int) protocol.UUID {
	return protocol.UUID(fmt.Sprintf("%08x-1111-4111-8111-111111111111", n))
}

// config 显式提供测试身份、镜像摘要与已登记内容。
func config() fakeguest.Config {
	digest := protocol.Digest("sha256:" + strings.Repeat("e", 64))
	return fakeguest.Config{Instance: vm.VMInstance{Ref: vm.VMRef{ID: "guest-contract-vm"}, Epoch: "boot-1"}, GuestBuild: "fake-build", ImageDigest: protocol.Digest("sha256:" + strings.Repeat("d", 64)), Content: protocol.Preparation{PlanDigest: &digest, PreparedID: "prepared-1", ManifestRef: "fixed-agent", ManifestDigest: protocol.Digest("sha256:" + strings.Repeat("a", 64)), ConfigDigest: protocol.Digest("sha256:" + strings.Repeat("b", 64)), WorkspaceRef: "workspace-1", WorkspaceDigest: protocol.Digest("sha256:" + strings.Repeat("c", 64))}}
}

// setup 创建独立未握手连接及合法准备、启动参数。
func setup(t *testing.T) (*fakeguest.Boot, *fakeguest.Client, protocol.PrepareAgentParams, protocol.StartSessionParams) {
	t.Helper()
	cfg := config()
	boot, err := fakeguest.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client, err := boot.Connect(cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	p := protocol.PrepareAgentParams{OperationID: uuid(1), ManifestRef: cfg.Content.ManifestRef, ManifestDigest: cfg.Content.ManifestDigest, ConfigDigest: cfg.Content.ConfigDigest, WorkspaceRef: cfg.Content.WorkspaceRef, WorkspaceDigest: cfg.Content.WorkspaceDigest}
	s := protocol.StartSessionParams{OperationID: uuid(2), SessionID: "session-1", PreparedID: cfg.Content.PreparedID, WorkspaceRef: cfg.Content.WorkspaceRef, WorkspaceDigest: cfg.Content.WorkspaceDigest, Rows: 24, Cols: 80}
	return boot, client, p, s
}

// ready 完成固定内容的握手准备。
func ready(t *testing.T, c *fakeguest.Client, p protocol.PrepareAgentParams) {
	t.Helper()
	if _, err := c.Hello(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PrepareAgent(t.Context(), p); err != nil {
		t.Fatal(err)
	}
}

// requireCode 检查业务错误及其稳定错误码。
func requireCode(t *testing.T, err error, code protocol.Code) {
	t.Helper()
	var e *protocol.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// TestSharedContract 运行可供后续真实 Client 使用的全部公共行为用例。
func TestSharedContract(t *testing.T) {
	guestcontract.Run(t, func(t *testing.T) (guest.Client, protocol.PrepareAgentParams, protocol.StartSessionParams) {
		_, c, p, s := setup(t)
		return c, p, s
	})
}

// TestAcceptedFailureAndLostResponse 验证提交前取消固定失败与提交后丢响应不重复创建。
func TestAcceptedFailureAndLostResponse(t *testing.T) {
	b, c, p, s := setup(t)
	ready(t, c, p)
	before := fakeguest.NewGate()
	if err := b.Inject(fakeguest.StartBeforeCommit, fakeguest.Fault{Gate: before}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { _, err := c.StartSession(ctx, s); result <- err }()
	<-before.Entered()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, err := c.StartSession(t.Context(), s)
	requireCode(t, err, protocol.CodeCancelled)
	state, err := c.GetState(t.Context())
	if err != nil || len(state.Sessions) != 0 {
		t.Fatal(state, err)
	}
	after := fakeguest.NewGate()
	if err := b.Inject(fakeguest.StartAfterCommit, fakeguest.Fault{Gate: after}); err != nil {
		t.Fatal(err)
	}
	s.OperationID = uuid(3)
	ctx, cancel = context.WithCancel(t.Context())
	go func() { _, err := c.StartSession(ctx, s); result <- err }()
	<-after.Entered()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := b.Connect(config().Instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Close() })
	if _, err := next.Hello(t.Context()); err != nil {
		t.Fatal(err)
	}
	replay, err := next.StartSession(t.Context(), s)
	if err != nil || replay.SessionID != s.SessionID {
		t.Fatal(replay, err)
	}
	state, err = next.GetState(t.Context())
	if err != nil || len(state.Sessions) != 1 {
		t.Fatal(state, err)
	}
	changed := s
	changed.PreparedID = "missing-reference"
	_, err = next.StartSession(t.Context(), changed)
	requireCode(t, err, protocol.CodeConflict)
	wrong := config().Instance
	wrong.Epoch = "old"
	if _, err := b.Connect(wrong); err == nil {
		t.Fatal("旧epoch未拒绝")
	}
}

// TestRingBufferFinAndHalfClose 验证原始字节、丢弃计数、尾部排空与真正的输入半关闭语义。
func TestRingBufferFinAndHalfClose(t *testing.T) {
	b, c, p, s := setup(t)
	ready(t, c, p)
	if _, err := c.StartSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	output := bytes.Repeat([]byte{'x'}, 2*protocol.OutputLimit)
	if n, err := b.FeedOutput(t.Context(), s.SessionID, output); err != nil || n != len(output) {
		t.Fatal(n, err)
	}
	dropped, err := b.DroppedBytes(s.SessionID)
	if err != nil || dropped != protocol.Counter(protocol.OutputLimit) {
		t.Fatal(dropped, err)
	}
	terminal, err := c.AttachTerminal(t.Context(), s.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminal.Close() })
	input := []byte{'a', 0, 0x1b, '[', '3', '1', 'm', 3, 4}
	if n, err := terminal.Write(input); err != nil || n != len(input) {
		t.Fatal(n, err)
	}
	got, err := b.TakeInput(s.SessionID)
	if err != nil || !bytes.Equal(got, input) {
		t.Fatal(got, err)
	}
	if err := terminal.CloseWrite(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err = b.TakeInput(s.SessionID)
	if err != nil || len(got) != 0 {
		t.Fatal("CloseWrite 注入了PTY字符", got, err)
	}
	if err := terminal.Resize(t.Context(), 40, 120); err != nil {
		t.Fatal(err)
	}
	rows, cols, err := b.Window(s.SessionID)
	if err != nil || rows != 40 || cols != 120 {
		t.Fatal(rows, cols, err)
	}
	// 模拟主进程先退出，尾部字节稍后才到达，只有 EndOutput 才产生 EOF。
	if err := b.Exit(protocol.SessionExit{SessionID: s.SessionID, Code: 7, Reason: protocol.ExitReasonProcessExited}, false); err != nil {
		t.Fatal(err)
	}
	prefix := make([]byte, protocol.OutputLimit)
	for total := 0; total < len(prefix); {
		n, err := terminal.Read(prefix[total:])
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if !bytes.Equal(prefix, output[len(output)-len(prefix):]) {
		t.Fatal("环形缓冲内容错误")
	}
	if _, err := b.FeedOutput(t.Context(), s.SessionID, []byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := b.EndOutput(s.SessionID); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(terminal)
	if err != nil || string(tail) != "tail" {
		t.Fatal(string(tail), err)
	}
}

// TestSlowOutputDoesNotBlockStop 验证满输出缓冲不会阻止控制停止且尾部不会被清空。
func TestSlowOutputDoesNotBlockStop(t *testing.T) {
	b, c, p, s := setup(t)
	ready(t, c, p)
	if _, err := c.StartSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	terminal, err := c.AttachTerminal(t.Context(), s.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminal.Close() })
	data := bytes.Repeat([]byte{'z'}, protocol.OutputLimit)
	if _, err := b.FeedOutput(t.Context(), s.SessionID, data); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := b.FeedOutput(t.Context(), s.SessionID, []byte("blocked")); done <- err }()
	if err := c.StopSession(t.Context(), protocol.StopSessionParams{SessionID: s.SessionID, GraceMs: 1, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(terminal)
	if err != nil || !bytes.Equal(tail, data) {
		t.Fatal(len(tail), err)
	}
}

// TestGroupAndForce 保留主进程退出结果，同时要求残留组成员真正清空。
func TestGroupAndForce(t *testing.T) {
	b, c, p, s := setup(t)
	ready(t, c, p)
	if _, err := c.StartSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	exit := protocol.SessionExit{SessionID: s.SessionID, Code: 7, Reason: protocol.ExitReasonProcessExited}
	if err := b.Exit(exit, true); err != nil {
		t.Fatal(err)
	}
	if err := b.IgnoreTERM(s.SessionID, true); err != nil {
		t.Fatal(err)
	}
	opts := protocol.StopSessionParams{SessionID: s.SessionID, GraceMs: 1, ForceAfterTimeout: false}
	requireCode(t, c.StopSession(t.Context(), opts), protocol.CodeDeadlineExceeded)
	next := s
	next.OperationID = uuid(3)
	next.SessionID = "session-next"
	_, err := c.StartSession(t.Context(), next)
	requireCode(t, err, protocol.CodeFailedPrecondition)
	opts.ForceAfterTimeout = true
	if err := c.StopSession(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	got, err := c.WaitSession(t.Context(), s.SessionID)
	if err != nil || got != exit {
		t.Fatal(got, err)
	}
	if _, err := c.StartSession(t.Context(), next); err != nil {
		t.Fatal(err)
	}
}

// TestCapacityAndReplay 覆盖两种 boot 容量边界，满时旧请求仍可返回固定结果。
func TestCapacityAndReplay(t *testing.T) {
	t.Run("Operations", func(t *testing.T) {
		_, c, p, _ := setup(t)
		if _, err := c.Hello(t.Context()); err != nil {
			t.Fatal(err)
		}
		first := p
		for i := 1; i <= protocol.MaxOperations; i++ {
			p.OperationID = uuid(i)
			if _, err := c.PrepareAgent(t.Context(), p); err != nil {
				t.Fatal(i, err)
			}
		}
		p.OperationID = uuid(protocol.MaxOperations + 1)
		_, err := c.PrepareAgent(t.Context(), p)
		requireCode(t, err, protocol.CodeResourceExhausted)
		if _, err := c.PrepareAgent(t.Context(), first); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("Sessions", func(t *testing.T) {
		b, c, p, s := setup(t)
		ready(t, c, p)
		first := s
		for i := 0; i < protocol.MaxSessions; i++ {
			s.OperationID = uuid(i + 2)
			s.SessionID = protocol.OpaqueID(fmt.Sprintf("session-%02d", i))
			if i == 0 {
				first = s
			}
			if _, err := c.StartSession(t.Context(), s); err != nil {
				t.Fatal(i, err)
			}
			if err := b.Exit(protocol.SessionExit{SessionID: s.SessionID, Code: 0, Reason: protocol.ExitReasonProcessExited}, false); err != nil {
				t.Fatal(err)
			}
		}
		s.OperationID = uuid(100)
		s.SessionID = "session-over-limit"
		_, err := c.StartSession(t.Context(), s)
		requireCode(t, err, protocol.CodeResourceExhausted)
		if _, err := c.StartSession(t.Context(), first); err != nil {
			t.Fatal(err)
		}
	})
}

// TestDistinctStartCommitRace 验证不同操作在提交点仍只能创建一个活动进程组。
func TestDistinctStartCommitRace(t *testing.T) {
	b, c, p, s := setup(t)
	ready(t, c, p)
	gate := fakeguest.NewGate()
	if err := b.Inject(fakeguest.StartBeforeCommit, fakeguest.Fault{Gate: gate}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { _, err := c.StartSession(ctx, s); result <- err }()
	<-gate.Entered()
	next := s
	next.OperationID, next.SessionID = uuid(3), "other-session"
	if _, err := c.StartSession(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	gate.Release()
	requireCode(t, <-result, protocol.CodeFailedPrecondition)
	if err := c.StopSession(t.Context(), protocol.StopSessionParams{SessionID: next.SessionID, GraceMs: 1, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	// 预留后的失败已固定，后续前置状态改变也不能变为成功。
	_, err := c.StartSession(t.Context(), s)
	requireCode(t, err, protocol.CodeFailedPrecondition)
	state, err := c.GetState(t.Context())
	if err != nil || len(state.Sessions) != 1 {
		t.Fatal(state, err)
	}
}

// TestDisconnectBeforeCommit 验证连接失效取消已接纳但未提交的创建，重连返回固定失败。
func TestDisconnectBeforeCommit(t *testing.T) {
	b, c, p, s := setup(t)
	ready(t, c, p)
	gate := fakeguest.NewGate()
	if err := b.Inject(fakeguest.StartBeforeCommit, fakeguest.Fault{Gate: gate}); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := c.StartSession(t.Context(), s); result <- err }()
	<-gate.Entered()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, guest.ErrDisconnected) {
		t.Fatal(err)
	}
	next, err := b.Connect(config().Instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Close() })
	if _, err := next.Hello(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = next.StartSession(t.Context(), s)
	requireCode(t, err, protocol.CodeCancelled)
	state, err := next.GetState(t.Context())
	if err != nil || len(state.Sessions) != 0 {
		t.Fatal(state, err)
	}
}

// TestPreparationInputIsolation 验证构造参数的可选摘要不与内部登记共享。
func TestPreparationInputIsolation(t *testing.T) {
	cfg := config()
	expected := *cfg.Content.PlanDigest
	boot, err := fakeguest.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	*cfg.Content.PlanDigest = cfg.Content.ConfigDigest
	client, err := boot.Connect(cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Hello(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := client.PrepareAgent(t.Context(), protocol.PrepareAgentParams{OperationID: uuid(200), ManifestRef: cfg.Content.ManifestRef, ManifestDigest: cfg.Content.ManifestDigest, ConfigDigest: cfg.Content.ConfigDigest, WorkspaceRef: cfg.Content.WorkspaceRef, WorkspaceDigest: cfg.Content.WorkspaceDigest})
	if err != nil {
		t.Fatal(err)
	}
	if result.PlanDigest == nil || *result.PlanDigest != expected {
		t.Fatal("构造参数污染内部登记")
	}
}
