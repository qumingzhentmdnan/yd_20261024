package proxy_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"safeagent/internal/host/gateway"
	"safeagent/internal/host/vm"
	"safeagent/internal/testkit/fakecredential"
)

const cliTestSecret = "explicit-host-cli-profile-test-only"
const capturedClaudeBeta = "claude-code-20250219,interleaved-thinking-2025-05-14,prompt-caching-scope-2026-01-05,effort-2025-11-24"

// cliGateway 将正式CLI profile装配到仅信任假TLS证书及loopback网段的Gateway。
func cliGateway(t *testing.T, agent string, upstream *httptest.Server) (*gateway.Gateway, gateway.ChannelIdentity, gateway.BindingID) {
	t.Helper()
	spec, err := gateway.CLIService(agent, upstream.URL, "session-key", []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	g, err := gateway.New(gateway.Config{Services: []gateway.Service{spec}, MaxBindings: 1, MaxConcurrent: 1, MaxRequestBytes: 1 << 20, MaxResponseBytes: 8 << 20, MaxHeaderBytes: 16384, TotalTimeout: 10 * time.Second, IdleTimeout: 2 * time.Second, RootCAs: roots}, fakecredential.New(map[string]string{"session-key": cliTestSecret}), net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	identity := gateway.ChannelIdentity{Instance: vm.VMInstance{Ref: vm.VMRef{ID: "cli-profile"}, Epoch: "epoch"}, ConnectionID: "connection"}
	binding, err := g.Bind(context.Background(), identity, gateway.BindingSpec{ServiceRefs: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	return g, identity, binding
}

// cliRequest 根据已捕获路由发送业务及伪造认证，验证Bridge不会决定上游身份。
func cliRequest(t *testing.T, agent, address, body string) *http.Request {
	t.Helper()
	target := "/v1/responses"
	if agent == "claude" {
		target = "/v1/messages?beta=true"
	}
	request, err := http.NewRequest(http.MethodPost, address+target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer guest-untrusted")
	request.Header.Set("X-Api-Key", "guest-untrusted")
	request.Header.Set("Proxy-Authorization", "guest-untrusted")
	request.Header.Set("Cookie", "guest-untrusted")
	request.Header.Set("Forwarded", "host=evil.invalid")
	request.Header.Set("X-Forwarded-Host", "evil.invalid")
	request.Header.Set("X-Guest-Secret", "guest-untrusted")
	request.Header.Set("Anthropic-Version", "2023-06-01")
	request.Header["Anthropic-Beta"] = []string{capturedClaudeBeta, "future-cli-capability-test"}
	return request
}

// checkCLIUpstream 验证上游收到精确查询及Host认证，必要beta值不被硬编码重写。
func checkCLIUpstream(t *testing.T, agent string, r *http.Request) {
	t.Helper()
	target, auth, key := "/v1/responses", "Bearer "+cliTestSecret, ""
	if agent == "claude" {
		target, auth, key = "/v1/messages?beta=true", "", cliTestSecret
	}
	if r.Method != http.MethodPost || r.RequestURI != target || r.Header.Get("Authorization") != auth || r.Header.Get("X-Api-Key") != key {
		t.Error("实际路由或Host认证不符")
	}
	for _, name := range []string{"Proxy-Authorization", "Cookie", "Forwarded", "X-Forwarded-Host", "X-Guest-Secret"} {
		if r.Header.Get(name) != "" {
			t.Error("Guest不可信头没有剥离", name)
		}
	}
	if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Error("必要请求头丢失")
	}
	if agent == "claude" {
		if !reflect.DeepEqual(r.Header.Values("Anthropic-Beta"), []string{capturedClaudeBeta, "future-cli-capability-test"}) {
			t.Error("beta头没有原样保留")
		}
	} else if r.Header.Get("Anthropic-Beta") != "" {
		t.Error("Claude beta头被转发给Codex")
	}
}

// checkCLIGuestHeaders 验证只交付已批准响应头，Host假secret不出现在Guest头部。
func checkCLIGuestHeaders(t *testing.T, response *http.Response) {
	t.Helper()
	for name, values := range response.Header {
		for _, value := range values {
			if strings.Contains(value, cliTestSecret) {
				t.Error("Host假secret泄露到Guest响应头", name)
			}
		}
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "Set-Cookie", "Location", "X-Guest-Secret"} {
		if response.Header.Get(name) != "" {
			t.Error("未批准响应头未剥离", name)
		}
	}
}

// TestCLIProfileBridgeStreamingAndErrors 验证两种profile经真实Bridge/Host/Gateway的首块、认证、错误与正常复用。
func TestCLIProfileBridgeStreamingAndErrors(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			release := make(chan struct{})
			first := []byte("event: delta\ndata: first\n\n")
			tail := []byte{0, 255, 10, 13, 128}
			errorBody := `{"type":"error","error":{"type":"rate_limit_error","message":"retry later"}}`
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkCLIUpstream(t, agent, r)
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				for _, name := range []string{"Authorization", "X-Api-Key", "Set-Cookie", "X-Guest-Secret"} {
					w.Header().Set(name, cliTestSecret)
				}
				w.Header().Set("Location", "https://evil.invalid")
				w.Header().Set("X-Request-Id", "upstream-fixed-id")
				w.Header().Set("X-Should-Retry", "false")
				if string(data) == "error" {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "3")
					w.WriteHeader(http.StatusTooManyRequests)
					io.WriteString(w, errorBody)
					return
				}
				if string(data) != `{"stream":true}` {
					t.Error("模型body被修改")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write(first)
				w.(http.Flusher).Flush()
				select {
				case <-release:
					w.Write(tail)
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(upstream.Close)
			g, identity, binding := cliGateway(t, agent, upstream)
			address, _, _ := localBridge(t, g, identity, binding)
			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Do(cliRequest(t, agent, address, `{"stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			checkCLIGuestHeaders(t, response)
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("X-Request-Id") != "upstream-fixed-id" {
				t.Fatal("流式状态或头丢失")
			}
			gotFirst := make([]byte, len(first))
			if _, err = io.ReadFull(response.Body, gotFirst); err != nil || !bytes.Equal(gotFirst, first) {
				t.Fatal("未在尾块放行前交付首块", err)
			}
			close(release)
			gotTail, err := io.ReadAll(response.Body)
			if err != nil || !bytes.Equal(gotTail, tail) {
				t.Fatal("流或二进制被改变", gotTail, err)
			}
			response.Body.Close()
			response, err = client.Do(cliRequest(t, agent, address, "error"))
			if err != nil {
				t.Fatal("正常流结束后通道不可复用", err)
			}
			defer response.Body.Close()
			checkCLIGuestHeaders(t, response)
			data, err := io.ReadAll(response.Body)
			if err != nil || string(data) != errorBody || response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "3" {
				t.Fatal("上游错误没有原样交付", response.StatusCode, string(data), err)
			}
			retry := ""
			if agent == "claude" {
				retry = "false"
			}
			if response.Header.Get("X-Should-Retry") != retry {
				t.Fatal("供应商重试头丢失或跨供应商转发")
			}
			if strings.Contains(string(data), cliTestSecret) {
				t.Fatal("Host假secret泄露到Guest错误body")
			}
		})
	}
}

// TestCLIProfileBridgeCancellation 验证两种profile流式取消均到达假TLS上游且不伪装成成功EOF。
func TestCLIProfileBridgeCancellation(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			canceled := make(chan struct{})
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkCLIUpstream(t, agent, r)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(canceled)
			}))
			t.Cleanup(upstream.Close)
			g, identity, binding := cliGateway(t, agent, upstream)
			address, _, done := localBridge(t, g, identity, binding)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := cliRequest(t, agent, address, "cancel")
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request.WithContext(ctx))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			first := make([]byte, len("data: first\n\n"))
			if _, err = io.ReadFull(response.Body, first); err != nil {
				t.Fatal(err)
			}
			cancel()
			if _, err = io.ReadAll(response.Body); err == nil {
				t.Fatal("取消流被当作成功EOF")
			}
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("TLS上游未观察到取消")
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("取消后Host代理未退出")
			}
		})
	}
}
