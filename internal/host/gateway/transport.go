package gateway

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
)

// service 保存构造时编译的不可变服务配置。
type service struct {
	// config 保存 Host 配置的独立副本。
	config Service
	// origin 保存已解析的 HTTPS 源站。
	origin url.URL
	// paths 保存精确路径集合。
	paths map[string]bool
	// client 使用该服务专属的地址策略和 TLS 校验。
	client *http.Client
}

// validPath 检查固定路由使用规范的无查询路径。
func validPath(p string) bool {
	return strings.HasPrefix(p, "/") && path.Clean(p) == p && !strings.ContainsAny(p, "%?#\\\r\n\x00")
}

// newService 编译服务路由并固定每次实际拨号的地址授权。
func newService(spec Service, config Config, resolver Resolver) (*service, error) {
	origin, err := url.Parse(spec.Origin)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || spec.Ref == "" || spec.CredentialRef == "" || len(spec.Paths) == 0 || len(spec.AllowedNetworks) == 0 || spec.Auth != Bearer && spec.Auth != APIKey {
		return nil, ErrInvalidConfig
	}
	paths := make(map[string]bool, len(spec.Paths))
	for _, p := range spec.Paths {
		if !validPath(p) || paths[p] {
			return nil, ErrInvalidConfig
		}
		paths[p] = true
	}
	for _, prefix := range spec.AllowedNetworks {
		if !prefix.IsValid() {
			return nil, ErrInvalidConfig
		}
	}
	spec.Paths = slices.Clone(spec.Paths)
	spec.AllowedNetworks = slices.Clone(spec.AllowedNetworks)
	transport := &http.Transport{
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: config.MaxHeaderBytes,
		TLSHandshakeTimeout:    config.TotalTimeout,
		ResponseHeaderTimeout:  config.TotalTimeout,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: config.RootCAs},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := resolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, ErrForbidden
			}
			for _, ip := range ips {
				allowed := false
				for _, prefix := range spec.AllowedNetworks {
					if prefix.Contains(ip.Unmap()) {
						allowed = true
						break
					}
				}
				if !allowed {
					return nil, ErrForbidden
				}
			}
			var dialErr error
			for _, ip := range ips {
				conn, err := (&net.Dialer{Timeout: config.TotalTimeout}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				dialErr = err
			}
			return nil, dialErr
		},
	}
	return &service{config: spec, origin: *origin, paths: paths, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// checkHeaders 限制外部请求头的总大小，包含每个值的分隔开销。
func checkHeaders(headers http.Header, budget int64) error {
	for name, values := range headers {
		if int64(len(name))+2 > budget {
			return ErrTooLarge
		}
		budget -= int64(len(name)) + 2
		for _, value := range values {
			if int64(len(value))+2 > budget {
				return ErrTooLarge
			}
			budget -= int64(len(value)) + 2
		}
	}
	return nil
}

// selectHeaders 按固定允许列表新建头部，不复制认证或连接级字段。
func selectHeaders(source http.Header, names ...string) http.Header {
	result := make(http.Header, len(names))
	for _, name := range names {
		if values := source.Values(name); len(values) > 0 {
			result[name] = slices.Clone(values)
		}
	}
	return result
}
