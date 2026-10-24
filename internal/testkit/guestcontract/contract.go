// Package guestcontract 提供 Fake 与后续真实 GuestClient 共用的外部行为测试。
package guestcontract

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"safeagent/internal/host/guest"
	"safeagent/internal/protocol"
)

// Factory 为每个场景提供未 Hello 的独立 client 和已登记内容的合法启动参数。
// StartSessionParams.PreparedID 由 suite 使用 PrepareAgent 的实际结果填充。
// 真实工厂负责在 Cleanup 中用独立 context 回收进程和 VM，不能依赖已关闭的 client。
type Factory func(*testing.T) (guest.Client, protocol.PrepareAgentParams, protocol.StartSessionParams)

// fixture 为每个场景保存独立连接与总时限。
type fixture struct {
	// client 是唯一外部控制入口。
	client guest.Client
	// prepare 指向测试环境登记的固定内容。
	prepare protocol.PrepareAgentParams
	// start 在准备完成后绑定实际 preparedID。
	start protocol.StartSessionParams
	// ctx 将阻塞测试限制为十五秒。
	ctx context.Context
}

// newFixture 初始化未握手连接，注册可唤醒阻塞调用的清理路径。
func newFixture(t *testing.T, factory Factory) fixture {
	t.Helper()
	c, p, s := factory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return fixture{client: c, prepare: p, start: s, ctx: ctx}
}

