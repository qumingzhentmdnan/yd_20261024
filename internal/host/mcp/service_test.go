package mcp_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"safeagent/internal/guest/bridge"
	"safeagent/internal/host/gateway"
	hostmcp "safeagent/internal/host/mcp"
	"safeagent/internal/host/proxy"
	"safeagent/internal/host/vm"
	"safeagent/internal/proxywire"
	"safeagent/internal/testkit/fakecredential"
)

// fixture 保存真实 TLS、Host Gateway 及 Guest Bridge 组成的 MCP 测试链。
type fixture struct {
	// gateway 是正式受限 Gateway。
	gateway *gateway.Gateway
	// identity 是 Host 可信绑定身份。
	identity gateway.ChannelIdentity
	// binding 是本次授权。
	binding gateway.BindingID
	// url 是 Guest 回环 HTTP 服务入口。
	url string
	// calls 统计实际允许工具执行次数。
	calls atomic.Int64
	// denied 统计禁止工具执行次数。
	denied atomic.Int64
}

// newFixture 连接真实 TCP/TLS SDK 服务、正式 Gateway 和 net.Pipe 设备桥，不启动 VM。
func newFixture(t *testing.T, jsonResponse bool) *fixture {
	t.Helper()
	f := &fixture{}
	server := sdk.NewServer(&sdk.Implementation{Name: "reproducible-remote", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "read_status", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		f.calls.Add(1)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "safe-result"}}}, nil
	})
	server.AddTool(&sdk.Tool{Name: "delete_all", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		f.denied.Add(1)
		return &sdk.CallToolResult{}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: jsonResponse})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/remote/mcp" || r.Header.Get("Authorization") != "Bearer host-test-token" {
			t.Error("Host认证或固定endpoint失效")
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	service, err := hostmcp.NewService(hostmcp.Spec{Ref: "test", UpstreamURL: upstream.URL + "/remote/mcp", CredentialRef: "test", AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, AllowedTools: []string{"read_status"}})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	f.gateway, err = gateway.New(gateway.Config{Services: []gateway.Service{service}, MaxBindings: 3, MaxConcurrent: 3, MaxRequestBytes: 1 << 20, MaxResponseBytes: 4 << 20, MaxHeaderBytes: 16384, TotalTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, RootCAs: roots}, fakecredential.New(map[string]string{"test": "host-test-token"}), net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.gateway.Close() })
	f.identity = gateway.ChannelIdentity{Instance: vm.VMInstance{Ref: vm.VMRef{ID: "mcp-vm"}, Epoch: "boot-1"}, ConnectionID: "channel-1"}
	f.binding, err = f.gateway.Bind(context.Background(), f.identity, gateway.BindingSpec{ServiceRefs: []string{service.Ref}})
	if err != nil {
		t.Fatal(err)
	}
	host, guest := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		if err := proxywire.Write(host, proxywire.Frame{Kind: proxywire.Ready}, time.Now().Add(time.Second)); err != nil {
			done <- err
			return
		}
		done <- proxy.Serve(ctx, host, f.gateway, f.identity, f.binding)
	}()
	b, err := bridge.New(ctx, guest, "model", func() { cancel(); host.Close() })
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- b.Serve(listener) }()
	f.url = "http://" + listener.Addr().String() + "/mcp/test"
	t.Cleanup(func() { cancel(); b.Close(); <-done; <-bridgeDone })
	return f
}

// connectGuest 使用官方 MCP SDK 经正式 Guest HTTP Bridge 执行 initialize。
func connectGuest(t *testing.T, f *fixture) *sdk.ClientSession {
	t.Helper()
	c := sdk.NewClient(&sdk.Implementation{Name: "guest-test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := c.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: f.url, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// TestBridgeMCPJSONAndSSE 验证真实 HTTPS 上游的 JSON/SSE、握手、白名单发现与调用及禁止工具拒绝。
func TestBridgeMCPJSONAndSSE(t *testing.T) {
	for _, jsonResponse := range []bool{true, false} {
		name := "sse"
		if jsonResponse {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, jsonResponse)
			guest := connectGuest(t, f)
			if guest.InitializeResult().ProtocolVersion != "2025-06-18" {
				t.Fatalf("基线版本不匹配: %s", guest.InitializeResult().ProtocolVersion)
			}
			list, err := guest.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Tools) != 1 || list.Tools[0].Name != "read_status" {
				t.Fatalf("白名单泄漏:%v", list.Tools)
			}
			result, err := guest.CallTool(context.Background(), &sdk.CallToolParams{Name: "read_status", Arguments: map[string]any{}})
			if err != nil || result.IsError || len(result.Content) != 1 {
				t.Fatalf("调用失败:%v %v", result, err)
			}
			encoded, _ := json.Marshal(result)
			if !strings.Contains(string(encoded), "safe-result") || strings.Contains(string(encoded), "host-test-token") {
				t.Fatal("结果或凭据隔离失效")
			}
			_, err = guest.CallTool(context.Background(), &sdk.CallToolParams{Name: "delete_all", Arguments: map[string]any{}})
			if err == nil || f.denied.Load() != 0 || f.calls.Load() != 1 {
				t.Fatalf("未授权工具执行: err=%v calls=%d denied=%d", err, f.calls.Load(), f.denied.Load())
			}
		})
	}
}

