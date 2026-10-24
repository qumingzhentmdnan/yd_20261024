package rpc

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
)

// localTerminal 构造完成绑定的独立终端，用真实字节通道测试 Fin 取消。
func localTerminal(t *testing.T) (*Terminal, net.Conn) {
	t.Helper()
	left, right := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{ctx: ctx, cancel: cancel}
	term := &Terminal{client: c, session: "session", stream: 1, port: left, changed: make(chan struct{}), write: make(chan struct{}, 1), finDone: make(chan struct{}), readerDone: make(chan struct{}), bound: true}
	go term.readLoop()
	t.Cleanup(func() { cancel(); term.abort(guest.ErrDisconnected); right.Close(); <-term.readerDone })
	return term, right
}

// TestCloseWriteCancellationKeepsOutput 验证 Fin 未写出任何字节的取消不会停止输出 Read。
func TestCloseWriteCancellationKeepsOutput(t *testing.T) {
	term, peer := localTerminal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := term.CloseWrite(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		sent <- guestwire.Write(peer, protocol.Terminal, protocol.GuestToHost, protocol.Frame{Kind: protocol.DataKind, StreamID: 1, Payload: []byte("tail")}, time.Now().Add(time.Second))
	}()
	data := make([]byte, 4)
	if _, err := io.ReadFull(term, data); err != nil || string(data) != "tail" {
		t.Fatal(string(data), err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}

// TestUnboundTerminalDoesNotDrain 验证初始化期间退出事件不会启动等待不存在 reader 的清理任务。
func TestUnboundTerminalDoesNotDrain(t *testing.T) {
	term := &Terminal{readerDone: make(chan struct{})}
	term.drain()
	started := false
	term.drainOnce.Do(func() { started = true })
	if !started {
		t.Fatal("未绑定终端错误启动了排空任务")
	}
}

// TestCloseTotalBudget 验证对端不读取控制Cancel时Close不会再增加独立发送预算。
func TestCloseTotalBudget(t *testing.T) {
	control, peer := net.Pipe()
	defer peer.Close()
	id, _ := protocol.NewUUID()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{instance: vm.VMInstance{Ref: vm.VMRef{ID: "test"}, Epoch: "epoch"}, id: id, control: control, ctx: ctx, cancel: cancel, write: make(chan struct{}, 1), slots: make(chan struct{}, 16), requests: make(map[protocol.Sequence]*pending)}
	port, remote := net.Pipe()
	defer remote.Close()
	done := make(chan struct{})
	close(done)
	term := &Terminal{client: c, session: "session", stream: 1, port: port, changed: make(chan struct{}), readerDone: done}
	go func() { guestwire.Read(peer, protocol.Control, protocol.HostToGuest) }()
	start := time.Now()
	if err := term.Close(); err == nil {
		t.Fatal("未确认Detach不应成功")
	}
	if elapsed := time.Since(start); elapsed > guestwire.Budget+300*time.Millisecond {
		t.Fatalf("Close超出总预算: %s", elapsed)
	}
}

// gatedDeadlineConn 在初始写预算设置期间提供可控交错，验证取消不会被初始预算覆盖。
type gatedDeadlineConn struct {
	// Conn 是实际内存字节通道。
	net.Conn
	// entered 通知初始deadline设置已经进入。
	entered chan struct{}
	// release 由测试允许初始设置结束。
	release chan struct{}
	// once 只暂停初始设置。
	once sync.Once
}

// SetWriteDeadline 暂停第一次设置，后续取消截止时间正常传给底层。
func (c *gatedDeadlineConn) SetWriteDeadline(d time.Time) error {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Conn.SetWriteDeadline(d)
}

// TestCloseWriteCancellationDuringInitialDeadline 验证初始deadline设置期间取消能立即打断Fin写。
func TestCloseWriteCancellationDuringInitialDeadline(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	wrapped := &gatedDeadlineConn{Conn: left, entered: make(chan struct{}), release: make(chan struct{})}
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	term := &Terminal{client: &Client{ctx: base}, port: wrapped, stream: 1, write: make(chan struct{}, 1), finDone: make(chan struct{}), changed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- term.CloseWrite(ctx) }()
	<-wrapped.entered
	cancel()
	close(wrapped.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("初始deadline覆盖取消")
	}
}

// TestPartialFinAbortsAttachment 验证Fin头部分写入后失败属于不可恢复半帧，不能继续读取为成功。
func TestPartialFinAbortsAttachment(t *testing.T) {
	term, peer := localTerminal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	received := make(chan error, 1)
	go func() { var first [1]byte; _, err := io.ReadFull(peer, first[:]); received <- err }()
	if err := term.CloseWrite(ctx); err == nil {
		t.Fatal("部分Fin不应成功")
	}
	if err := <-received; err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if _, err := term.Read(data[:]); !errors.Is(err, guest.ErrOutputIncomplete) {
		t.Fatal(err)
	}
}
