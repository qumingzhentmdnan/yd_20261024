package guest

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/protocol"
)

// testBoot 构造只用于 wire 状态验证的显式登记，不启动进程。
func testBoot(t *testing.T) *Boot {
	t.Helper()
	const d = protocol.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")
	b, err := New(Config{Content: protocol.Preparation{PreparedID: "registered", ManifestRef: "fixed", ManifestDigest: d, ConfigDigest: d, WorkspaceRef: "workspace", WorkspaceDigest: d}, Build: "test", ImageDigest: d})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testConnection 接真实 Handler 与控制 socketpair，保留原始 Host 写端。
func testConnection(t *testing.T, b *Boot) (net.Conn, <-chan error) {
	t.Helper()
	host, remote := net.Pipe()
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "terminal"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Serve(context.Background(), remote, listener) }()
	t.Cleanup(func() {
		host.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		b.Close(ctx)
	})
	return host, done
}

// exchange 用原始 envelope 发送精确请求并读取关联响应。
func exchange(t *testing.T, p net.Conn, r protocol.Request) protocol.Response {
	t.Helper()
	data, err := protocol.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = guestwire.Write(p, protocol.Control, protocol.HostToGuest, protocol.Frame{Kind: protocol.RequestKind, Payload: data}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	frame, err := guestwire.Read(p, protocol.Control, protocol.GuestToHost)
	if err != nil {
		t.Fatal(err)
	}
	response, err := protocol.DecodeResponse(frame.Payload, r)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// TestHandshakeIdentityAndParamsPriority 验证未握手优先、合法身份坏参数返回业务错误、旧身份直接断线。
func TestHandshakeIdentityAndParamsPriority(t *testing.T) {
	b := testBoot(t)
	p, done := testConnection(t, b)
	id, _ := protocol.NewUUID()
	r := protocol.Request{RequestID: 1, Epoch: "epoch", ConnectionID: id, Method: protocol.MethodGetState, TimeoutMs: 1000, Params: []byte(`{"unknown":1}`)}
	response := exchange(t, p, r)
	if response.Error == nil || response.Error.Code != protocol.CodeFailedPrecondition {
		t.Fatal(response)
	}
	nonce, _ := protocol.NewNonce()
	params, _ := protocol.Encode(protocol.HelloParams{VMRef: "vm", Challenge: nonce, SupportedVersions: []string{"0.1"}})
	r.RequestID++
	r.Method = protocol.MethodHello
	r.Params = params
	response = exchange(t, p, r)
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	r.RequestID++
	r.Method = protocol.MethodGetState
	r.Params = []byte(`{"unknown":1}`)
	response = exchange(t, p, r)
	if response.Error == nil || response.Error.Code != protocol.CodeInvalidArgument {
		t.Fatal(response)
	}
	r.RequestID++
	r.Epoch = "old"
	data, _ := protocol.Encode(r)
	if err := guestwire.Write(p, protocol.Control, protocol.HostToGuest, protocol.Frame{Kind: protocol.RequestKind, Payload: data}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, protocol.ErrFrame) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("旧身份没有失效连接")
	}
}

// TestRequestSequenceIndependentOfCount 验证合法的高起始序号不会被误当作请求数量超限。
func TestRequestSequenceIndependentOfCount(t *testing.T) {
	b := testBoot(t)
	p, _ := testConnection(t, b)
	id, _ := protocol.NewUUID()
	nonce, _ := protocol.NewNonce()
	params, _ := protocol.Encode(protocol.HelloParams{VMRef: "vm", Challenge: nonce, SupportedVersions: []string{"0.1"}})
	r := protocol.Request{RequestID: 5000, Epoch: "epoch", ConnectionID: id, Method: protocol.MethodHello, TimeoutMs: 1000, Params: params}
	response := exchange(t, p, r)
	if response.Error != nil {
		t.Fatal(response.Error)
	}
}

// TestResizeLockCancellation 验证输入占用PTY时Resize及时响应取消，不阻塞控制槽位。
func TestResizeLockCancellation(t *testing.T) {
	b := testBoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{info: protocol.SessionInfo{SessionID: "session"}, ioMu: make(chan struct{}, 1)}
	s.ioMu <- struct{}{}
	a := &attachment{session: s, stream: 1, state: protocol.AttachmentStateBound, ctx: ctx}
	c := &connection{boot: b, attachment: a}
	request, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	_, _, err := c.execute(request, protocol.MethodResizeTerminal, &protocol.ResizeTerminalParams{SessionID: "session", StreamID: 1, Rows: 24, Cols: 80})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	<-s.ioMu
}

// TestResizeRechecksReleasedAttachment 验证等待资格的旧Resize不能操作新授权。
func TestResizeRechecksReleasedAttachment(t *testing.T) {
	b := testBoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{info: protocol.SessionInfo{SessionID: "session"}, ioMu: make(chan struct{}, 1)}
	s.ioMu <- struct{}{}
	a := &attachment{session: s, stream: 1, state: protocol.AttachmentStateBound, ctx: ctx}
	c := &connection{boot: b, attachment: a}
	done := make(chan error, 1)
	go func() {
		_, _, err := c.execute(context.Background(), protocol.MethodResizeTerminal, &protocol.ResizeTerminalParams{SessionID: "session", StreamID: 1, Rows: 24, Cols: 80})
		done <- err
	}()
	b.mu.Lock()
	c.attachment = &attachment{session: s, stream: 2}
	b.mu.Unlock()
	<-s.ioMu
	err := <-done
	if err == nil {
		t.Fatal("旧Resize不应成功")
	}
}
