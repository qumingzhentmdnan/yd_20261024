package gateway

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"safeagent/internal/host/vm"
	"safeagent/internal/testkit/fakecredential"
)

// testIdentity 创建由 Host 测试夹具明确指定的可信身份。
func testIdentity() ChannelIdentity {
	return ChannelIdentity{Instance: vm.VMInstance{Ref: vm.VMRef{ID: "test-vm"}, Epoch: "boot-1"}, ConnectionID: "connection-1"}
}

// testConfig 给本机 TLS 测试提供明确且较小的预算。
func testConfig(server *httptest.Server) Config {
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return Config{
		Services:    []Service{{Ref: "model", Origin: server.URL, Paths: []string{"/v1/responses"}, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, CredentialRef: "test-key", Auth: Bearer}},
		MaxBindings: 2, MaxConcurrent: 1, MaxRequestBytes: 1024, MaxResponseBytes: 4096, MaxHeaderBytes: 4096,
		TotalTimeout: 3 * time.Second, IdleTimeout: time.Second, RootCAs: roots,
	}
}

// testGateway 创建正式 Gateway 与唯一已授权绑定。
func testGateway(t *testing.T, config Config, credentials CredentialStore) (*Gateway, BindingID) {
	t.Helper()
	g, err := New(config, credentials, net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	id, err := g.Bind(context.Background(), testIdentity(), BindingSpec{ServiceRefs: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	return g, id
}

// testRequest 构造明确的空模型请求，不使用真实凭证。
func testRequest() ProxyRequest {
	return ProxyRequest{ServiceRef: "model", Method: http.MethodPost, Path: "/v1/responses", Body: io.NopCloser(strings.NewReader("{}")), Headers: http.Header{"Content-Type": {"application/json"}}}
}

// waitDone 在真实网络观察上限内等待取消或资源关闭。
func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("等待资源释放超时")
	}
}

// TestStreamingAuthAndCapacity 验证真实 Gateway 的首块、凭证隔离、流生命周期配额和撤销。
func TestStreamingAuthAndCapacity(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") != "Bearer host-test-value" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Forwarded") != "" || r.URL.Path != "/v1/responses" {
			t.Error("上游认证或路由隔离失败")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "secret-cookie")
		w.Header().Set("Authorization", "must-not-forward")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	credentials := fakecredential.New(map[string]string{"test-key": "host-test-value"})
	g, id := testGateway(t, testConfig(server), credentials)
	request := testRequest()
	request.Headers.Set("Authorization", "Bearer guest-value")
	request.Headers.Set("X-Api-Key", "guest-value")
	request.Headers.Set("Cookie", "guest-value")
	request.Headers.Set("Forwarded", "host=evil.invalid")
	request.Headers.Set("Host", "evil.invalid")
	response, err := g.Serve(context.Background(), testIdentity(), id, request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Headers.Get("Set-Cookie") != "" || response.Headers.Get("Authorization") != "" {
		t.Fatal("响应泄露认证字段")
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("未及时获得首块: %q %v", line, err)
	}
	if _, err := g.Serve(context.Background(), testIdentity(), id, testRequest()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("响应未结束时没有占额: %v", err)
	}
	if err := g.Revoke(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrRevoked) {
		t.Fatalf("撤销未传递: %v", err)
	}
	waitDone(t, canceled)
	if credentials.Calls() != 1 {
		t.Fatal("拒绝请求仍访问凭证")
	}
}

// TestIdentityAndRoutes 验证旧身份、未授权服务、非法路径和关闭后的请求均拒绝。
func TestIdentityAndRoutes(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	credentials := fakecredential.New(map[string]string{"test-key": "host-test-value"})
	g, old := testGateway(t, testConfig(server), credentials)
	identity := testIdentity()
	identity.Instance.Epoch = "boot-2"
	identity.ConnectionID = "connection-2"
	id, err := g.Bind(context.Background(), identity, BindingSpec{ServiceRefs: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Serve(context.Background(), testIdentity(), old, testRequest()); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := g.Serve(context.Background(), testIdentity(), id, testRequest()); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	for _, badPath := range []string{"https://evil.invalid/v1/responses", "//evil.invalid", "/v1/../v1/responses", "/v1/responses?target=x", "/v1/%72esponses"} {
		r := testRequest()
		r.Path = badPath
		if _, err := g.Serve(context.Background(), identity, id, r); !errors.Is(err, ErrForbidden) {
			t.Fatalf("非法路由未拒绝: %q %v", badPath, err)
		}
	}
	r := testRequest()
	r.ServiceRef = "other-service"
	if _, err := g.Serve(context.Background(), identity, id, r); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if hits.Load() != 0 || credentials.Calls() != 0 {
		t.Fatal("拒绝前访问上游或凭证")
	}
	g.Close()
	if _, err := g.Serve(context.Background(), identity, id, testRequest()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

// TestExactBodyLimits 验证请求及流式响应的精确容量边界。
func TestExactBodyLimits(t *testing.T) {
	for name, responseBytes := range map[string]int{"精确响应上限": 16, "响应超限": 17} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				io.WriteString(w, strings.Repeat("r", responseBytes))
			}))
			defer server.Close()
			config := testConfig(server)
			config.MaxRequestBytes, config.MaxResponseBytes = 16, 16
			g, id := testGateway(t, config, fakecredential.New(map[string]string{"test-key": "host-test-value"}))
			request := testRequest()
			request.Body = io.NopCloser(strings.NewReader(strings.Repeat("q", 16)))
			response, err := g.Serve(context.Background(), testIdentity(), id, request)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if len(data) != 16 || responseBytes == 16 && readErr != nil || responseBytes > 16 && !errors.Is(readErr, ErrTooLarge) {
				t.Fatalf("响应边界错误: %d %v", len(data), readErr)
			}
			request = testRequest()
			request.Body = io.NopCloser(strings.NewReader(strings.Repeat("q", 17)))
			if _, err := g.Serve(context.Background(), testIdentity(), id, request); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("请求超限未拒绝: %v", err)
			}
		})
	}
}

