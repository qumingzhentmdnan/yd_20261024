package gatewayprobe

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testClient 使用受信任测试证书及显式地址策略创建本地探针客户端。
func testClient(upstream *httptest.Server, lookup func(context.Context, string, string) ([]netip.Addr, error)) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 4096,
			ResponseHeaderTimeout:  time.Second,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				ips, err := lookup(ctx, "ip", host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if !netip.MustParsePrefix("127.0.0.1/32").Contains(ip.Unmap()) {
						return nil, errors.New("目标地址未授权")
					}
				}
				return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
}

// proxyHandler 仅为本地验证装配绑定取消、固定路由、头清洗和逐块刷新。
func proxyHandler(client *http.Client, upstream string, binding context.Context, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if binding.Err() != nil || r.URL.Path != "/model" || r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		stop := context.AfterFunc(binding, cancel)
		defer stop()
		controller := http.NewResponseController(w)
		inputClosed := make(chan struct{})
		closeInput := context.AfterFunc(ctx, func() {
			controller.SetReadDeadline(time.Now())
			r.Body.Close()
			close(inputClosed)
		})
		defer func() {
			if !closeInput() {
				<-inputClosed
			}
		}()
		data, err := io.ReadAll(io.LimitReader(r.Body, 1025))
		if err != nil || len(data) > 1024 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		out, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream+"/v1/responses", strings.NewReader(string(data)))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		out.Header.Set("Content-Type", r.Header.Get("Content-Type"))
		out.Header.Set("Authorization", "Bearer host-test-only")
		response, err := client.Do(out)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		w.WriteHeader(response.StatusCode)
		buffer := make([]byte, 32*1024)
		for {
			n, readErr := response.Body.Read(buffer)
			if n > 0 {
				if controller.SetWriteDeadline(time.Now().Add(300*time.Millisecond)) != nil {
					return
				}
				if _, err := w.Write(buffer[:n]); err != nil {
					return
				}
				if controller.Flush() != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}
}

// waitSignal 在真实网络观察期限内等待资源取消事件。
func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("未在期限内观察到取消")
	}
}

// TestSSEAndCancellation 验证未结束的上游首块可见及两种取消来源。
func TestSSEAndCancellation(t *testing.T) {
	for name, revoke := range map[string]bool{"客户端断开": false, "绑定撤销": true} {
		t.Run(name, func(t *testing.T) {
			canceled := make(chan struct{})
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				if r.Header.Get("Authorization") != "Bearer host-test-only" || r.URL.Path != "/v1/responses" || r.Header.Get("Cookie") != "" {
					t.Error("认证或路由隔离失败")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(canceled)
			}))
			defer upstream.Close()
			binding, cancelBinding := context.WithCancel(context.Background())
			defer cancelBinding()
			proxy := httptest.NewServer(proxyHandler(testClient(upstream, net.DefaultResolver.LookupNetIP), upstream.URL, binding, 4*time.Second))
			defer proxy.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/model", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer guest-test-only")
			request.Header.Set("Cookie", "guest-cookie")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			line, err := bufio.NewReader(response.Body).ReadString('\n')
			if err != nil || line != "data: first\n" {
				t.Fatalf("首块未及时交付: %q %v", line, err)
			}
			if revoke {
				cancelBinding()
			} else {
				cancel()
			}
			waitSignal(t, canceled)
		})
	}
}

// TestSlowReader 验证慢读者触发有界背压，写超时可以结束阻塞转发。
func TestSlowReader(t *testing.T) {
	const total = 64 << 20
	var produced atomic.Int64
	canceled := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(canceled)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := make([]byte, 32*1024)
		for produced.Load() < total {
			n, err := w.Write(chunk)
			produced.Add(int64(n))
			if err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(proxyHandler(testClient(upstream, net.DefaultResolver.LookupNetIP), upstream.URL, context.Background(), 4*time.Second))
	defer proxy.Close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	response, err := http.Post(proxy.URL+"/model", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	// 不读取响应体，等待适配器的真实网络写超时，而非用 sleep 推测同步。
	waitSignal(t, canceled)
	runtime.ReadMemStats(&after)
	delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("上游已产生=%d/%d 字节，堆增量=%d 字节", produced.Load(), total, delta)
	if produced.Load() >= total || delta > 16<<20 {
		t.Fatal("慢读时出现整包消费或异常堆增长")
	}
}

// TestRedirectAndDNSChange 验证不跟随重定向以及每次拨号重新应用地址策略。
func TestRedirectAndDNSChange(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	var denied atomic.Bool
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		if denied.Load() {
			return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	client := testClient(upstream, lookup)
	response, err := client.Post(upstream.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || targetHits.Load() != 0 {
		t.Fatal("重定向逃逸")
	}
	denied.Store(true)
	if response, err := client.Post(upstream.URL, "application/json", strings.NewReader("{}")); err == nil {
		response.Body.Close()
		t.Fatal("DNS 改变后未拒绝目标地址")
	}
}
