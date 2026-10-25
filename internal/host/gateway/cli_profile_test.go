package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"

	"safeagent/internal/testkit/fakecredential"
)

// TestCLIServiceProfiles 验证Host输入保持显式，且供应商路径与认证不可由agent别名扩大。
func TestCLIServiceProfiles(t *testing.T) {
	networks := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	for _, agent := range []string{"codex", "claude"} {
		s, err := CLIService(agent, "https://explicit.example", "session-key", networks)
		if err != nil || s.Ref != "model" || s.Origin != "https://explicit.example" || s.CredentialRef != "session-key" || !slices.Equal(s.AllowedNetworks, networks) {
			t.Fatal(s, err)
		}
		wantPaths, wantAuth := []string{"/v1/responses"}, Bearer
		if agent == "claude" {
			wantPaths, wantAuth = []string{"/v1/messages", "/v1/messages?beta=true"}, APIKey
		}
		if s.Auth != wantAuth || !slices.Equal(s.Paths, wantPaths) {
			t.Fatal("供应商权限不精确", s)
		}
		s.AllowedNetworks[0] = netip.MustParsePrefix("198.51.100.0/24")
		if networks[0] != netip.MustParsePrefix("192.0.2.0/24") {
			t.Fatal("profile与调用方共享可变网段切片")
		}
	}
	for _, agent := range []string{"", "Codex", "claude-other", "https://evil.invalid"} {
		if _, err := CLIService(agent, "https://explicit.example", "session-key", networks); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("未知agent没有拒绝", agent, err)
		}
	}
}

// TestCLIExactTargets 验证查询仅按固定原文字节授权，变体不会访问凭证或上游。
func TestCLIExactTargets(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			var hits atomic.Int64
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			config := testConfig(upstream)
			spec, err := CLIService(agent, upstream.URL, "session-key", config.Services[0].AllowedNetworks)
			if err != nil {
				t.Fatal(err)
			}
			config.Services = []Service{spec}
			credentials := fakecredential.New(map[string]string{"session-key": "explicit-profile-test-only"})
			g, id := testGateway(t, config, credentials)
			for _, target := range spec.Paths {
				r := testRequest()
				r.Path = target
				response, err := g.Serve(context.Background(), testIdentity(), id, r)
				if err != nil {
					t.Fatal(target, err)
				}
				response.Body.Close()
			}
			for _, target := range []string{
				"/v1/messages?", "/v1/messages?beta=false", "/v1/messages?beta=True",
				"/v1/messages?beta=true&beta=true", "/v1/messages?beta=true&x=1",
				"/v1/messages?%62eta=true", "/v1/messages?beta=%74rue", "/v1/messages?beta=true;other=1",
				"/v1/messages?beta=true#fragment", "/v1/messages/count_tokens", "/v1/messages/count_tokens?beta=true",
				"/v1/responses?beta=true", "/v1/responses?x=1", "/v1/responses/compact",
				"https://evil.invalid/v1/messages?beta=true", "//evil.invalid/v1/messages?beta=true",
				"/v1/../v1/messages?beta=true", "/v1/%6dessages?beta=true",
			} {
				r := testRequest()
				r.Path = target
				if _, err := g.Serve(context.Background(), testIdentity(), id, r); !errors.Is(err, ErrForbidden) {
					t.Fatal("未登记目标没有拒绝", target, err)
				}
			}
			for _, method := range []string{http.MethodGet, http.MethodConnect, http.MethodHead} {
				r := testRequest()
				r.Path, r.Method = spec.Paths[0], method
				if _, err := g.Serve(context.Background(), testIdentity(), id, r); !errors.Is(err, ErrForbidden) {
					t.Fatal("未登记方法没有拒绝", method, err)
				}
			}
			otherTargets := []string{"/v1/messages", "/v1/messages?beta=true"}
			if agent == "claude" {
				otherTargets = []string{"/v1/responses"}
			}
			for _, target := range otherTargets {
				r := testRequest()
				r.Path = target
				if _, err := g.Serve(context.Background(), testIdentity(), id, r); !errors.Is(err, ErrForbidden) {
					t.Fatal("profile授权了另一供应商路由", target, err)
				}
			}
			if hits.Load() != int64(len(spec.Paths)) || credentials.Calls() != len(spec.Paths) {
				t.Fatal("被拒绝请求触及上游或凭证", hits.Load(), credentials.Calls())
			}
		})
	}
}

// TestMessagesBetaRequiresRegistration 验证既有无查询服务不会因新增语法自动取得beta查询权限。
func TestMessagesBetaRequiresRegistration(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("未登记查询不能访问上游") }))
	defer server.Close()
	config := testConfig(server)
	config.Services[0].Paths = []string{"/v1/messages"}
	config.Services[0].Auth = APIKey
	credentials := fakecredential.New(map[string]string{"test-key": "explicit-profile-test-only"})
	g, id := testGateway(t, config, credentials)
	r := testRequest()
	r.Path = "/v1/messages?beta=true"
	if _, err := g.Serve(context.Background(), testIdentity(), id, r); !errors.Is(err, ErrForbidden) {
		t.Fatal("未登记查询没有拒绝", err)
	}
	if credentials.Calls() != 0 {
		t.Fatal("被拒绝查询读取了凭证")
	}
}

// TestCLIQueryConfigurationBoundary 验证Host配置也不能登记其他查询或编码别名。
func TestCLIQueryConfigurationBoundary(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("无效配置不能访问上游") }))
	defer server.Close()
	for _, target := range []string{"/v1/messages?beta=false", "/v1/messages?beta=true&other=1", "/other?beta=true", "/v1/responses?beta=true", "/v1/messages?beta=%74rue"} {
		config := testConfig(server)
		config.Services[0].Paths = []string{target}
		g, err := New(config, fakecredential.New(map[string]string{"test-key": "explicit-profile-test-only"}), net.DefaultResolver)
		if err == nil {
			g.Close()
			t.Fatal("配置放开任意查询", target)
		}
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatal(target, err)
		}
	}
}
