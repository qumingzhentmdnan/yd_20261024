package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// binding 保存一次通道授权的不可变身份与服务集合。
type binding struct {
	// identity 来自可信 Host 握手结果。
	identity ChannelIdentity
	// services 限制本绑定能引用的服务。
	services map[string]bool
	// mcp 保存本绑定独占的 MCP 协议会话。
	mcp map[string]MCPSession
}

// Gateway 管理进程内绑定与在途流，不持久化授权。
type Gateway struct {
	// mu 保护绑定替换、撤销和在途请求注册的原子性。
	mu sync.Mutex
	// config 保存显式资源预算。
	config Config
	// credentials 仅在 Host 内解析凭证。
	credentials CredentialStore
	// services 保存不可变编译结果。
	services map[string]*service
	// bindings 保存当前有效授权。
	bindings map[BindingID]binding
	// current 按 VM 资源标识定位当前绑定。
	current map[string]BindingID
	// flows 保存仍占用配额的请求。
	flows map[*flow]struct{}
	// closed 在 Close 后禁止新增绑定或请求。
	closed bool
}

// New 校验 Host 配置并创建 Gateway；Host 必须提供非 nil 的凭证存储和 Resolver。
func New(config Config, credentials CredentialStore, resolver Resolver) (*Gateway, error) {
	if config.MaxBindings <= 0 || config.MaxConcurrent <= 0 || config.MaxRequestBytes <= 0 || config.MaxRequestBytes == math.MaxInt64 || config.MaxResponseBytes <= 0 || config.MaxHeaderBytes <= 0 || config.TotalTimeout < 0 || config.IdleTimeout <= 0 || len(config.Services) == 0 {
		return nil, ErrInvalidConfig
	}
	g := &Gateway{config: config, credentials: credentials, services: make(map[string]*service), bindings: make(map[BindingID]binding), current: make(map[string]BindingID), flows: make(map[*flow]struct{})}
	for _, spec := range config.Services {
		if _, exists := g.services[spec.Ref]; exists {
			return nil, ErrInvalidConfig
		}
		compiled, err := newService(spec, config, resolver)
		if err != nil {
			return nil, err
		}
		g.services[spec.Ref] = compiled
	}
	return g, nil
}

// Bind 原子登记 Host 当前通道，并撤销同一 VM 的旧连接或旧代次。
func (g *Gateway) Bind(ctx context.Context, identity ChannelIdentity, spec BindingSpec) (BindingID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if identity.Instance.Validate() != nil || identity.ConnectionID == "" || len(spec.ServiceRefs) == 0 {
		return "", ErrInvalidConfig
	}
	services := make(map[string]bool, len(spec.ServiceRefs))
	for _, ref := range spec.ServiceRefs {
		if _, exists := g.services[ref]; !exists || services[ref] {
			return "", ErrInvalidConfig
		}
		services[ref] = true
	}
	var random [16]byte
	rand.Read(random[:])
	id := BindingID("g_" + hex.EncodeToString(random[:]))
	g.mu.Lock()
	var retired []MCPSession
	defer func() {
		g.mu.Unlock()
		for _, session := range retired {
			session.Close()
		}
	}()
	if g.closed {
		return "", ErrClosed
	}
	old, exists := g.current[identity.Instance.Ref.ID]
	if exists {
		if g.bindings[old].identity == identity {
			return "", ErrConflict
		}
		retired = g.revokeLocked(old)
	} else if len(g.bindings) >= g.config.MaxBindings {
		return "", ErrCapacity
	}
	sessions := make(map[string]MCPSession)
	for ref := range services {
		s := g.services[ref]
		if s.config.MCP != nil {
			sessions[ref] = s.config.MCP.NewSession(s.client, g.credentials, s.config.CredentialRef)
		}
	}
	g.bindings[id] = binding{identity: identity, services: services, mcp: sessions}
	g.current[identity.Instance.Ref.ID] = id
	return id, nil
}

// revokeLocked 在授权锁内撤销绑定并向在途请求发出取消。
func (g *Gateway) revokeLocked(id BindingID) []MCPSession {
	var retired []MCPSession
	b := g.bindings[id]
	for _, session := range b.mcp {
		session.Revoke()
		retired = append(retired, session)
	}
	delete(g.bindings, id)
	delete(g.current, b.identity.Instance.Ref.ID)
	for f := range g.flows {
		if f.binding == id {
			f.cancel(ErrRevoked)
		}
	}
	return retired
}

// Revoke 撤销授权并发出取消，未知绑定视为已撤销。
func (g *Gateway) Revoke(ctx context.Context, id BindingID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	var retired []MCPSession
	if _, exists := g.bindings[id]; exists {
		retired = g.revokeLocked(id)
	}
	g.mu.Unlock()
	for _, session := range retired {
		session.Close()
	}
	return nil
}

// Close 失效所有授权并取消在途请求，不等待远端确认退出。
func (g *Gateway) Close() error {
	g.mu.Lock()
	g.closed = true
	var retired []MCPSession
	for id := range g.bindings {
		retired = append(retired, g.revokeLocked(id)...)
	}
	g.mu.Unlock()
	for _, session := range retired {
		session.Close()
	}
	for _, s := range g.services {
		s.client.CloseIdleConnections()
	}
	return nil
}