// TestIdleAndTotalTimeout 验证正式响应包装器的停滞超时及部分输入取消。
func TestIdleAndTotalTimeout(t *testing.T) {
	for name, idle := range map[string]bool{"响应空闲超时": true, "响应总超时": false} {
		t.Run(name, func(t *testing.T) {
			canceled := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				io.WriteString(w, "first\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(canceled)
			}))
			defer server.Close()
			config := testConfig(server)
			want := error(context.DeadlineExceeded)
			if idle {
				config.IdleTimeout = 150 * time.Millisecond
				want = ErrIdleTimeout
			} else {
				config.TotalTimeout = 150 * time.Millisecond
			}
			g, id := testGateway(t, config, fakecredential.New(map[string]string{"test-key": "host-test-value"}))
			response, err := g.Serve(context.Background(), testIdentity(), id, testRequest())
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(response.Body)
			response.Body.Close()
			if !errors.Is(err, want) {
				t.Fatalf("超时错误不符: %v", err)
			}
			waitDone(t, canceled)
		})
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("未完成输入不应访问上游") }))
	defer server.Close()
	config := testConfig(server)
	config.IdleTimeout = 150 * time.Millisecond
	g, id := testGateway(t, config, fakecredential.New(map[string]string{"test-key": "host-test-value"}))
	input, writer := io.Pipe()
	defer writer.Close()
	request := testRequest()
	request.Body = input
	if _, err := g.Serve(context.Background(), testIdentity(), id, request); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("阻塞输入未按时取消: %v", err)
	}
}

// gateCredentials 用可取消闸门确定请求已注册但尚未访问上游的时刻。
type gateCredentials struct {
	// entered 通知测试凭证解析已经开始。
	entered chan struct{}
}

// Resolve 等待取消，确保撤销覆盖已注册的在途请求。
func (c gateCredentials) Resolve(ctx context.Context, _ string) (string, error) {
	close(c.entered)
	<-ctx.Done()
	return "", context.Cause(ctx)
}

// TestRevokeRegistrationRace 验证替换绑定与关闭都能取消刚登记的请求。
func TestRevokeRegistrationRace(t *testing.T) {
	for name, closeGateway := range map[string]bool{"绑定替换": false, "关闭Gateway": true} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("取消后不应访问上游") }))
			defer server.Close()
			credentials := gateCredentials{entered: make(chan struct{})}
			g, id := testGateway(t, testConfig(server), credentials)
			result := make(chan error, 1)
			go func() {
				_, err := g.Serve(context.Background(), testIdentity(), id, testRequest())
				result <- err
			}()
			waitDone(t, credentials.entered)
			if closeGateway {
				g.Close()
			} else {
				identity := testIdentity()
				identity.ConnectionID = "new-connection"
				if _, err := g.Bind(context.Background(), identity, BindingSpec{ServiceRefs: []string{"model"}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-result; !errors.Is(err, ErrRevoked) {
				t.Fatalf("取消注册请求失败: %v", err)
			}
			g.mu.Lock()
			count := len(g.flows)
			g.mu.Unlock()
			if count != 0 {
				t.Fatal("已取消请求仍占额")
			}
		})
	}
}

// TestCancelCloseOnce 验证读、关闭及撤销竞争时资源只释放一次。
func TestCancelCloseOnce(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	g, id := testGateway(t, testConfig(server), fakecredential.New(map[string]string{"test-key": "host-test-value"}))
	body := &countedBody{Reader: strings.NewReader("{}")}
	request := testRequest()
	request.Body = body
	response, err := g.Serve(context.Background(), testIdentity(), id, request)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { io.ReadAll(response.Body) })
	wg.Go(func() { response.Body.Close() })
	wg.Go(func() { g.Revoke(context.Background(), id) })
	wg.Wait()
	if body.closes.Load() != 1 {
		t.Fatal("输入资源重复关闭")
	}
}

// countedBody 统计唯一 Close，用于验证资源所有权。
type countedBody struct {
	// Reader 提供固定内容。
	*strings.Reader
	// closes 记录并发安全的关闭次数。
	closes atomic.Int64
}

// Close 记录关闭次数，不提供任何默认成功数据。
func (b *countedBody) Close() error {
	b.closes.Add(1)
	return nil
}
