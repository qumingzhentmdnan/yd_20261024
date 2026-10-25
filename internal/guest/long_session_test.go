package guest

import (
	"errors"
	"net"
	"testing"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/protocol"
)

// TestLongGetStateKeepsSequenceRejection 验证只读长会话不会耗尽历史计数，仍拒绝重用请求号。
func TestLongGetStateKeepsSequenceRejection(t *testing.T) {
	b := testBoot(t)
	p, done := testConnection(t, b)
	id, _ := protocol.NewUUID()
	nonce, _ := protocol.NewNonce()
	params, _ := protocol.Encode(protocol.HelloParams{VMRef: "vm", Challenge: nonce, SupportedVersions: []string{"0.1"}})
	r := protocol.Request{RequestID: 1, Epoch: "epoch", ConnectionID: id, Method: protocol.MethodHello, TimeoutMs: 1000, Params: params}
	if response := exchange(t, p, r); response.Error != nil {
		t.Fatal(response.Error)
	}
	r.Method = protocol.MethodGetState
	r.Params = []byte(`{}`)
	for range protocol.MaxRequests + 8 {
		r.RequestID++
		if response := exchange(t, p, r); response.Error != nil {
			t.Fatal(response.Error)
		}
	}
	raw, err := protocol.EncodeRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = guestwire.Write(p, protocol.Control, protocol.HostToGuest, protocol.Frame{Kind: protocol.RequestKind, Payload: raw}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, protocol.ErrFrame) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("重复请求号未使连接失败")
	}
}

// TestGetStateAtHistoricalCapacity 验证Guest历史满额仍接受查询，而非只读请求仍触发原容量边界。
func TestGetStateAtHistoricalCapacity(t *testing.T) {
	b := testBoot(t)
	host, remote := net.Pipe()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := newConnection(b, remote, listener)
	c.hello = true
	c.id, _ = protocol.NewUUID()
	c.epoch = "epoch"
	c.count = protocol.MaxRequests
	done := make(chan error, 1)
	go func() { err := c.run(); c.tasks.Wait(); done <- err }()
	t.Cleanup(func() { host.Close(); c.close() })
	r := protocol.Request{RequestID: 1, Epoch: c.epoch, ConnectionID: c.id, Method: protocol.MethodGetState, TimeoutMs: 1000, Params: []byte(`{}`)}
	if response := exchange(t, host, r); response.Error != nil {
		t.Fatal(response.Error)
	}
	c.mu.Lock()
	count := c.count
	c.mu.Unlock()
	if count != protocol.MaxRequests {
		t.Fatal("查询消耗历史容量", count)
	}
	r.RequestID++
	r.Method = protocol.MethodStopSession
	r.Params, _ = protocol.Encode(protocol.StopSessionParams{SessionID: "session", GraceMs: 1})
	raw, err := protocol.EncodeRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = guestwire.Write(host, protocol.Control, protocol.HostToGuest, protocol.Frame{Kind: protocol.RequestKind, Payload: raw}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, protocol.ErrFrame) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("非只读容量限制被移除")
	}
}
