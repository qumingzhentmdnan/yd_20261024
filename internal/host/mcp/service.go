// Package mcp 使用官方 SDK 终止远程 MCP 协议，只重新暴露 Host 允许的工具。
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"safeagent/internal/host/gateway"
)

var serviceName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Spec 是用户在 Host 确认的唯一 MCP 端点和工具权限。
type Spec struct {
	// Ref 是公开的逻辑服务名，不含 URL 或凭据。
	Ref string
	// UpstreamURL 是无查询、用户信息和重定向的 HTTPS MCP 精确端点。
	UpstreamURL string
	// AllowedNetworks 限制每次实际 DNS 解析得到的完整地址集合。
	AllowedNetworks []netip.Prefix
	// CredentialRef 引用 Host 凭据存储中的 token。
	CredentialRef string
	// AllowedTools 是允许发现及执行的确切工具名，空列表无效。
	AllowedTools []string
}

// NewService 创建只读工具白名单代理；工具的业务只读性仍须用户确认。
func NewService(spec Spec) (gateway.Service, error) {
	u, err := url.Parse(spec.UpstreamURL)
	if err != nil || !serviceName.MatchString(spec.Ref) || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Path == "" || strings.ContainsAny(u.Path, "%\\\r\n") || len(spec.AllowedTools) == 0 {
		return gateway.Service{}, gateway.ErrInvalidConfig
	}
	allowed := make(map[string]bool, len(spec.AllowedTools))
	for _, name := range spec.AllowedTools {
		if name == "" || len(name) > 128 || allowed[name] || strings.ContainsAny(name, "\r\n\x00") {
			return gateway.Service{}, gateway.ErrInvalidConfig
		}
		allowed[name] = true
	}
	origin := *u
	origin.Path = ""
	return gateway.Service{Ref: "mcp:" + spec.Ref, Origin: origin.String(), Paths: []string{"/mcp"}, AllowedNetworks: spec.AllowedNetworks, CredentialRef: spec.CredentialRef, Auth: gateway.Bearer, MCP: &factory{endpoint: spec.UpstreamURL, allowed: allowed}}, nil
}

// factory 保存不含密钥的不可变端点及工具策略。
type factory struct {
	// endpoint 是 Host 固定的 HTTPS MCP 地址。
	endpoint string
	// allowed 只在构造时写入。
	allowed map[string]bool
}

// NewSession 为单个可信 Gateway binding 创建独立的下游 SDK 会话空间。
func (f *factory) NewSession(client *http.Client, store gateway.CredentialStore, ref string) gateway.MCPSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{factory: f, client: client, store: store, ref: ref, ctx: ctx, cancel: cancel}
	return s
}

// session 将下游协议状态和远端请求限制在一次绑定内。
type session struct {
	// factory 保存静态授权。
	factory *factory
	// client 固定 TLS、DNS、IP 和禁止重定向的出站策略。
	client *http.Client
	// store 仅在 Host 发起请求时解析凭据。
	store gateway.CredentialStore
	// ref 是凭据引用，不是实际 token。
	ref string
	// ctx 在绑定撤销时取消所有远端请求。
	ctx context.Context
	// cancel 发布撤销。
	cancel context.CancelFunc
	// mu 串行化首次发现及关闭。
	mu sync.Mutex
	// server 保存已登记允许工具的 SDK 服务。
	server *sdk.Server
	// handler 保存本绑定独立的 MCP session ID 集合。
	handler *sdk.StreamableHTTPHandler
}

// connect 为一次操作建立经 Host 认证的远程 MCP 会话，不向 Guest 透传远端 session ID。
func (s *session) connect(ctx context.Context) (*sdk.ClientSession, error) {
	client := *s.client
	client.Transport = &authTransport{base: s.client.Transport, store: s.store, ref: s.ref, life: s.ctx, endpoint: s.factory.endpoint}
	client.Timeout = 55 * time.Second
	c := sdk.NewClient(&sdk.Implementation{Name: "safeagent-host", Version: "1"}, nil)
	return c.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: s.factory.endpoint, HTTPClient: &client, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
}

