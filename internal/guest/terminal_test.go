package guest

import (
	"context"
	"net"
	"testing"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/protocol"
)

// TestOldBindTokenRejected 验证格式正确但旧token不能绑定当前授权，失败只释放attachment。
func TestOldBindTokenRejected(t *testing.T) {
	b := testBoot(t)
	id, _ := protocol.NewUUID()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &connection{boot: b, ctx: ctx, cancel: cancel, id: id, epoch: "epoch", streams: make(map[protocol.StreamID]bool), released: make(map[protocol.StreamID]protocol.OpaqueID)}
	s := &session{info: protocol.SessionInfo{SessionID: "session"}, changed: make(chan struct{})}
	b.sessions["session"] = s
	_, _, err := c.attach(protocol.AttachTerminalParams{SessionID: "session", StreamID: 1})
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	defer right.Close()
	done := make(chan struct{})
	go func() { c.serveTerminal(left); close(done) }()
	old, _ := protocol.NewNonce()
	raw, _ := protocol.Encode(protocol.Bind{SessionID: "session", Epoch: "epoch", ConnectionID: id, BindToken: old})
	if err = guestwire.Write(right, protocol.Terminal, protocol.HostToGuest, protocol.Frame{Kind: protocol.BindKind, StreamID: 1, Payload: raw}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("旧token未结束attachment")
	}
	b.mu.Lock()
	a := c.attachment
	b.mu.Unlock()
	if a != nil || ctx.Err() != nil {
		t.Fatal("旧token失败边界错误")
	}
}

// TestBindTimeoutReleasesAuthorization 验证完整响应后的绑定预算到期会回收待绑定授权。
func TestBindTimeoutReleasesAuthorization(t *testing.T) {
	b := testBoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &connection{boot: b, ctx: ctx, cancel: cancel, streams: make(map[protocol.StreamID]bool), released: make(map[protocol.StreamID]protocol.OpaqueID)}
	b.sessions["session"] = &session{info: protocol.SessionInfo{SessionID: "session"}, changed: make(chan struct{})}
	_, sent, err := c.attach(protocol.AttachTerminalParams{SessionID: "session", StreamID: 1})
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	a := c.attachment
	b.mu.Unlock()
	sent()
	select {
	case <-a.released:
	case <-time.After(guestwire.Budget + time.Second):
		t.Fatal("Bind超时未回收")
	}
	c.tasks.Wait()
}
