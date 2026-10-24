package gatewayprobe

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// watchedBody 在第二次读取前通知测试请求体已进入不完整上传阶段。
type watchedBody struct {
	// ReadCloser 持有本地 HTTP 连接上的请求体。
	io.ReadCloser
	// reads 仅由处理请求的协程访问。
	reads int
	// blocked 通知测试接下来会等待缺失的请求字节。
	blocked chan struct{}
}

// Read 让测试通过明确屏障观察部分请求体读取。
func (b *watchedBody) Read(p []byte) (int, error) {
	b.reads++
	if b.reads == 2 {
		close(b.blocked)
	}
	return b.ReadCloser.Read(p)
}

// TestPartialUploadCancellation 验证客户端保持连接但停止上传时仍能超时和撤销。
func TestPartialUploadCancellation(t *testing.T) {
	for name, revoke := range map[string]bool{"上传总超时": false, "上传时撤销": true} {
		t.Run(name, func(t *testing.T) {
			binding, cancel := context.WithCancel(context.Background())
			defer cancel()
			blocked, finished := make(chan struct{}), make(chan struct{})
			handler := proxyHandler(http.DefaultClient, "https://unused.invalid", binding, 150*time.Millisecond)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				r.Body = &watchedBody{ReadCloser: r.Body, blocked: blocked}
				handler.ServeHTTP(w, r)
			}))
			defer proxy.Close()
			conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := io.WriteString(conn, "POST /model HTTP/1.1\r\nHost: probe\r\nContent-Length: 10\r\n\r\n{"); err != nil {
				t.Fatal(err)
			}
			waitSignal(t, blocked)
			if revoke {
				cancel()
			}
			waitSignal(t, finished)
		})
	}
}

// TestDomainTLS 验证按原域名校验证书与 SNI，拨号仍只使用允许的 IP。
func TestDomainTLS(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != "model.example.com" {
			t.Errorf("SNI 丢失: %q", r.TLS.ServerName)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	client := testClient(upstream, lookup)
	response, err := client.Get("https://model.example.com:" + port)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response, err := client.Get("https://wrong.invalid:" + port); err == nil {
		response.Body.Close()
		t.Fatal("错误主机名未拒绝")
	} else {
		var hostnameError x509.HostnameError
		if !errors.As(err, &hostnameError) {
			t.Fatalf("非预期主机名错误: %v", err)
		}
	}
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs = x509.NewCertPool()
	if response, err := client.Get("https://model.example.com:" + port); err == nil {
		response.Body.Close()
		t.Fatal("不可信证书未拒绝")
	}
}

// TestStalledResponseTimeout 验证响应首块之后停滞也受整个请求的超时限制。
func TestStalledResponseTimeout(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmtBody := []byte("data: first\n\n")
		w.Write(fmtBody)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	client := testClient(upstream, net.DefaultResolver.LookupNetIP)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("停滞响应未超时: %v", err)
	}
	waitSignal(t, canceled)
}
