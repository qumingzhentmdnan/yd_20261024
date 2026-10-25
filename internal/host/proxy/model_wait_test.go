package proxy_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestBridgeModelSilence 验证模型首响应与后续响应块静默超过两秒仍在请求总预算内成功。
func TestBridgeModelSilence(t *testing.T) {
	entered := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseTail := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-releaseFirst:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-releaseTail:
			io.WriteString(w, "data: tail\n\n")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	g, identity, binding := gatewayWithBudgets(t, upstream, 60*time.Second, 60*time.Second)
	address, _, _ := localBridge(t, g, identity, binding)
	result := make(chan error, 1)
	first := make(chan struct{})
	go func() {
		response, err := (&http.Client{Timeout: 12 * time.Second}).Post(address+"/v1/responses", "application/json", bytes.NewBufferString(`{"stream":true}`))
		if err != nil {
			result <- err
			return
		}
		defer response.Body.Close()
		data := make([]byte, len("data: first\n\n"))
		if _, err = io.ReadFull(response.Body, data); err != nil {
			result <- err
			return
		}
		if string(data) != "data: first\n\n" {
			t.Error("首块内容改变")
		}
		close(first)
		tail, err := io.ReadAll(response.Body)
		if err == nil && string(tail) != "data: tail\n\n" {
			t.Error("尾块内容改变")
		}
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("请求未到达TLS上游")
	}
	// 闸门从已观察请求起保持三秒，明确跨过旧Bridge两秒首字节预算。
	select {
	case err := <-result:
		t.Fatal("模型首响应等待提前结束", err)
	case <-time.After(3 * time.Second):
	}
	close(releaseFirst)
	select {
	case <-first:
	case err := <-result:
		t.Fatal("首块失败", err)
	case <-time.After(3 * time.Second):
		t.Fatal("首块未流式交付")
	}
	select {
	case err := <-result:
		t.Fatal("模型块间等待提前结束", err)
	case <-time.After(3 * time.Second):
	}
	close(releaseTail)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("响应未完成")
	}
}

// TestBridgeIdleFailureAllowsRetry 验证上游首响应超时和流中停滞只结束当前请求，下一请求仍可完成。
func TestBridgeIdleFailureAllowsRetry(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if calls.Add(1) == 1 {
					if streaming {
						w.Write([]byte("partial"))
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
					return
				}
				io.WriteString(w, "retry-ok")
			}))
			t.Cleanup(upstream.Close)
			g, identity, binding := gatewayWithBudgets(t, upstream, 150*time.Millisecond, 0)
			address, _, done := localBridge(t, g, identity, binding)
			client := &http.Client{Timeout: 3 * time.Second}
			first, err := client.Post(address+"/v1/responses", "application/json", bytes.NewBufferString("first"))
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.ReadAll(first.Body)
			first.Body.Close()
			if streaming && readErr == nil {
				t.Fatal("截断响应被当作完整响应")
			}
			if !streaming && first.StatusCode != http.StatusBadGateway {
				t.Fatal(first.StatusCode)
			}
			next, err := client.Post(address+"/v1/responses", "application/json", bytes.NewBufferString("next"))
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(next.Body)
			next.Body.Close()
			if err != nil || string(data) != "retry-ok" {
				t.Fatalf("重试失败：%q %v", data, err)
			}
			select {
			case err := <-done:
				t.Fatalf("请求失败导致代理退出：%v", err)
			default:
			}
		})
	}
}

// TestBridgeStreamBeyondSixtySeconds 验证持续进展的响应跨越旧六十秒截止后仍可完整交付。
func TestBridgeStreamBeyondSixtySeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("长响应验证需要超过60秒")
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		for i := 0; i < 65; i++ {
			io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
			}
		}
		io.WriteString(w, "done")
	}))
	t.Cleanup(upstream.Close)
	g, identity, binding := gatewayWithBudgets(t, upstream, 5*time.Second, 0)
	address, _, _ := localBridge(t, g, identity, binding)
	response, err := (&http.Client{Timeout: 75 * time.Second}).Post(address+"/v1/responses", "application/json", bytes.NewBufferString("long"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != strings.Repeat("x", 65)+"done" {
		t.Fatalf("长响应失败：%d字节 %v", len(data), err)
	}
}
