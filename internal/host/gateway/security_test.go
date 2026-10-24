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
	"sync/atomic"
	"testing"
	"time"

	"safeagent/internal/testkit/fakecredential"
)

// resolverFunc 为正式拨号器提供可观察、可改变的测试 DNS 结果。
type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

// LookupNetIP 执行本次测试登记的解析逻辑。
func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

// TestDomainDNSAndRedirect 验证正式转发保留域名 TLS、不跟随重定向、每次拨号重新检查 IP。
func TestDomainDNSAndRedirect(t *testing.T) {
	var escaped atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { escaped.Add(1) }))
	defer target.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.TLS.ServerName != "model.example.com" || !strings.HasPrefix(r.Host, "model.example.com:") {
			t.Error("Host 或 SNI 被替换为 IP")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig(upstream)
	config.Services[0].Origin = "https://model.example.com:" + port
	var denied atomic.Bool
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		if denied.Load() {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.0.2.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	g, err := New(config, fakecredential.New(map[string]string{"test-key": "host-test-value"}), resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	id, err := g.Bind(context.Background(), testIdentity(), BindingSpec{ServiceRefs: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := g.Serve(context.Background(), testIdentity(), id, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.Status != http.StatusTemporaryRedirect || response.Headers.Get("Location") != "" || escaped.Load() != 0 {
		t.Fatal("重定向边界失效")
	}
	denied.Store(true)
	if _, err := g.Serve(context.Background(), testIdentity(), id, testRequest()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("混合授权/未授权 IP 未拒绝: %v", err)
	}
	config.RootCAs = x509.NewCertPool()
	badTLS, err := New(config, fakecredential.New(map[string]string{"test-key": "host-test-value"}), resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer badTLS.Close()
	badID, err := badTLS.Bind(context.Background(), testIdentity(), BindingSpec{ServiceRefs: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badTLS.Serve(context.Background(), testIdentity(), badID, testRequest()); err == nil {
		t.Fatal("不可信 TLS 证书未拒绝")
	}
}

// TestCredentialAndHeaderLimits 验证另一种认证方式、未知凭证无回退及请求头预算。
func TestCredentialAndHeaderLimits(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Api-Key") != "host-test-value" || r.Header.Get("Authorization") != "" {
			t.Error("API Key 注入错误")
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	config := testConfig(server)
	config.Services[0].Auth = APIKey
	g, id := testGateway(t, config, fakecredential.New(map[string]string{"test-key": "host-test-value"}))
	response, err := g.Serve(context.Background(), testIdentity(), id, testRequest())
	if err != nil || response.Status != http.StatusTooManyRequests || response.Headers.Get("Retry-After") != "1" {
		t.Fatalf("上游错误状态未保留: %v", err)
	}
	response.Body.Close()
	request := testRequest()
	request.Headers.Set("Authorization", strings.Repeat("x", 4097))
	if _, err := g.Serve(context.Background(), testIdentity(), id, request); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("请求头超限未拒绝: %v", err)
	}
	missing, missingID := testGateway(t, config, fakecredential.New(nil))
	if _, err := missing.Serve(context.Background(), testIdentity(), missingID, testRequest()); !errors.Is(err, fakecredential.ErrNotFound) {
		t.Fatalf("缺失凭证错误被掩盖: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatal("无授权或超限请求仍访问上游")
	}
}

// TestProgressResetsIdle 验证持续交付数据的响应可以跨越单个空闲预算。
func TestProgressResetsIdle(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for range 6 {
			select {
			case <-ticker.C:
				io.WriteString(w, "data\n")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer server.Close()
	config := testConfig(server)
	config.IdleTimeout = 250 * time.Millisecond
	g, id := testGateway(t, config, fakecredential.New(map[string]string{"test-key": "host-test-value"}))
	response, err := g.Serve(context.Background(), testIdentity(), id, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "data\n" {
		t.Fatalf("未获得首次进展: %q %v", first, err)
	}
	// 模拟旧空闲回调已排队但在新进展后才执行，必须复核当前状态。
	response.Body.(*responseBody).flow.finish(ErrIdleTimeout)
	data, err := io.ReadAll(reader)
	if err != nil || first+string(data) != strings.Repeat("data\n", 6) {
		t.Fatalf("有进展的流被空闲超时中断: %q %v", data, err)
	}
}
