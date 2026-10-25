// Package api 提供仅监听本机的带会话认证产品入口。
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"safeagent/internal/host/projectselection"
	"safeagent/internal/protocol"
)

// Request 是用户一次明确确认的启动选择，密钥只在 Host 内存流转。
type Request struct {
	// Agent 指定 codex 或 claude。
	Agent string `json:"agent"`
	// Project 是绝对项目根目录。
	Project string `json:"project"`
	// Files 是用户勾选的相对文件。
	Files []string `json:"files"`
	// Origin 是模型 HTTPS 源站。
	Origin string `json:"origin"`
	// Key 只供本次 Host 凭证存储使用。
	Key string `json:"key"`
	// MCP 是用户确认的外部工具连接，凭据仅在Host内存。
	MCP []MCP `json:"mcp,omitempty"`
	// Model 是用户显式自定义模型，可为空使用镜像默认值。
	Model string `json:"model"`
}

// MCP 是一次会话明确授权的工具连接。
type MCP struct {
	// Ref 是工具服务逻辑名称。
	Ref string `json:"ref"`
	// URL 是精确的HTTPS远程端点。
	URL string `json:"url"`
	// Key 是Host认证凭据，不发送Guest。
	Key string `json:"key"`
	// AllowedTools 是用户确认只读语义的工具名。
	AllowedTools []string `json:"allowedTools"`
}

// Terminal 暴露已经绑定的 Guest PTY。
type Terminal interface {
	io.ReadWriteCloser
	// Resize 设置Guest PTY行列数。
	Resize(context.Context, protocol.Dimension, protocol.Dimension) error
}

// Review 保存用户确认所针对的不可变审核标识与纯文本差异。
type Review struct {
	// ID 绑定当前冻结快照。
	ID string `json:"id"`
	// Changes 保存变更元数据。
	Changes any `json:"changes"`
	// Diff 是展示用纯文本，不作为执行内容。
	Diff string `json:"diff"`
}

// Runner 装配真实 VM，会话结束之前可调用审核回调。
type Runner func(context.Context, Request, func(Terminal) error, func(Review, func(context.Context) error) error) error

// Server 管理单个本机用户会话，不持久化凭据或终端内容。
type Server struct {
	// token 是本进程随机生成的访问凭据。
	token string
	// origin 是唯一允许的浏览器源。
	origin string
	// run 启动真实 VM。
	run Runner
	// mu 保护会话状态与有界输出。
	mu sync.Mutex
	// closing 在服务关闭后拒绝启动新会话。
	closing bool
	// running 表示当前 VM 尚未完全清理。
	running bool
	// terminal 是当前就绪的 PTY，未就绪时为 nil。
	terminal Terminal
	// output 保存最多 1MiB 待消费原始输出。
	output []byte
	// state 是用户可见阶段。
	state string
	// stopReview 让交互回调结束并进入审核。
	stopReview chan struct{}
	// stopOnce 保证仅通知一次。
	stopOnce sync.Once
	// review 是已冻结的审核结果。
	review *Review
	// apply 是显式确认后执行的写回函数。
	apply func(context.Context) error
	// decision 在用户批准或保留后结束审核。
	decision chan struct{}
	// done 在本次 VM 清理完毕时关闭。
	done chan struct{}
	// inputMu 保持浏览器输入的单次写入互斥。
	inputMu sync.Mutex
	// cancel 取消当前 VM 并回收资源。
	cancel context.CancelFunc
	// github 是可选的 GitHub OAuth 登录服务。
	github *githubAuth
}

// New 为本次本地监听地址创建随机认证上下文。
func New(origin string, run Runner) (*Server, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return &Server{token: hex.EncodeToString(secret), origin: origin, run: run, state: "idle"}, nil
}

// URL 返回认证值位于 fragment 的启动链接，fragment 不发送给 HTTP 服务。
func (s *Server) URL() string { return s.origin + "/#" + s.token }