// ready 完成 Hello 与准备，不启动工作负载。
func ready(t *testing.T, f *fixture) {
	t.Helper()
	result, err := f.client.Hello(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.Validate(result); err != nil {
		t.Fatal(err)
	}
	prepared, err := f.client.PrepareAgent(f.ctx, f.prepare)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.Validate(prepared); err != nil {
		t.Fatal(err)
	}
	f.start.PreparedID = prepared.PreparedID
}

// start 创建唯一工作负载并检查公共结果。
func start(t *testing.T, f fixture) protocol.SessionInfo {
	t.Helper()
	info, err := f.client.StartSession(f.ctx, f.start)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.Validate(info); err != nil {
		t.Fatal(err)
	}
	return info
}

// requireCode 检查稳定业务错误，不把传输错误混作业务结果。
func requireCode(t *testing.T, err error, code protocol.Code) {
	t.Helper()
	var e *protocol.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// stop 以显式允许强停的短宽限期结束组。
func stop(t *testing.T, f fixture) {
	t.Helper()
	if err := f.client.StopSession(f.ctx, protocol.StopSessionParams{SessionID: f.start.SessionID, GraceMs: 100, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
}

// Run 验证公共接口的生命周期，不限定具体进程实现或终端设备。
func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("HandshakeAndPreCancelledStart", func(t *testing.T) {
		f := newFixture(t, factory)
		_, err := f.client.GetState(f.ctx)
		requireCode(t, err, protocol.CodeFailedPrecondition)
		ready(t, &f)
		_, err = f.client.Hello(f.ctx)
		requireCode(t, err, protocol.CodeFailedPrecondition)
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		if _, err := f.client.StartSession(ctx, f.start); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Start: %v", err)
		}
		state, err := f.client.GetState(f.ctx)
		if err != nil || len(state.Sessions) != 0 {
			t.Fatalf("cancelled Start changed state: %+v %v", state, err)
		}
	})
	t.Run("ConcurrentStartConflictAndStableReplay", func(t *testing.T) {
		f := newFixture(t, factory)
		ready(t, &f)
		begin := make(chan struct{})
		results := make(chan protocol.SessionInfo, 16)
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				<-begin
				info, err := f.client.StartSession(f.ctx, f.start)
				if err != nil {
					t.Error(err)
					return
				}
				results <- info
			})
		}
		close(begin)
		wg.Wait()
		close(results)
		count := 0
		for info := range results {
			if info.SessionID != f.start.SessionID {
				t.Error(info)
			}
			count++
		}
		if count != 16 {
			t.Fatalf("only %d starts", count)
		}
		changed := f.start
		changed.PreparedID = "valid-but-not-found"
		_, err := f.client.StartSession(f.ctx, changed)
		requireCode(t, err, protocol.CodeConflict)
		state, err := f.client.GetState(f.ctx)
		if err != nil || len(state.Sessions) != 1 {
			t.Fatalf("duplicate process: %+v %v", state, err)
		}
		stop(t, f)
		stop(t, f)
		replay, err := f.client.StartSession(f.ctx, f.start)
		if err != nil || replay.SessionID != f.start.SessionID {
			t.Fatalf("replay: %+v %v", replay, err)
		}
		state, err = f.client.GetState(f.ctx)
		if err != nil || len(state.Sessions) != 1 || state.Sessions[0].Exit == nil {
			t.Fatalf("lost exit: %+v %v", state, err)
		}
	})
	t.Run("WaitCancellationAndImmutableExit", func(t *testing.T) {
		f := newFixture(t, factory)
		ready(t, &f)
		start(t, f)
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		if _, err := f.client.WaitSession(ctx, f.start.SessionID); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		results := make(chan protocol.SessionExit, 16)
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				exit, err := f.client.WaitSession(f.ctx, f.start.SessionID)
				if err != nil {
					t.Error(err)
					return
				}
				results <- exit
			})
		}
		stop(t, f)
		wg.Wait()
		close(results)
		var first protocol.SessionExit
		count := 0
		for exit := range results {
			if count == 0 {
				first = exit
			}
			if exit != first {
				t.Fatal("waiters disagree")
			}
			count++
		}
		if count != 16 {
			t.Fatalf("only %d waiters", count)
		}
		state, err := f.client.GetState(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		state.Sessions[0].Exit.Code = 123
		again, err := f.client.WaitSession(f.ctx, f.start.SessionID)
		if err != nil || again != first {
			t.Fatal("mutable exit snapshot")
		}
	})
	t.Run("TerminalDetachHalfCloseAndStop", func(t *testing.T) {
		f := newFixture(t, factory)
		ready(t, &f)
		start(t, f)
		terminal, err := f.client.AttachTerminal(f.ctx, f.start.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { terminal.Close() })
		_, err = f.client.AttachTerminal(f.ctx, f.start.SessionID)
		requireCode(t, err, protocol.CodeFailedPrecondition)
		if err := terminal.Resize(f.ctx, 40, 120); err != nil {
			t.Fatal(err)
		}
		if err := terminal.CloseWrite(f.ctx); err != nil {
			t.Fatal(err)
		}
		cancelled, cancel := context.WithCancel(f.ctx)
		cancel()
		if err := terminal.CloseWrite(cancelled); err != nil {
			t.Fatalf("重复 CloseWrite 改写了成功结果: %v", err)
		}
		if _, err := terminal.Write([]byte("after-fin")); !errors.Is(err, guest.ErrInputClosed) {
			t.Fatalf("Write after Fin: %v", err)
		}
		if err := terminal.Close(); err != nil {
			t.Fatal(err)
		}
		terminal2, err := f.client.AttachTerminal(f.ctx, f.start.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { terminal2.Close() })
		state, err := f.client.GetState(f.ctx)
		if err != nil || state.Sessions[0].Exit != nil {
			t.Fatalf("detach stopped process: %+v %v", state, err)
		}
		stop(t, f)
		read := make(chan error, 1)
		go func() { _, err := io.Copy(io.Discard, terminal2); read <- err }()
		select {
		case err := <-read:
			if err != nil {
				t.Fatal(err)
			}
		case <-f.ctx.Done():
			terminal2.Close()
			<-read
			t.Fatal("terminal missing Fin")
		}
	})
	t.Run("DisconnectWakesWaiters", func(t *testing.T) {
		f := newFixture(t, factory)
		ready(t, &f)
		start(t, f)
		result := make(chan error, 1)
		go func() { _, err := f.client.WaitSession(f.ctx, f.start.SessionID); result <- err }()
		if err := f.client.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			if !errors.Is(err, guest.ErrDisconnected) {
				t.Fatal(err)
			}
		case <-f.ctx.Done():
			t.Fatal("disconnect did not wake waiter")
		}
	})
	t.Run("CloseWriteFirstCancellationIsStable", func(t *testing.T) {
		f := newFixture(t, factory)
		ready(t, &f)
		start(t, f)
		terminal, err := f.client.AttachTerminal(f.ctx, f.start.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { terminal.Close() })
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		if err := terminal.CloseWrite(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				if err := terminal.CloseWrite(f.ctx); !errors.Is(err, context.Canceled) {
					t.Errorf("首次取消结果被改写: %v", err)
				}
			})
		}
		wg.Wait()
		stop(t, f)
	})
}
