package mcp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"safeagent/internal/host/gateway"
	"safeagent/internal/testkit/fakecredential"
)

// roundTripFunc 用于直接验证 SDK 解码前的 Host 出站边界。
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip 执行明确测试响应，不进行网络拨号。
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestUpstreamLimitBeforeSDK 验证远端JSON和SSE响应在SDK解析前就受到累计字节限制。
func TestUpstreamLimitBeforeSDK(t *testing.T) {
	for _, kind := range []string{"application/json", "text/event-stream"} {
		t.Run(kind, func(t *testing.T) {
			base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer host-test" {
					t.Fatal("认证未注入")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (4<<20)+1)))}, nil
			})
			transport := authTransport{base: base, store: fakecredential.New(map[string]string{"test": "host-test"}), ref: "test", life: context.Background(), endpoint: "https://example.com/mcp"}
			request, _ := http.NewRequest("POST", "https://example.com/mcp", nil)
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != gateway.ErrTooLarge || len(data) != 4<<20 {
				t.Fatalf("SDK前响应上限失效: bytes=%d err=%v", len(data), err)
			}
		})
	}
}

// TestRevokeDoesNotWaitForInitialization 验证首次发现占有session锁时，撤销只发布取消且不等待该锁。
func TestRevokeDoesNotWaitForInitialization(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{ctx: ctx, cancel: cancel}
	s.mu.Lock()
	done := make(chan struct{})
	go func() { s.Revoke(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Revoke等待初始化锁")
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("撤销未传递取消")
	}
	s.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
