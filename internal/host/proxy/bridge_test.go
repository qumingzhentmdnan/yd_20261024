package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"testing"
	"time"

	"safeagent/internal/guest/bridge"
	"safeagent/internal/host/gateway"
	"safeagent/internal/host/proxy"
	"safeagent/internal/host/vm"
	"safeagent/internal/proxywire"
	"safeagent/internal/testkit/fakecredential"
)

// newGateway 使用真实TLS服务和明确假凭证构造实际Gateway。
func newGateway(t *testing.T, upstream *httptest.Server) (*gateway.Gateway, gateway.ChannelIdentity, gateway.BindingID) {
	t.Helper()
	return gatewayWithIdle(t, upstream, 2*time.Second)
}

// gatewayWithIdle 让设备失联测试采用不同于Gateway空闲超时的明确探测窗口。
func gatewayWithIdle(t *testing.T, upstream *httptest.Server, idle time.Duration) (*gateway.Gateway, gateway.ChannelIdentity, gateway.BindingID) {
	t.Helper()
	return gatewayWithBudgets(t, upstream, idle, 10*time.Second)
}

// gatewayWithBudgets 显式区分适配层总时限与Gateway自身时限，避免超时测试假阳性。
func gatewayWithBudgets(t *testing.T, upstream *httptest.Server, idle, total time.Duration) (*gateway.Gateway, gateway.ChannelIdentity, gateway.BindingID) {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	g, err := gateway.New(gateway.Config{Services: []gateway.Service{{Ref: "model", Origin: upstream.URL, Paths: []string{"/v1/responses"}, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, CredentialRef: "fake", Auth: gateway.Bearer}}, MaxBindings: 2, MaxConcurrent: 1, MaxRequestBytes: 1 << 20, MaxResponseBytes: 8 << 20, MaxHeaderBytes: 16384, TotalTimeout: total, IdleTimeout: idle, RootCAs: roots}, fakecredential.New(map[string]string{"fake": "explicit-host-test-only"}), net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	identity := gateway.ChannelIdentity{Instance: vm.VMInstance{Ref: vm.VMRef{ID: "proxy"}, Epoch: "epoch"}, ConnectionID: "connection"}
	binding, err := g.Bind(context.Background(), identity, gateway.BindingSpec{ServiceRefs: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	return g, identity, binding
}

// localBridge 通过内存设备连接真实Host适配器和HTTP Bridge。
func localBridge(t *testing.T, g *gateway.Gateway, identity gateway.ChannelIdentity, binding gateway.BindingID) (string, *bridge.Bridge, <-chan error) {
	t.Helper()
	host, guest := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		if err := proxywire.Write(host, proxywire.Frame{Kind: proxywire.Ready}, time.Now().Add(time.Second)); err != nil {
			done <- err
			return
		}
		done <- proxy.Serve(ctx, host, g, identity, binding)
	}()
	b, err := bridge.New(ctx, guest, "model", func() { cancel(); host.Close() })
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); b.Serve(listener) }()
	t.Cleanup(func() {
		b.Close()
		cancel()
		host.Close()
		guest.Close()
		select {
		case <-serverDone:
		case <-time.After(3 * time.Second):
			t.Error("HTTP服务未退出")
		}
	})
	return "http://" + listener.Addr().String(), b, done
}

