package gateway

import (
	"bytes"
	"io"
	"net/http"
)

// mcpWriter 缓存 SDK 的 JSON 响应并限制内存占用。
type mcpWriter struct {
	// header 保存经过白名单选择前的协议响应头。
	header http.Header
	// status 保存 HTTP 响应码，零值表示尚未写出。
	status int
	// body 保存有界响应内容。
	body bytes.Buffer
	// err 保存 SDK 输出超过预算的首次错误，避免交付部分 JSON。
	err error
	// limit 是本次响应的字节上限。
	limit int64
}

// Header 提供 SDK 响应头。
func (w *mcpWriter) Header() http.Header { return w.header }

// WriteHeader 固定第一次响应状态。
func (w *mcpWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

// Write 拒绝超过 Host 响应预算的协议输出。
func (w *mcpWriter) Write(p []byte) (int, error) {
	if int64(w.body.Len()+len(p)) > w.limit {
		w.err = ErrTooLarge
		return 0, ErrTooLarge
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

// forwardMCP 只交付固定 MCP 请求头，由独立 SDK handler 终止协议。
func (g *Gateway) forwardMCP(f *flow, request ProxyRequest, data []byte) (ProxyResponse, error) {
	g.mu.Lock()
	b, ok := g.bindings[f.binding]
	session := b.mcp[request.ServiceRef]
	g.mu.Unlock()
	if !ok {
		return ProxyResponse{}, ErrRevoked
	}
	r, err := http.NewRequestWithContext(f.ctx, request.Method, "http://127.0.0.1/mcp", bytes.NewReader(data))
	if err != nil {
		return ProxyResponse{}, err
	}
	r.Header = selectHeaders(request.Headers, "Content-Type", "Accept", "Mcp-Session-Id", "Mcp-Protocol-Version")
	w := &mcpWriter{header: make(http.Header), limit: g.config.MaxResponseBytes}
	session.ServeHTTP(w, r)
	if w.err != nil {
		return ProxyResponse{}, w.err
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if err := f.attach(io.NopCloser(bytes.NewReader(w.body.Bytes()))); err != nil {
		return ProxyResponse{}, err
	}
	return ProxyResponse{Status: w.status, Headers: selectHeaders(w.header, "Content-Type", "Mcp-Session-Id"), Body: &responseBody{flow: f, source: f.output, remaining: g.config.MaxResponseBytes}}, nil
}