// Handler 返回页面与受认证 API，拒绝跨源及 DNS rebinding 请求。
func (s *Server) Handler(page []byte, assets map[string][]byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'")
		if "http://"+r.Host != s.origin {
			http.Error(w, "拒绝访问", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/" && r.Method == "GET" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(page)
			return
		}
		if data, ok := assets[r.URL.Path]; ok && r.Method == "GET" {
			if r.URL.Path == "/assets/app.js" {
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			} else {
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			}
			w.Write(data)
			return
		}
		// OAuth 回调来自 GitHub 重定向，不能携带本地 fragment 令牌；其余认证接口仍受本地令牌保护。
		if r.URL.Path == "/api/auth/callback" {
			s.githubAuthHandler(w, r)
			return
		}
		if r.Header.Get("Origin") != "" && r.Header.Get("Origin") != s.origin {
			http.Error(w, "拒绝跨源请求", 403)
			return
		}
		authorized := subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) == 1
		if !authorized {
			if cookie, err := r.Cookie("safeagent_local_session"); err == nil {
				authorized = subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.token)) == 1
			}
		}
		if !authorized {
			http.Error(w, "需要本地会话认证", 401)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) == 1 && !strings.HasPrefix(r.URL.Path, "/api/auth/") {
			http.SetCookie(w, &http.Cookie{Name: "safeagent_local_session", Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		}
		if strings.HasPrefix(r.URL.Path, "/api/auth/") {
			s.githubAuthHandler(w, r)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "仅支持POST", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		s.handle(w, r)
	})
}

// decode 限制浏览器输入并拒绝未知字段与尾随 JSON。
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("额外JSON")
	}
	return nil
}

// handle 分派显式浏览器操作，错误响应不包含供应商或密钥原文。
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/directories":
		var req struct {
			// Path 是目录选择器当前目录。
			Path string `json:"path"`
		}
		if decode(w, r, &req) != nil {
			http.Error(w, "无效请求", 400)
			return
		}
		directories, err := projectselection.Directories(req.Path)
		if err != nil {
			http.Error(w, "无法读取目录", 400)
			return
		}
		json.NewEncoder(w).Encode(directories)
	case "/api/files":
		var req struct {
			// Project 是待枚举目录。
			Project string `json:"project"`
		}
		if decode(w, r, &req) != nil {
			http.Error(w, "无效请求", 400)
			return
		}
		files, err := projectselection.List(r.Context(), req.Project)
		if err != nil {
			http.Error(w, "无法读取Git项目，请检查根目录与权限", 400)
			return
		}
		json.NewEncoder(w).Encode(files)
	case "/api/start":
		var req Request
		if decode(w, r, &req) != nil || req.Key == "" || (req.Agent != "codex" && req.Agent != "claude") {
			http.Error(w, "启动参数无效", 400)
			return
		}
		candidates, err := projectselection.List(r.Context(), req.Project)
		if err != nil {
			http.Error(w, "项目不可用", 400)
			return
		}
		allowed := map[string]bool{}
		for _, f := range candidates {
			allowed[f.Path] = true
		}
		for _, p := range req.Files {
			if !allowed[p] {
				http.Error(w, "文件不在项目候选列表", 400)
				return
			}
		}
		s.mu.Lock()
		if s.running || s.closing {
			s.mu.Unlock()
			http.Error(w, "已有运行会话", 409)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		s.done = make(chan struct{})
		s.running = true
		s.state = "starting"
		s.output = nil
		s.review = nil
		s.apply = nil
		s.stopReview = make(chan struct{})
		s.decision = make(chan struct{})
		s.stopOnce = sync.Once{}
		s.mu.Unlock()
		go s.execute(ctx, req)
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case "/api/poll":
		s.mu.Lock()
		result := map[string]any{"state": s.state, "output": s.output, "review": s.review}
		s.output = nil
		s.mu.Unlock()
		json.NewEncoder(w).Encode(result)
	case "/api/input":
		var req struct {
			// Data 是 base64 编码的原始 PTY 字节。
			Data []byte `json:"data"`
		}
		if decode(w, r, &req) != nil {
			http.Error(w, "无效输入", 400)
			return
		}
		s.mu.Lock()
		term := s.terminal
		s.mu.Unlock()
		if term == nil {
			http.Error(w, "终端未就绪", 409)
			return
		}
		s.inputMu.Lock()
		_, writeErr := term.Write(req.Data)
		s.inputMu.Unlock()
		if writeErr != nil {
			http.Error(w, "终端写入失败", 409)
			return
		}
		json.NewEncoder(w).Encode(true)
	case "/api/resize":
		var req struct {
			// Rows 是PTY行数。
			Rows protocol.Dimension `json:"rows"`
			// Cols 是PTY列数。
			Cols protocol.Dimension `json:"cols"`
		}
		if decode(w, r, &req) != nil || protocol.Validate(req.Rows) != nil || protocol.Validate(req.Cols) != nil {
			http.Error(w, "无效终端大小", 400)
			return
		}
		s.mu.Lock()
		term := s.terminal
		s.mu.Unlock()
		if term == nil {
			http.Error(w, "终端未就绪", 409)
			return
		}
		if err := term.Resize(r.Context(), req.Rows, req.Cols); err != nil {
			http.Error(w, "调整终端失败", 409)
			return
		}
		json.NewEncoder(w).Encode(true)

	case "/api/review":
		s.mu.Lock()
		if s.terminal == nil {
			s.mu.Unlock()
			http.Error(w, "终端未就绪", 409)
			return
		}
		s.stopOnce.Do(func() { close(s.stopReview) })
		s.mu.Unlock()
		json.NewEncoder(w).Encode(true)
	case "/api/apply", "/api/retain":
		var req struct {
			// ID 必须明确绑定用户已经预览的审核。
			ID string `json:"id"`
		}
		if decode(w, r, &req) != nil {
			http.Error(w, "审核标识无效", 400)
			return
		}
		s.mu.Lock()
		// 确认回写期间串行化停止、清理与下次启动，保持审核及decision属于同一会话。
		defer s.mu.Unlock()
		if s.apply == nil || s.review == nil || req.ID != s.review.ID {
			http.Error(w, "没有待审核变更", 409)
			return
		}
		if r.URL.Path == "/api/apply" {
			if err := s.apply(r.Context()); err != nil {
				http.Error(w, "导出失败：可能存在宿主冲突，变更已保留", 409)
				return
			}
		}
		s.apply = nil
		close(s.decision)
		json.NewEncoder(w).Encode(true)

	case "/api/stop":
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Unlock()
		json.NewEncoder(w).Encode(true)
	default:
		http.NotFound(w, r)
	}
}

