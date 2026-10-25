package rpc

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
)

// ledgerClient 创建可逐帧控制取消和迟到响应的真实客户端。
func ledgerClient(t *testing.T) (*Client, net.Conn) {
	t.Helper()
	left, right := net.Pipe()
	c, err := New(vm.VMInstance{Ref: vm.VMRef{ID: "ledger"}, Epoch: "epoch"}, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { right.Close(); c.Close() })
	return c, right
}

// readLedgerRequest 读取并验证客户端发送的请求帧。
func readLedgerRequest(t *testing.T, p net.Conn) protocol.Request {
	t.Helper()
	p.SetReadDeadline(time.Now().Add(time.Second))
	f, err := guestwire.Read(p, protocol.Control, protocol.HostToGuest)
	if err != nil {
		t.Fatal(err)
	}
	r, err := protocol.DecodeRequest(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// waitReading 等待唯一读循环完成响应验证，避免把写入完成误当作账本回收完成。
func waitReading(t *testing.T, c *Client, want int) {
	t.Helper()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		c.mu.Lock()
		got := c.reading
		c.mu.Unlock()
		if got == want {
			return
		}
		select {
		case <-timeout.C:
			t.Fatalf("只读占额=%d，期望%d", got, want)
		case <-tick.C:
		}
	}
}

// TestCanceledGetStateCapacity 验证取消占额、满额拒绝、迟到错误响应回收及关闭释放。
func TestCanceledGetStateCapacity(t *testing.T) {
	c, p := ledgerClient(t)
	var first protocol.Request
	for i := 0; i < protocol.MaxPending; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := c.GetState(ctx); done <- err }()
		r := readLedgerRequest(t, p)
		if i == 0 {
			first = r
		}
		cancel()
		f, err := guestwire.Read(p, protocol.Control, protocol.HostToGuest)
		if err != nil || f.Kind != protocol.CancelKind {
			t.Fatal(f, err)
		}
		if err = <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	_, err := c.GetState(context.Background())
	var business *protocol.Error
	if !errors.As(err, &business) || business.Code != protocol.CodeResourceExhausted {
		t.Fatal(err)
	}
	c.mu.Lock()
	next := c.next
	size := len(c.requests)
	c.mu.Unlock()
	if next != protocol.MaxPending || size != protocol.MaxPending {
		t.Fatal("满额分配序号或丢失登记", next, size)
	}
	data, err := protocol.EncodeResponse(protocol.Response{RequestID: first.RequestID, Epoch: first.Epoch, ConnectionID: first.ConnectionID, Error: protocol.Failure(protocol.CodeCancelled)}, first)
	if err != nil {
		t.Fatal(err)
	}
	if err = guestwire.Write(p, protocol.Control, protocol.GuestToHost, protocol.Frame{Kind: protocol.ResponseKind, Payload: data}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	waitReading(t, c, protocol.MaxPending-1)
	// 错误响应也完成方法关联验证，释放一个槽位后可以登记新请求。
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.GetState(ctx); done <- err }()
	r := readLedgerRequest(t, p)
	if r.RequestID != next+1 {
		t.Fatal(r.RequestID)
	}
	cancel()
	if _, err = guestwire.Read(p, protocol.Control, protocol.HostToGuest); err != nil {
		t.Fatal(err)
	}
	<-done
	c.Close()
	waitReading(t, c, 0)
}

// TestGetStateSequenceExhaustion 验证序号耗尽仍明确拒绝且不复用零号。
func TestGetStateSequenceExhaustion(t *testing.T) {
	c, _ := ledgerClient(t)
	c.next = ^protocol.Sequence(0)
	_, err := c.GetState(context.Background())
	var business *protocol.Error
	if !errors.As(err, &business) || business.Code != protocol.CodeResourceExhausted {
		t.Fatal(err)
	}
}

// TestActiveGetStateCapacityRejectsImmediately 验证活动调用占满slots时第十七个查询也立即返回容量错误。
func TestActiveGetStateCapacityRejectsImmediately(t *testing.T) {
	c, p := ledgerClient(t)
	done := make(chan error, protocol.MaxPending)
	for range protocol.MaxPending {
		go func() { _, err := c.GetState(context.Background()); done <- err }()
		readLedgerRequest(t, p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.GetState(ctx)
	var business *protocol.Error
	if !errors.As(err, &business) || business.Code != protocol.CodeResourceExhausted {
		t.Fatal("满额应立即拒绝而非等待调用资格", err)
	}
	c.Close()
	for range protocol.MaxPending {
		<-done
	}
	waitReading(t, c, 0)
}

// TestUnsentGetStateReleasesReservation 验证等待调用资格时取消会回收尚未发送的只读预留。
func TestUnsentGetStateReleasesReservation(t *testing.T) {
	c, _ := ledgerClient(t)
	for range protocol.MaxPending {
		c.slots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.GetState(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	waitReading(t, c, 0)
	c.mu.Lock()
	size, next := len(c.requests), c.next
	c.mu.Unlock()
	if size != 0 || next != 0 {
		t.Fatal("未发送请求留下登记或序号", size, next)
	}
}