// TestBridgeStreamingCapacityAndAbort 验证首块、二进制、假认证隔离、并发拒绝及撤销异常。
func TestBridgeStreamingCapacityAndAbort(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	binary := []byte{0, 255, 10, 13, 128}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != "actual-body" || r.Header.Get("Authorization") != "Bearer explicit-host-test-only" || r.Header.Get("X-Api-Key") != "" {
			t.Error("实际数据或认证隔离失败", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Authorization", "must-not-leak")
		w.Write(binary)
		w.(http.Flusher).Flush()
		close(entered)
		select {
		case <-release:
			w.Write([]byte("tail"))
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	g, identity, binding := newGateway(t, upstream)
	address, _, done := localBridge(t, g, identity, binding)
	client := &http.Client{Timeout: 5 * time.Second}
	request, _ := http.NewRequest("POST", address+"/v1/responses", bytes.NewBufferString("actual-body"))
	request.Header.Set("Authorization", "guest-spoof")
	request.Header.Set("X-Api-Key", "guest-spoof")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len(binary))
	if _, err = io.ReadFull(response.Body, first); err != nil || !bytes.Equal(first, binary) {
		t.Fatal("首块/二进制", first, err)
	}
	<-entered
	if response.Header.Get("Authorization") != "" {
		t.Fatal("响应泄露认证")
	}
	second, err := client.Post(address+"/v1/responses", "application/json", bytes.NewBufferString("second"))
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != 503 {
		t.Fatal("第二请求未立即拒绝", second.StatusCode)
	}
	// 第二请求的body停滞，拒绝不能等待排空。
	partial, err := net.Dial("tcp", address[len("http://"):])
	if err != nil {
		t.Fatal(err)
	}
	partial.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(partial, "POST /v1/responses HTTP/1.1\r\nHost: loopback\r\nContent-Length: 100\r\n\r\nx")
	rejected, err := http.ReadResponse(bufio.NewReader(partial), nil)
	if err != nil {
		partial.Close()
		t.Fatal("并发停滞上传未有界拒绝", err)
	}
	if rejected.StatusCode != 503 {
		t.Fatal(rejected.StatusCode)
	}
	rejected.Body.Close()
	partial.Close()
	g.Revoke(context.Background(), binding)
	if _, err = io.ReadAll(response.Body); err == nil {
		t.Fatal("撤销被伪装成成功EOF")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("撤销后Host未结束")
	}
}

// TestBridgeNormalReuse 验证只有正常End后才能复用，响应字节不被重写。
func TestBridgeNormalReuse(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(w, r.Body) }))
	defer upstream.Close()
	g, identity, binding := newGateway(t, upstream)
	address, _, _ := localBridge(t, g, identity, binding)
	for i := 0; i < 2; i++ {
		response, err := http.Post(address+"/v1/responses", "application/octet-stream", bytes.NewReader([]byte{0, 1, 255, 10}))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || !bytes.Equal(data, []byte{0, 1, 255, 10}) {
			t.Fatal(data, err)
		}
	}
}

// TestHostPartialUploadBudget 验证控制仍健康也不能无限占用proxy接收阶段。
func TestHostPartialUploadBudget(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("不完整请求不应到达上游") }))
	defer upstream.Close()
	g, identity, binding := newGateway(t, upstream)
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(context.Background(), host, g, identity, binding) }()
	data, _ := proxywire.Encode(proxywire.RequestMeta{Service: "model", Method: "POST", Path: "/v1/responses"})
	if err := proxywire.Write(guest, proxywire.Frame{Kind: proxywire.Request, ID: 1, Payload: data}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("上传停滞未失败")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("上传停滞无界等待")
	}
}

