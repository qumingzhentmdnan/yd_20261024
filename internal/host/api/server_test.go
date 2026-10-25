package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"runtime"
	"safeagent/internal/protocol"
	"strings"
	"testing"
)

// TestBrowserBoundary 验证本机入口认证、跨源与Host边界及不可信文本输出。
func TestBrowserBoundary(t *testing.T) {
	s, err := New("http://127.0.0.1:1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler([]byte("page"), map[string][]byte{"/assets/app.js": []byte("script")})
	for _, test := range []struct {
		// name 描述请求场景。
		name string
		// host 指定被测Host。
		host string
		// origin 指定被测浏览器源。
		origin string
		// auth 指定访问凭据。
		auth string
		// want 是预期HTTP状态。
		want int
	}{{"no token", "127.0.0.1:1234", "", "", 401}, {"foreign origin", "127.0.0.1:1234", "http://evil.example", "Bearer " + s.token, 403}, {"rebind", "evil.example", "", "Bearer " + s.token, 403}, {"authenticated", "127.0.0.1:1234", "http://127.0.0.1:1234", "Bearer " + s.token, 200}} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+test.host+"/api/poll", strings.NewReader("{}"))
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Authorization", test.auth)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestReviewLifecycle 验证用户审核前不会写回，确认回写后完成生命周期。
func TestReviewLifecycle(t *testing.T) {
	ready := make(chan struct{})
	applied := make(chan struct{})
	s, err := New("http://127.0.0.1:1234", func(ctx context.Context, req Request, interact func(Terminal) error, review func(Review, func(context.Context) error) error) error {
		close(ready)
		return review(Review{ID: "test-review", Diff: "<script>untrusted</script>"}, func(context.Context) error { close(applied); return nil })
	})
	if err != nil {
		t.Fatal(err)
	}
	s.running = true
	s.state = "starting"
	s.decision = make(chan struct{})
	s.done = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go s.execute(ctx, Request{})
	<-ready
	for {
		s.mu.Lock()
		waiting := s.apply != nil
		s.mu.Unlock()
		if waiting {
			break
		}
		runtime.Gosched()
	}
	select {
	case <-applied:
		t.Fatal("applied without confirmation")
	default:
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:1234/api/apply", strings.NewReader(`{"id":"test-review"}`))
	r.Header.Set("Authorization", "Bearer "+s.token)
	w := httptest.NewRecorder()
	s.Handler(nil, nil).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	<-applied
	<-s.done
	s.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.state != "finished" {
		t.Fatalf("state=%s running=%v", s.state, s.running)
	}
}

// TestApplySerializesStop 验证在途回写完成前不会取消审核所属会话。
func TestApplySerializesStop(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	stopped := make(chan struct{})
	s, err := New("http://127.0.0.1:1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.review = &Review{ID: "fixed"}
	s.running = true
	s.state = "review"
	s.decision = make(chan struct{})
	s.cancel = func() { close(stopped) }
	s.apply = func(context.Context) error {
		close(entered)
		<-release
		select {
		case <-stopped:
			t.Error("回写期间会话已被取消")
		default:
		}
		return nil
	}
	h := s.Handler(nil, nil)
	// request 构造当前审核的认证请求。
	request := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://127.0.0.1:1234/api/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+s.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	applyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { applyDone <- request("apply", `{"id":"fixed"}`) }()
	<-entered
	stopDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { stopDone <- request("stop", `{}`) }()
	close(release)
	if response := <-applyDone; response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if response := <-stopDone; response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	<-stopped
	if response := request("apply", `{"id":"fixed"}`); response.Code != 409 {
		t.Fatal("重复确认被接受")
	}
}

// outputTerminal 提供确定性的PTY输出，验证缓存滚动不会终止会话。
type outputTerminal struct {
	// Reader 保存本次测试输出，读完后返回EOF。
	*bytes.Reader
}

// Write 接受测试中未使用的PTY输入。
func (t *outputTerminal) Write(p []byte) (int, error) { return len(p), nil }

// Close 结束测试终端且不持有外部资源。
func (t *outputTerminal) Close() error { return nil }

// Resize 接受测试窗口尺寸。
func (t *outputTerminal) Resize(context.Context, protocol.Dimension, protocol.Dimension) error {
	return nil
}

// TestTerminalOutputWindow 验证超过缓存容量的输出不会结束为失败，且偏移和重复轮询内容一致。
func TestTerminalOutputWindow(t *testing.T) {
	data := bytes.Repeat([]byte("terminal-output"), 160000)
	s, err := New("http://127.0.0.1:1234", func(ctx context.Context, req Request, interact func(Terminal) error, review func(Review, func(context.Context) error) error) error {
		return interact(&outputTerminal{bytes.NewReader(data)})
	})
	if err != nil {
		t.Fatal(err)
	}
	s.running = true
	s.done = make(chan struct{})
	s.stopReview = make(chan struct{})
	go s.execute(context.Background(), Request{})
	for {
		s.mu.Lock()
		state := s.state
		s.mu.Unlock()
		if state == "cli-exited" {
			close(s.stopReview)
			break
		}
		runtime.Gosched()
	}
	<-s.done
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "http://127.0.0.1:1234/api/poll", strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+s.token)
		w := httptest.NewRecorder()
		s.Handler(nil, nil).ServeHTTP(w, r)
		var result struct {
			// State 是会话执行结果。
			State string `json:"state"`
			// Output 是仍保留的原始输出字节。
			Output []byte `json:"output"`
			// Start 是缓存首字节在完整输出中的位置。
			Start int64 `json:"outputStart"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.State != "finished" || result.Start != int64(len(data)-(1<<20)) || !bytes.Equal(result.Output, data[len(data)-(1<<20):]) {
			t.Fatalf("state=%s start=%d bytes=%d", result.State, result.Start, len(result.Output))
		}
	}
}

// TestReviewAfterCLIExit 验证CLI退出后仍可通过停止并审核结束会话。
func TestReviewAfterCLIExit(t *testing.T) {
	s, err := New("http://127.0.0.1:1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.running = true
	s.state = "cli-exited"
	s.stopReview = make(chan struct{})
	r := httptest.NewRequest("POST", "http://127.0.0.1:1234/api/review", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+s.token)
	w := httptest.NewRecorder()
	s.Handler(nil, nil).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	select {
	case <-s.stopReview:
	default:
		t.Fatal("停止并审核未通知CLI退出会话")
	}
}

// TestDeleteStoppedSandbox 验证停止后的沙箱可由用户明确删除并清除列表记录。
func TestDeleteStoppedSandbox(t *testing.T) {
	s, err := New("http://127.0.0.1:1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	s.SetDestroyer(func(context.Context) error { deleted = true; return nil })
	s.request = Request{Agent: "codex", Project: "/tmp/project", Model: "gpt-6.1-sol"}
	s.state = "finished"
	r := httptest.NewRequest("POST", "http://127.0.0.1:1234/api/delete", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+s.token)
	w := httptest.NewRecorder()
	s.Handler(nil, nil).ServeHTTP(w, r)
	if w.Code != 200 || !deleted {
		t.Fatalf("status=%d deleted=%v", w.Code, deleted)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.request.Project != "" || s.state != "idle" {
		t.Fatalf("project=%q state=%s", s.request.Project, s.state)
	}
}
