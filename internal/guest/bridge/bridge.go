// Package bridge 将Guest回环HTTP请求转换为独立proxy通道。
package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"safeagent/internal/proxywire"
)

// Bridge 拥有唯一物理通道和一个在途HTTP资格。
type Bridge struct {
	// conn 是本boot不可重连的proxy设备。
	conn net.Conn
	// service 是镜像固定的逻辑服务引用。
	service string
	// slots 拒绝第二个并发请求，不排队。
	slots chan struct{}
	// next 只由持有slots的handler访问。
	next uint32
	// ctx 在通道结束时取消。
	ctx context.Context
	// cancel 结束通道生命周期。
	cancel context.CancelFunc
	// fail 通知入口同时关闭control。
	fail func()
	// mu 串行化关闭与HTTP任务登记。
	mu sync.Mutex
	// closed 阻止关闭后登记新的handler。
	closed bool
	// handlers 等待已登记HTTP任务及其取消回调结束。
	handlers sync.WaitGroup
	// once 保证故障只发布一次。
	once sync.Once
}

// New 等待Host可信装配的Ready，失败时不开放HTTP监听。
func New(ctx context.Context, c net.Conn, service string, fail func()) (*Bridge, error) {
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(finished); c.Close() })
	defer func() {
		if !stop() {
			<-finished
		}
	}()
	f, err := proxywire.Read(c, time.Now().Add(10*time.Second))
	if err != nil {
		return nil, err
	}
	if f.Kind != proxywire.Ready {
		return nil, proxywire.ErrFrame
	}
	life, cancel := context.WithCancel(ctx)
	return &Bridge{conn: c, service: service, slots: make(chan struct{}, 1), ctx: life, cancel: cancel, fail: fail}, nil
}

// Close 失效proxy并通知Guest入口关闭控制服务，不停止工作负载。
func (b *Bridge) Close() {
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		b.cancel()
		b.conn.Close()
		b.fail()
	})
}