// Serve 接管输入 body，执行授权并返回持续占用配额的有界响应流。
func (g *Gateway) Serve(ctx context.Context, identity ChannelIdentity, id BindingID, request ProxyRequest) (ProxyResponse, error) {
	f, s, err := g.admit(ctx, identity, id, request)
	if err != nil {
		return ProxyResponse{}, errors.Join(err, request.Body.Close())
	}
	response, err := g.forward(f, s, request)
	if err != nil {
		f.finish(err)
		<-f.done
		return ProxyResponse{}, errors.Join(f.result(), f.closeErr)
	}
	return response, nil
}

// admit 将授权检查和在途注册放在同一锁内，撤销不能漏掉刚接纳的请求。
func (g *Gateway) admit(ctx context.Context, identity ChannelIdentity, id BindingID, request ProxyRequest) (*flow, *service, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := checkHeaders(request.Headers, g.config.MaxHeaderBytes); err != nil {
		return nil, nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, nil, ErrClosed
	}
	b, exists := g.bindings[id]
	if !exists || b.identity != identity || !b.services[request.ServiceRef] || request.Method != http.MethodPost && !(g.services[request.ServiceRef].config.MCP != nil && (request.Method == http.MethodDelete || request.Method == http.MethodGet)) {
		return nil, nil, ErrForbidden
	}
	s := g.services[request.ServiceRef]
	if !s.paths[request.Path] {
		return nil, nil, ErrForbidden
	}
	if len(g.flows) >= g.config.MaxConcurrent {
		return nil, nil, ErrCapacity
	}
	f := newFlow(ctx, g, id, request.Body)
	g.flows[f] = struct{}{}
	return f, s, nil
}

// forward 在配额内读取请求、注入凭证并获取上游响应头。
func (g *Gateway) forward(f *flow, s *service, request ProxyRequest) (ProxyResponse, error) {
	data, err := io.ReadAll(io.LimitReader(progressReader{flow: f, reader: request.Body}, g.config.MaxRequestBytes+1))
	if err != nil {
		return ProxyResponse{}, err
	}
	if int64(len(data)) > g.config.MaxRequestBytes {
		return ProxyResponse{}, ErrTooLarge
	}
	if s.config.MCP != nil {
		return g.forwardMCP(f, request, data)
	}
	credential, err := g.credentials.Resolve(f.ctx, s.config.CredentialRef)
	if err != nil {
		return ProxyResponse{}, err
	}
	if credential == "" || strings.ContainsAny(credential, "\r\n\x00") {
		return ProxyResponse{}, ErrCredential
	}
	target := s.origin
	target.Path, target.RawQuery, _ = strings.Cut(request.Path, "?")
	upstream, err := http.NewRequestWithContext(f.ctx, request.Method, target.String(), bytes.NewReader(data))
	if err != nil {
		return ProxyResponse{}, err
	}
	upstream.Header = selectHeaders(request.Headers, "Content-Type", "Accept", "Anthropic-Version")
	if s.config.Auth == APIKey && target.Path == "/v1/messages" {
		for _, value := range request.Headers.Values("Anthropic-Beta") {
			upstream.Header.Add("Anthropic-Beta", value)
		}
	}
	if s.config.Auth == Bearer {
		upstream.Header.Set("Authorization", "Bearer "+credential)
	} else {
		upstream.Header.Set("X-Api-Key", credential)
	}
	response, err := s.client.Do(upstream)
	if err != nil {
		return ProxyResponse{}, err
	}
	if err := f.attach(response.Body); err != nil {
		return ProxyResponse{}, err
	}
	if response.ContentLength > g.config.MaxResponseBytes {
		return ProxyResponse{}, ErrTooLarge
	}
	f.touch()
	headers := selectHeaders(response.Header, "Content-Type", "Retry-After", "X-Request-Id")
	if s.config.Auth == APIKey && target.Path == "/v1/messages" {
		for _, value := range response.Header.Values("X-Should-Retry") {
			headers.Add("X-Should-Retry", value)
		}
	}
	return ProxyResponse{Status: response.StatusCode, Headers: headers, Body: &responseBody{flow: f, source: response.Body, remaining: g.config.MaxResponseBytes}}, nil
}

// flow 统一管理取消、计时及输入输出资源的一次性释放。
type flow struct {
	// mu 保护响应交付与关闭、空闲计时之间的竞态。
	mu sync.Mutex
	// gateway 在关闭后释放全局配额。
	gateway *Gateway
	// binding 标识接纳本请求的授权。
	binding BindingID
	// ctx 覆盖整个请求和响应生命周期。
	ctx context.Context
	// cancel 传递撤销、关闭和空闲超时原因。
	cancel context.CancelCauseFunc
	// totalCancel 释放总超时定时器。
	totalCancel context.CancelFunc
	// stopWatch 解除上下文取消回调。
	stopWatch func() bool
	// idle 限制没有数据进展的时间。
	idle *time.Timer
	// lastProgress 保存最近字节进展时刻。
	lastProgress time.Time
	// input 是本请求接管的输入 body。
	input io.ReadCloser
	// output 在响应头返回后登记上游 body，之前为空。
	output io.ReadCloser
	// terminal 保存首次终止原因，nil 表示仍可继续。
	terminal error
	// done 在资源关闭且配额释放后关闭。
	done chan struct{}
	// closeErr 保存资源关闭错误，done 关闭后可读取。
	closeErr error
}