// execute 在独立生命周期中运行并有界收集输出，绝不将底层错误回显到浏览器。
func (s *Server) execute(ctx context.Context, req Request) {
	err := s.run(ctx, req, func(term Terminal) error {
		s.mu.Lock()
		s.terminal = term
		s.state = "running"
		stop := s.stopReview
		s.mu.Unlock()
		done := make(chan error, 1)
		go func() {
			buf := make([]byte, 8192)
			for {
				n, err := term.Read(buf)
				if n > 0 {
					s.mu.Lock()
					if len(s.output)+n > 1<<20 {
						s.mu.Unlock()
						done <- fmt.Errorf("终端输出未及时消费")
						return
					}
					s.output = append(s.output, buf[:n]...)
					s.mu.Unlock()
				}
				if err != nil {
					done <- err
					return
				}
			}
		}()
		select {
		case <-ctx.Done():
			term.Close()
			<-done
			return ctx.Err()
		case <-stop:
			term.Close()
			<-done
			return nil
		case err := <-done:
			if err == io.EOF {
				return nil
			}
			return err
		}
	}, func(review Review, apply func(context.Context) error) error {
		s.mu.Lock()
		s.terminal = nil
		s.review = &review
		s.apply = apply
		s.state = "review"
		decision := s.decision
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-decision:
			return nil
		}
	})
	s.mu.Lock()
	s.terminal = nil
	s.running = false
	close(s.done)
	s.apply = nil
	if err != nil {
		s.state = "failed（启动/连接失败，或会话已取消）"
	} else {
		s.state = "finished"
	}
	s.mu.Unlock()
}

// Close 取消当前会话；调用方应在关闭 HTTP 服务时调用。
func (s *Server) Close() {
	s.mu.Lock()
	s.closing = true
	if s.cancel != nil {
		s.cancel()
	}
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}
