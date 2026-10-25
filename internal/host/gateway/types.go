// Package gateway 实现仅由可信 Host 通道调用的模型 HTTP 转发。
package gateway

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"time"

	"safeagent/internal/host/vm"
)

var (
	ErrInvalidConfig = errors.New("Gateway 配置无效")
	ErrForbidden     = errors.New("Gateway 请求未授权")
	ErrConflict      = errors.New("通道已经绑定")
	ErrClosed        = errors.New("Gateway 已关闭")
	ErrRevoked       = errors.New("Gateway 绑定已撤销")
	ErrCapacity      = errors.New("Gateway 已达到并发或绑定上限")
	ErrTooLarge      = errors.New("Gateway 数据超过限额")
	ErrIdleTimeout   = errors.New("Gateway 请求空闲超时")
	ErrCredential    = errors.New("Host 凭证不可用")
)

// ChannelIdentity 由 Host 已完成握手的物理通道建立，不能从 Guest 消息解码。
type ChannelIdentity struct {
	// Instance 标识 Host 已确认的 VM 启动代次。
	Instance vm.VMInstance
	// ConnectionID 标识该代次内不复用的连接。
	ConnectionID string
}

// BindingID 标识一次不可复活的通道授权。
type BindingID string

// BindingSpec 引用 Host 已配置的可访问服务。
type BindingSpec struct {
	// ServiceRefs 列出本绑定允许的逻辑服务，不是 URL。
	ServiceRefs []string
}

// CredentialStore 仅在 Host 内解析凭证，错误文本不能包含秘密。
type CredentialStore interface {
	// Resolve 返回显式引用的凭证，必须遵守取消，不得回退到其他凭证。
	Resolve(context.Context, string) (string, error)
}

// Resolver 提供可取消的 DNS 查询，生产可直接使用 net.DefaultResolver。
type Resolver interface {
	// LookupNetIP 返回本次拨号使用的完整地址集合。
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// AuthKind 限定 Host 注入认证的方式。
type AuthKind string

const (
	Bearer AuthKind = "bearer"
	APIKey AuthKind = "x-api-key"
)

// Service 保存只存在于 Host 的固定路由与认证配置。
type Service struct {
	// Ref 指定 Guest 可引用的逻辑服务名。
	Ref string
	// Origin 指定无路径、查询、用户信息或片段的 HTTPS 源站。
	Origin string
	// Paths 列出允许POST的精确目标；仅Claude Messages允许固定?beta=true查询。
	Paths []string
	// AllowedNetworks 显式限制每次 DNS 查询后实际拨号的地址。
	AllowedNetworks []netip.Prefix
	// CredentialRef 引用 Host 凭证，不向 Guest 提供。
	CredentialRef string
	// Auth 指定凭证注入方式。
	Auth AuthKind
}

// Config 固定 Gateway 的服务集合、资源预算及超时。
type Config struct {
	// Services 保存 Host 已批准的服务配置。
	Services []Service
	// MaxBindings 限制同时有效的 VM 绑定数量。
	MaxBindings int
	// MaxConcurrent 限制全局在途请求，响应流结束前一直占额。
	MaxConcurrent int
	// MaxRequestBytes 限制先缓存再发送的请求内容字节数。
	MaxRequestBytes int64
	// MaxResponseBytes 限制交付给 Guest 的响应内容字节数。
	MaxResponseBytes int64
	// MaxHeaderBytes 限制输入请求头与上游响应头的字节预算。
	MaxHeaderBytes int64
	// TotalTimeout 限制整个请求及响应流的持续时间。
	TotalTimeout time.Duration
	// IdleTimeout 限制没有请求或响应字节进展的时间。
	IdleTimeout time.Duration
	// RootCAs 为可选证书信任池，nil 使用系统信任，不禁用证书验证。
	RootCAs *x509.CertPool
}

// ProxyRequest 只携带逻辑服务和业务数据，不携带上游地址或可信身份。
type ProxyRequest struct {
	// ServiceRef 引用绑定允许的 Host 服务。
	ServiceRef string
	// Method 本轮只接受 POST。
	Method string
	// Path 必须精确匹配已登记目标；仅Claude Messages允许固定?beta=true查询。
	Path string
	// Headers 只允许读取固定白名单，调用期间调用方不得修改。
	Headers http.Header
	// Body 在调用 Serve 时转移所有权，Close 必须可并发解除阻塞 Read。
	Body io.ReadCloser
}

// ProxyResponse 保存上游状态和有界流式内容，调用方必须关闭 Body。
type ProxyResponse struct {
	// Status 保留上游 HTTP 状态，不跟随重定向。
	Status int
	// Headers 只包含允许交付的响应字段。
	Headers http.Header
	// Body 结束、关闭或取消后释放在途配额。
	Body io.ReadCloser
}