// newFlow 在 Gateway 锁内创建生命周期，回调等待注册完成后才能释放配额。
func newFlow(parent context.Context, gateway *Gateway, binding BindingID, input io.ReadCloser) *flow {
	var total context.Context
	var totalCancel context.CancelFunc
	if gateway.config.TotalTimeout > 0 {
		total, totalCancel = context.WithTimeout(parent, gateway.config.TotalTimeout)
	} else {
		total, totalCancel = context.WithCancel(parent)
	}
	ctx, cancel := context.WithCancelCause(total)
	f := &flow{gateway: gateway, binding: binding, ctx: ctx, cancel: cancel, totalCancel: totalCancel, input: input, lastProgress: time.Now(), done: make(chan struct{})}
	f.mu.Lock()
	f.stopWatch = context.AfterFunc(ctx, func() { f.finish(context.Cause(ctx)) })
	f.idle = time.AfterFunc(gateway.config.IdleTimeout, func() { f.finish(ErrIdleTimeout) })
	f.mu.Unlock()
	return f
}

// touch 记录实际数据进展，已终止的请求不再重置定时器。
func (f *flow) touch() {
	f.mu.Lock()
	if f.terminal == nil {
		f.lastProgress = time.Now()
		f.idle.Reset(f.gateway.config.IdleTimeout)
	}
	f.mu.Unlock()
}

// attach 与撤销串行交付上游 body，关闭后到达的响应立即关闭。
func (f *flow) attach(body io.ReadCloser) error {
	f.mu.Lock()
	if f.terminal != nil {
		err := f.terminal
		f.mu.Unlock()
		return errors.Join(err, body.Close())
	}
	f.output = body
	f.mu.Unlock()
	return nil
}

// finish 固定首次终止原因，在锁外关闭资源并释放配额。
func (f *flow) finish(cause error) {
	f.mu.Lock()
	if f.terminal != nil {
		f.mu.Unlock()
		return
	}
	// 空闲判定和终止状态提交共用锁，已排队的旧回调不能覆盖新字节进展。
	if cause == ErrIdleTimeout {
		remaining := f.gateway.config.IdleTimeout - time.Since(f.lastProgress)
		if remaining > 0 {
			f.idle.Reset(remaining)
			f.mu.Unlock()
			return
		}
	}
	f.terminal = cause
	output := f.output
	f.stopWatch()
	f.idle.Stop()
	f.mu.Unlock()
	f.cancel(cause)
	f.totalCancel()
	f.closeErr = f.input.Close()
	if output != nil {
		f.closeErr = errors.Join(f.closeErr, output.Close())
	}
	f.gateway.mu.Lock()
	delete(f.gateway.flows, f)
	f.gateway.mu.Unlock()
	close(f.done)
}

// result 返回固定终止原因，调用方不得用稍后的取消覆盖已观察到的错误。
func (f *flow) result() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.terminal
}

// progressReader 在请求输入产生数据时更新空闲计时。
type progressReader struct {
	// flow 管理本次请求取消与配额。
	flow *flow
	// reader 提供实际输入内容。
	reader io.Reader
}

// Read 遵守已发生的取消，并在实际读取字节后记录进展。
func (r progressReader) Read(p []byte) (int, error) {
	if err := r.flow.ctx.Err(); err != nil {
		return 0, context.Cause(r.flow.ctx)
	}
	n, err := r.reader.Read(p)
	if n > 0 {
		r.flow.touch()
	}
	return n, err
}

// responseBody 对单个读者实施交付容量限制，允许 Close 与 Read 并发。
type responseBody struct {
	// flow 保存取消、终止和资源所有权。
	flow *flow
	// source 提供上游流式内容。
	source io.Reader
	// remaining 仅由读取协程使用，限制还可交付的字节数。
	remaining int64
}

// Read 逐块交付响应，精确上限处探测 EOF，不交付超限字节。
func (b *responseBody) Read(p []byte) (int, error) {
	if err := b.flow.result(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var extra [1]byte
		n, err := b.source.Read(extra[:])
		if n > 0 {
			err = ErrTooLarge
		}
		if err != nil {
			b.flow.finish(err)
			err = b.flow.result()
		}
		return 0, err
	}
	n, err := b.source.Read(p[:min(int64(len(p)), b.remaining)])
	b.remaining -= int64(n)
	if n > 0 {
		b.flow.touch()
	}
	if err != nil {
		b.flow.finish(err)
		err = b.flow.result()
	}
	return n, err
}

// Close 终止上游读取并等待本地资源释放，不停止 Guest 工作负载。
func (b *responseBody) Close() error {
	b.flow.finish(io.ErrClosedPipe)
	<-b.flow.done
	return b.flow.closeErr
}