// TestHostRejectsIdentityAndMetadata 验证旧身份、撤销绑定、未授权服务路径均不访问上游。
func TestHostRejectsIdentityAndMetadata(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("未授权请求访问上游") }))
	defer upstream.Close()
	for _, scenario := range []string{"epoch", "connection", "revoked", "service", "path"} {
		t.Run(scenario, func(t *testing.T) {
			g, identity, binding := newGateway(t, upstream)
			meta := proxywire.RequestMeta{Service: "model", Method: "POST", Path: "/v1/responses"}
			switch scenario {
			case "epoch":
				identity.Instance.Epoch = "old"
			case "connection":
				identity.ConnectionID = "old"
			case "revoked":
				g.Revoke(context.Background(), binding)
			case "service":
				meta.Service = "forbidden"
			case "path":
				meta.Path = "https://evil.invalid/"
			}
			host, guest := net.Pipe()
			defer host.Close()
			defer guest.Close()
			done := make(chan error, 1)
			go func() { done <- proxy.Serve(context.Background(), host, g, identity, binding) }()
			data, _ := proxywire.Encode(meta)
			if err := proxywire.Write(guest, proxywire.Frame{Kind: proxywire.Request, ID: 1, Payload: data}, time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := proxywire.Write(guest, proxywire.Frame{Kind: proxywire.End, ID: 1}, time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			f, err := proxywire.Read(guest, time.Now().Add(time.Second))
			if err != nil || f.Kind != proxywire.Abort {
				t.Fatal(f, err)
			}
			if err = <-done; err == nil {
				t.Fatal("未授权请求未终止")
			}
		})
	}
}

// TestBridgeSlowUploadRetainsChannel 验证发送proxy前上传空闲超时释放资格并允许正常后续请求。
func TestBridgeSlowUploadRetainsChannel(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(w, r.Body) }))
	defer upstream.Close()
	g, identity, binding := newGateway(t, upstream)
	address, _, _ := localBridge(t, g, identity, binding)
	conn, err := net.Dial("tcp", address[len("http://"):])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(4 * time.Second))
	io.WriteString(conn, "POST /v1/responses HTTP/1.1\r\nHost: loopback\r\nContent-Length: 100\r\n\r\nx")
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal(response.StatusCode)
		}
	} else if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	next, err := http.Post(address+"/v1/responses", "application/json", bytes.NewBufferString("ok"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(next.Body)
	next.Body.Close()
	if err != nil || string(data) != "ok" {
		t.Fatal(string(data), err)
	}
}

// TestHostMalformedAndLateFrames 验证半帧、超长帧、乱序ID和旧请求迟到数据终止通道。
func TestHostMalformedAndLateFrames(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer upstream.Close()
	for _, scenario := range []string{"partial", "oversized", "wrong-id", "late-data"} {
		t.Run(scenario, func(t *testing.T) {
			g, identity, binding := newGateway(t, upstream)
			host, guest := net.Pipe()
			defer host.Close()
			defer guest.Close()
			done := make(chan error, 1)
			go func() { done <- proxy.Serve(context.Background(), host, g, identity, binding) }()
			switch scenario {
			case "partial":
				guest.Write([]byte("S"))
			case "oversized":
				guest.Write([]byte{'S', 'A', 'P', 'X', 0, 1, 4, 0, 0, 1, 0, 0, 0, 0, 0, 1})
			default:
				data, _ := proxywire.Encode(proxywire.RequestMeta{Service: "model", Method: "POST", Path: "/v1/responses"})
				proxywire.Write(guest, proxywire.Frame{Kind: proxywire.Request, ID: 1, Payload: data}, time.Now().Add(time.Second))
				if scenario == "wrong-id" {
					proxywire.Write(guest, proxywire.Frame{Kind: proxywire.End, ID: 2}, time.Now().Add(time.Second))
				} else {
					proxywire.Write(guest, proxywire.Frame{Kind: proxywire.End, ID: 1}, time.Now().Add(time.Second))
					for {
						f, err := proxywire.Read(guest, time.Now().Add(time.Second))
						if err != nil {
							t.Fatal(err)
						}
						if f.Kind == proxywire.End {
							break
						}
					}
					proxywire.Write(guest, proxywire.Frame{Kind: proxywire.Data, ID: 1, Payload: []byte("late")}, time.Now().Add(time.Second))
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("畸形/迟到帧未失败")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("畸形帧未有界结束")
			}
		})
	}
}

// TestBridgeResourceRelease 验证重复真实HTTP会话关闭后自有handler与通道退出，FD不随轮次累积。
func TestBridgeResourceRelease(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(w, r.Body) }))
	defer upstream.Close()
	g, identity, binding := newGateway(t, upstream)
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			address, b, done := localBridge(t, g, identity, binding)
			transport := &http.Transport{DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			response, err := client.Post(address+"/v1/responses", "application/json", bytes.NewBufferString("ok"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = io.Copy(io.Discard, response.Body); err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			b.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Host通道未退出")
			}
		})
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	// 每轮禁用HTTP连接复用并显式退出任务，关闭后FD不应高于原始基线。
	if len(after) > len(before) {
		t.Fatal("FD随会话累积", len(before), len(after))
	}
	t.Log("12轮自有任务显式退出，FD前后", len(before), len(after))
}
