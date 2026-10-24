package rpc_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
)

// digest 为显式登记的测试内容计算摘要。
func digest(data []byte) protocol.Digest {
	return protocol.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(data)))
}

// wireClient 创建可直接注入 wire 故障的对端，不模拟高层 Client 行为。
func wireClient(t *testing.T) (*rpc.Client, net.Conn) {
	t.Helper()
	host, remote := net.Pipe()
	c, err := rpc.New(vm.VMInstance{Ref: vm.VMRef{ID: "wire"}, Epoch: "epoch"}, host, func(context.Context) (net.Conn, error) { return nil, errors.New("unused") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remote.Close(); c.Close() })
	return c, remote
}

// receiveRequest 用共享解码器读取真实控制帧。
func receiveRequest(t *testing.T, p net.Conn) protocol.Request {
	t.Helper()
	f, err := guestwire.Read(p, protocol.Control, protocol.HostToGuest)
	if err != nil {
		t.Error(err)
		return protocol.Request{}
	}
	r, err := protocol.DecodeRequest(f.Payload)
	if err != nil {
		t.Error(err)
	}
	return r
}

// sendResult 用共享方法关联编码器生成响应。
func sendResult(t *testing.T, p net.Conn, r protocol.Request, result any) {
	t.Helper()
	raw, err := protocol.Encode(result)
	if err != nil {
		t.Error(err)
		return
	}
	data, err := protocol.EncodeResponse(protocol.Response{RequestID: r.RequestID, Epoch: r.Epoch, ConnectionID: r.ConnectionID, Result: raw}, r)
	if err != nil {
		t.Error(err)
		return
	}
	if err = guestwire.Write(p, protocol.Control, protocol.GuestToHost, protocol.Frame{Kind: protocol.ResponseKind, Payload: data}, time.Now().Add(time.Second)); err != nil {
		t.Error(err)
	}
}

// TestMismatchedChallenge 验证格式合法但不属于本次握手的 challenge 使连接失效。
func TestMismatchedChallenge(t *testing.T) {
	c, p := wireClient(t)
	done := make(chan error, 1)
	go func() { _, err := c.Hello(context.Background()); done <- err }()
	r := receiveRequest(t, p)
	nonce, err := protocol.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	sendResult(t, p, r, protocol.HelloResult{Version: "0.1", Challenge: nonce, GuestBuild: "test", ImageDigest: digest([]byte("image"))})
	if err = <-done; !errors.Is(err, guest.ErrDisconnected) {
		t.Fatal(err)
	}
}

// TestLateAndDuplicateResponse 验证取消后首个合法迟到响应丢弃，重复响应失效整个连接。
func TestLateAndDuplicateResponse(t *testing.T) {
	c, p := wireClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.GetState(ctx); done <- err }()
	r := receiveRequest(t, p)
	cancel()
	f, err := guestwire.Read(p, protocol.Control, protocol.HostToGuest)
	if err != nil || f.Kind != protocol.CancelKind {
		t.Fatal(f, err)
	}
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	state := protocol.GuestState{Sessions: []protocol.SessionInfo{}}
	sendResult(t, p, r, state)
	active := make(chan error, 1)
	go func() { _, err := c.GetState(context.Background()); active <- err }()
	next := receiveRequest(t, p)
	if next.RequestID <= r.RequestID {
		t.Fatal(next)
	}
	sendResult(t, p, r, state)
	select {
	case err = <-active:
		if !errors.Is(err, guest.ErrDisconnected) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("重复响应没有唤醒请求")
	}
}

// TestUnknownResponse 验证从未发送过的响应不能命中合法请求。
func TestUnknownResponse(t *testing.T) {
	c, p := wireClient(t)
	done := make(chan error, 1)
	go func() { _, err := c.GetState(context.Background()); done <- err }()
	r := receiveRequest(t, p)
	r.RequestID++
	sendResult(t, p, r, protocol.GuestState{Sessions: []protocol.SessionInfo{}})
	if err := <-done; !errors.Is(err, guest.ErrDisconnected) {
		t.Fatal(err)
	}
}

// TestRequestLedgerCapacity 验证四千零九十六条记录不淘汰，达到上限后保持可关闭。
func TestRequestLedgerCapacity(t *testing.T) {
	c, p := wireClient(t)
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		for range protocol.MaxRequests {
			r := receiveRequest(t, p)
			sendResult(t, p, r, protocol.GuestState{Sessions: []protocol.SessionInfo{}})
		}
	}()
	for range protocol.MaxRequests {
		if _, err := c.GetState(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	<-peerDone
	_, err := c.GetState(context.Background())
	var e *protocol.Error
	if !errors.As(err, &e) || e.Code != protocol.CodeResourceExhausted {
		t.Fatal(err)
	}
}

// TestEnvelopeBeforeParams 验证公共解码器能够分离 envelope 与合法身份下的坏方法参数。
func TestEnvelopeBeforeParams(t *testing.T) {
	id, err := protocol.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	r := protocol.Request{RequestID: 1, Epoch: "epoch", ConnectionID: id, Method: protocol.MethodGetState, TimeoutMs: 1000, Params: json.RawMessage(`{"unknown":1}`)}
	raw, err := protocol.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := protocol.DecodeRequestEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = envelope.DecodeParams(); err == nil {
		t.Fatal("坏方法参数未被拒绝")
	}
	if _, err = protocol.DecodeRequest(raw); err == nil {
		t.Fatal("严格 API 放行坏方法参数")
	}
}
