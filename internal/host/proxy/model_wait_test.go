package proxy_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
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
