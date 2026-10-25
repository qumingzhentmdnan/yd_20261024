package gateway

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"safeagent/internal/testkit/fakecredential"
)

// stalledMCP 模拟 SDK 正在等待初始化持锁，Close 需要等待业务退出。
type stalledMCP struct {
	// revoked 在撤销发布后关闭。
	revoked chan struct{}
	// release 控制 Close 何时完成。
	release chan struct{}
}

// NewSession 返回测试的唯一首次绑定会话。
func (s *stalledMCP) NewSession(*http.Client, CredentialStore, string) MCPSession { return s }

// ServeHTTP 不参与本生命周期锁回归。
func (s *stalledMCP) ServeHTTP(http.ResponseWriter, *http.Request) {}

// Revoke 先发布非阻塞取消。
func (s *stalledMCP) Revoke() { close(s.revoked) }

// Close 模拟等待初始化任务结束。
func (s *stalledMCP) Close() error { <-s.release; return nil }

// TestMCPRevokeReleasesGlobalLockBeforeClose 验证等待SDK收尾时全局授权锁仍可服务其他可信绑定。
func TestMCPRevokeReleasesGlobalLockBeforeClose(t *testing.T) {
	session := &stalledMCP{revoked: make(chan struct{}), release: make(chan struct{})}
	g, err := New(Config{Services: []Service{{Ref: "mcp:test", Origin: "https://example.com", Paths: []string{"/mcp"}, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, CredentialRef: "test", Auth: Bearer, MCP: session}, {Ref: "model", Origin: "https://example.com", Paths: []string{"/v1/responses"}, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, CredentialRef: "test", Auth: Bearer}}, MaxBindings: 2, MaxConcurrent: 1, MaxRequestBytes: 1024, MaxResponseBytes: 1024, MaxHeaderBytes: 1024, TotalTimeout: time.Second, IdleTimeout: time.Second}, fakecredential.New(map[string]string{"test": "fake"}), net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := g.Bind(context.Background(), testIdentity(), BindingSpec{ServiceRefs: []string{"mcp:test"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.Revoke(context.Background(), binding) }()
	select {
	case <-session.revoked:
	case <-time.After(time.Second):
		t.Fatal("未发布撤销")
	}
	bound := make(chan error, 1)
	identity := testIdentity()
	identity.Instance.Ref.ID = "other-vm"
	go func() {
		_, err := g.Bind(context.Background(), identity, BindingSpec{ServiceRefs: []string{"model"}})
		bound <- err
	}()
	select {
	case err := <-bound:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("等待SDK Close时全局锁仍被占用")
	}
	close(session.release)
	if err := <-done; err != nil {
		t.Error(err)
	}
	g.Close()
}