// TestBindingSessionIsolation 验证 MCP session ID 无法跨 binding 使用，撤销会使旧绑定失效。
func TestBindingSessionIsolation(t *testing.T) {
	f := newFixture(t, true)
	guest := connectGuest(t, f)
	other := f.identity
	other.Instance.Ref.ID = "other-vm"
	other.ConnectionID = "other-channel"
	binding, err := f.gateway.Bind(context.Background(), other, gateway.BindingSpec{ServiceRefs: []string{"mcp:test"}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read_status","arguments":{}}}`
	response, err := f.gateway.Serve(context.Background(), other, binding, gateway.ProxyRequest{ServiceRef: "mcp:test", Method: "POST", Path: "/mcp", Headers: http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}, "Mcp-Session-Id": {guest.ID()}, "Mcp-Protocol-Version": {"2025-06-18"}}, Body: io.NopCloser(strings.NewReader(body))})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.Status != 404 || f.calls.Load() != 0 {
		t.Fatalf("跨绑定会话未拒绝: %d", response.Status)
	}
	if err := f.gateway.Revoke(context.Background(), f.binding); err != nil {
		t.Fatal(err)
	}
	if _, err := guest.ListTools(context.Background(), nil); err == nil {
		t.Fatal("已撤销绑定仍可用")
	}
}

// TestGuestFiles 验证模型及 MCP 配置只包含公开回环地址并拒绝非法引用。
func TestGuestFiles(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		files, err := hostmcp.GuestFiles(agent, "custom-model", []string{"test"})
		if err != nil {
			t.Fatal(err)
		}
		combined := ""
		for _, data := range files {
			combined += string(data)
		}
		if !strings.Contains(combined, "custom-model") || !strings.Contains(combined, "http://127.0.0.1:18080/mcp/test") {
			t.Fatal("生成配置缺少模型或MCP")
		}
	}
	if _, err := hostmcp.GuestFiles("codex", "", []string{"../escape"}); err == nil {
		t.Fatal("非法ref通过")
	}
}

// TestMCPPolicyRejectsEndpointsAndRedirects 验证入口固定 HTTPS、无查询及重定向，SDK 不能改变上游目标。
func TestMCPPolicyRejectsEndpointsAndRedirects(t *testing.T) {
	spec := hostmcp.Spec{Ref: "test", UpstreamURL: "https://example.com/mcp", CredentialRef: "test", AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, AllowedTools: []string{"read_status"}}
	for _, endpoint := range []string{"http://example.com/mcp", "https://user:password@example.com/mcp", "https://example.com/mcp?url=http://127.0.0.1", "https://example.com/mcp#fragment", "https://example.com/a%2fb", "https://example.com"} {
		next := spec
		next.UpstreamURL = endpoint
		if _, err := hostmcp.NewService(next); err == nil {
			t.Errorf("非法endpoint通过:%s", endpoint)
		}
	}
	var escaped atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { escaped.Add(1) }))
	defer target.Close()
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer remote.Close()
	spec.UpstreamURL = remote.URL + "/mcp"
	service, err := hostmcp.NewService(spec)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(remote.Certificate())
	g, err := gateway.New(gateway.Config{Services: []gateway.Service{service}, MaxBindings: 1, MaxConcurrent: 1, MaxRequestBytes: 4096, MaxResponseBytes: 4096, MaxHeaderBytes: 4096, TotalTimeout: time.Second, IdleTimeout: time.Second, RootCAs: roots}, fakecredential.New(map[string]string{"test": "test-token"}), net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	identity := gateway.ChannelIdentity{Instance: vm.VMInstance{Ref: vm.VMRef{ID: "redirect-vm"}, Epoch: "boot"}, ConnectionID: "channel"}
	binding, err := g.Bind(context.Background(), identity, gateway.BindingSpec{ServiceRefs: []string{service.Ref}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	response, err := g.Serve(context.Background(), identity, binding, gateway.ProxyRequest{ServiceRef: service.Ref, Method: "POST", Path: "/mcp", Headers: http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.Status != 502 || escaped.Load() != 0 || strings.Contains(string(data), "test-token") {
		t.Fatalf("重定向或凭据边界失效:%d %s", response.Status, data)
	}
}