// initialize 发现允许工具并使用 SDK 重新发布，不发布资源、提示词、采样或其他远程能力。
func (s *session) initialize(ctx context.Context) error {
	upstream, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer upstream.Close()
	server := sdk.NewServer(&sdk.Implementation{Name: "safeagent-mcp", Version: "1"}, nil)
	cursor := ""
	pages := 0
	found := make(map[string]bool)
	for {
		pages++
		if pages > 16 {
			return gateway.ErrTooLarge
		}
		list, err := upstream.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return err
		}
		for _, tool := range list.Tools {
			if !s.factory.allowed[tool.Name] {
				continue
			}
			if found[tool.Name] {
				return gateway.ErrInvalidConfig
			}
			found[tool.Name] = true
			for _, schema := range []any{tool.InputSchema, tool.OutputSchema} {
				if schema == nil {
					continue
				}
				data, err := json.Marshal(schema)
				if err != nil {
					return gateway.ErrInvalidConfig
				}
				var object map[string]any
				if json.Unmarshal(data, &object) != nil || object["type"] != "object" {
					return gateway.ErrInvalidConfig
				}
			}
			if tool.InputSchema == nil {
				return gateway.ErrInvalidConfig
			}
			server.AddTool(tool, s.call)
		}
		cursor = list.NextCursor
		if cursor == "" {
			break
		}
	}
	for name := range s.factory.allowed {
		if !found[name] {
			return errors.New("MCP允许工具不存在")
		}
	}
	s.server = server
	s.handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true, SessionTimeout: 5 * time.Minute})
	return nil
}

// call 只执行已经由 SDK 注册且 Host 明确允许的工具，每次操作使用独立远端会话。
func (s *session) call(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	if !s.factory.allowed[req.Params.Name] {
		return nil, gateway.ErrForbidden
	}
	upstream, err := s.connect(ctx)
	if err != nil {
		return nil, errors.New("MCP远端连接失败")
	}
	defer upstream.Close()
	result, err := upstream.CallTool(ctx, &sdk.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
	if err != nil {
		return nil, errors.New("MCP远端工具调用失败")
	}
	return result, nil
}

// ServeHTTP 只开放有界的请求响应 MCP；可选独立 GET SSE 通道明确返回 405。
func (s *session) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		http.Error(w, "MCP connection revoked", 403)
		return
	}
	if s.handler == nil {
		if err := s.initialize(r.Context()); err != nil {
			s.mu.Unlock()
			http.Error(w, "MCP connection failed", 502)
			return
		}
	}
	handler := s.handler
	if r.Header.Get("Mcp-Session-Id") == "" {
		for range s.server.Sessions() {
			s.mu.Unlock()
			http.Error(w, "MCP session already initialized", 409)
			return
		}
	}
	s.mu.Unlock()
	handler.ServeHTTP(w, r)
}

// Revoke 只发布绑定取消，不等待首次发现持有的会话锁。
func (s *session) Revoke() { s.cancel() }

// Close 先取消远端请求，再关闭本绑定内的全部 SDK session。
func (s *session) Close() error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server != nil {
		for session := range s.server.Sessions() {
			session.Close()
		}
	}
	return nil
}

// authTransport 只向确切端点注入 Host 认证并将绑定撤销传播到 SDK 的独立上下文。
type authTransport struct {
	// base 是 Gateway 提供的受限出站 transport。
	base http.RoundTripper
	// store 解析实际 token。
	store gateway.CredentialStore
	// ref 是明确凭据引用。
	ref string
	// life 限制本次绑定生命周期。
	life context.Context
	// endpoint 阻止 SDK 或重定向改变目标。
	endpoint string
}

// RoundTrip 每次请求重新校验端点并注入认证，不接收 Guest 凭据。
func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() != t.endpoint {
		return nil, gateway.ErrForbidden
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(t.life, cancel)

	key, err := t.store.Resolve(ctx, t.ref)
	if err != nil || key == "" || strings.ContainsAny(key, "\r\n\x00") {
		stop()
		cancel()
		return nil, gateway.ErrCredential
	}
	request := r.Clone(ctx)
	request.Header = r.Header.Clone()
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := t.base.RoundTrip(request)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	response.Body = &authBody{ReadCloser: response.Body, stop: stop, cancel: cancel, remaining: 4 << 20}
	return response, nil
}

// authBody 将取消回调的生命周期延长到 SDK 消费完 HTTP 响应。
type authBody struct {
	// ReadCloser 保存实际远端响应。
	io.ReadCloser
	// remaining 限制一次远端响应，防止 SDK 解析前分配无界数据。
	remaining int64
	// stop 解除绑定取消回调。
	stop func() bool
	// cancel 释放单次请求上下文。
	cancel context.CancelFunc
}

// Close 关闭响应并释放本次请求的取消资源。
func (b *authBody) Close() error { b.stop(); b.cancel(); return b.ReadCloser.Close() }

// Read 限制远端 SSE 或 JSON 响应的实际字节，不信任 Content-Length。
func (b *authBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		var extra [1]byte
		n, err := b.ReadCloser.Read(extra[:])
		if n > 0 {
			return 0, gateway.ErrTooLarge
		}
		return 0, err
	}
	n, err := b.ReadCloser.Read(p[:min(int64(len(p)), b.remaining)])
	b.remaining -= int64(n)
	return n, err
}