// ServeHTTP 有界缓存上传，取得资格后逐块交付响应；异常流关闭HTTP连接。
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 原生 MCP 客户端会先探测 OAuth 元数据；Guest 不持有远程认证，明确返回本地未提供。
	if strings.HasPrefix(r.URL.Path, "/.well-known/") {
		w.Header().Set("Connection", "close")
		http.NotFound(w, r)
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		reject(w, "proxy closed")
		return
	}
	b.handlers.Add(1)
	b.mu.Unlock()
	defer b.handlers.Done()
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	default:
		reject(w, "proxy busy")
		return
	}
	if b.ctx.Err() != nil {
		reject(w, "proxy closed")
		return
	}
	rc := http.NewResponseController(w)
	total := time.Now().Add(proxywire.Total)
	ctx, cancel := context.WithDeadline(r.Context(), total)
	defer cancel()
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(callbackDone); rc.SetReadDeadline(time.Now()); rc.SetWriteDeadline(time.Now()) })
	defer func() {
		if !stop() {
			<-callbackDone
		}
	}()
	uploadTotal := time.Now().Add(10 * time.Second)
	var upload bytes.Buffer
	buffer := make([]byte, proxywire.Limit)
	for {
		if err := rc.SetReadDeadline(proxywire.Deadline(uploadTotal)); err != nil {
			http.Error(w, "read deadline", 500)
			return
		}
		n, readErr := r.Body.Read(buffer)
		if upload.Len()+n > proxywire.RequestLimit {
			http.Error(w, "body too large", 413)
			return
		}
		upload.Write(buffer[:n])
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			http.Error(w, "upload failed", 400)
			return
		}
	}
	data := upload.Bytes()
	service := b.service
	route := r.URL.RequestURI()
	if strings.HasPrefix(r.URL.Path, "/mcp/") {
		ref := strings.TrimPrefix(r.URL.Path, "/mcp/")
		if ref == "" || strings.ContainsAny(ref, "/%?\\\r\n") || r.URL.RawQuery != "" {
			http.Error(w, "invalid MCP service", 400)
			return
		}
		service, route = "mcp:"+ref, "/mcp"
	}
	meta, err := proxywire.Encode(proxywire.RequestMeta{Service: service, Method: r.Method, Path: route, Headers: r.Header})
	if err != nil {
		http.Error(w, "headers too large", 431)
		return
	}
	if b.next == math.MaxUint32 {
		b.Close()
		http.Error(w, "proxy exhausted", 503)
		return
	}
	b.next++
	id := b.next
	// 从写出第一帧开始，任何中途取消都终止本boot通道，禁止迟到数据复用。
	streamDone := make(chan struct{})
	stopStream := context.AfterFunc(ctx, func() { defer close(streamDone); b.Close() })
	lifeDone := make(chan struct{})
	stopLife := context.AfterFunc(b.ctx, func() { defer close(lifeDone); cancel() })
	defer func() {
		if !stopLife() {
			<-lifeDone
		}
	}()
	defer func() {
		if !stopStream() {
			<-streamDone
		}
	}()
	write := func(kind byte, p []byte) error {
		return proxywire.Write(b.conn, proxywire.Frame{Kind: kind, ID: id, Payload: p}, proxywire.Deadline(total))
	}
	if err = write(proxywire.Request, meta); err == nil {
		for len(data) > 0 {
			n := min(len(data), proxywire.Limit)
			err = write(proxywire.Data, data[:n])
			if err != nil {
				break
			}
			data = data[n:]
		}
	}
	if err == nil {
		err = write(proxywire.End, nil)
	}
	if err != nil {
		b.Close()
		panic(http.ErrAbortHandler)
	}
	// 模型可在请求总预算内静默思考；Read仍从首字节起限制完整物理帧为两秒。
	f, err := proxywire.Read(b.conn, total)
	if err != nil || f.ID != id || f.Kind != proxywire.Response {
		b.Close()
		panic(http.ErrAbortHandler)
	}
	var response proxywire.ResponseMeta
	if err = proxywire.Decode(f.Payload, &response); err != nil || response.Status < 200 || response.Status > 599 {
		b.Close()
		panic(http.ErrAbortHandler)
	}
	for name, values := range response.Headers {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.Status)
	for {
		f, err = proxywire.Read(b.conn, total)
		if err != nil || f.ID != id {
			b.Close()
			panic(http.ErrAbortHandler)
		}
		switch f.Kind {
		case proxywire.Data:
			if err = rc.SetWriteDeadline(proxywire.Deadline(total)); err == nil {
				_, err = w.Write(f.Payload)
			}
			if err == nil {
				err = rc.Flush()
			}
			if err != nil {
				b.Close()
				panic(http.ErrAbortHandler)
			}
		case proxywire.End:
			return
		default:
			b.Close()
			panic(http.ErrAbortHandler)
		}
	}
}

// Serve 只接受Guest回环监听器，结束时等待全部handler及取消回调退出。
func (b *Bridge) Serve(listener net.Listener) error {
	host, _, err := net.SplitHostPort(listener.Addr().String())
	if err != nil || host != "127.0.0.1" {
		return errors.New("Bridge必须监听127.0.0.1")
	}
	server := &http.Server{Handler: b, MaxHeaderBytes: proxywire.Limit, ReadHeaderTimeout: proxywire.Budget, IdleTimeout: proxywire.Budget}
	stopped := make(chan struct{})
	stop := context.AfterFunc(b.ctx, func() { defer close(stopped); server.Close() })
	err = server.Serve(listener)
	b.Close()
	b.handlers.Wait()
	if !stop() {
		<-stopped
	}
	return err
}

// reject 关闭未消费body的HTTP连接并限制写预算，避免服务端排空停滞上传。
func reject(w http.ResponseWriter, message string) {
	w.Header().Set("Connection", "close")
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(proxywire.Budget)); err != nil {
		panic(http.ErrAbortHandler)
	}
	http.Error(w, message, http.StatusServiceUnavailable)
}
