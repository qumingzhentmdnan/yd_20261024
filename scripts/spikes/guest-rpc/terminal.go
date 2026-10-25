package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// attachment 表示单次终端授权，不能在新控制连接中复活。
type attachment struct {
	// stream 是 Host 分配的非零标识。
	stream uint32
	// token 只允许当前 attachment 完成一次 Bind。
	token string
	// session 是实际 PTY 所属的工作负载。
	session *session
	// bound 防止同一个 token 重复绑定。
	bound bool
	// ctx 在 detach、端口错误或控制断开后取消。
	ctx context.Context
	// cancel 中止本 attachment 的输入输出等待。
	cancel context.CancelFunc
	// done 在已绑定端口的读写循环退出且设备关闭后关闭。
	done chan struct{}
}

// binding 是 terminal Bind/Bound 的身份信息。
type binding struct {
	// SessionID 标识目标工作负载。
	SessionID string `json:"sessionID"`
	// Epoch 必须与控制 Hello 一致。
	Epoch string `json:"epoch"`
	// ConnectionID 限定当前控制连接。
	ConnectionID string `json:"connectionID"`
	// BindToken 必须与 AttachTerminal 返回值一致。
	BindToken string `json:"bindToken"`
}

// attach 创建尚未开始发送终端字节的授权。
func (c *connection) attach(p params) (any, *rpcError) {
	c.guest.mu.Lock()
	s := c.guest.sessions[p.SessionID]
	c.guest.mu.Unlock()
	if s == nil {
		return nil, failure("NOT_FOUND")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.StreamID == 0 || c.streams[p.StreamID] {
		return nil, failure("INVALID_ARGUMENT")
	}
	if c.attachment != nil {
		return nil, failure("FAILED_PRECONDITION")
	}
	ctx, cancel := context.WithCancel(c.ctx)
	a := &attachment{stream: p.StreamID, token: newID(), session: s, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	c.attachment = a
	c.streams[p.StreamID] = true
	return map[string]any{"streamID": a.stream, "bindToken": a.token}, nil
}

// detach 只释放仍属于当前 attachment 的权限，不终止进程。
func (c *connection) detach(a *attachment) {
	a.cancel()
	c.mu.Lock()
	bound := a.bound
	c.mu.Unlock()
	// 已绑定端口必须先停止旧读写者，再允许新授权使用同一设备。
	if bound {
		<-a.done
	}
	c.mu.Lock()
	if c.attachment == a {
		c.attachment = nil
		a.session.mu.Lock()
		a.session.attached = false
		a.session.mu.Unlock()
		a.session.wakeSpace()
	}
	c.mu.Unlock()
}

// terminalLoop 独立读取终端端口，控制断开后结束全部终端等待。
func (c *connection) terminalLoop() {
	for c.ctx.Err() == nil {
		port, err := openDevice(c.ctx, "/dev/safeagent.terminal")
		if err != nil {
			fmt.Fprintln(os.Stderr, "TERMINAL_OPEN_ERROR", err)
			c.cancel()
			return
		}
		err = c.serveTerminal(port)
		port.file.Close()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "TERMINAL_PROTOCOL_ERROR", err)
		}
		if err := pause(c.ctx); err != nil {
			return
		}
	}
}

// serveTerminal 先绑定授权，再并行转发 PTY 输入与输出。
func (c *connection) serveTerminal(port *device) error {
	f, err := readFrame(port, true)
	if err != nil {
		return err
	}
	var b binding
	if f.kind != bindKind || decodeJSON(f.body, &b) != nil {
		return errors.New("expected bind")
	}
	c.mu.Lock()
	a := c.attachment
	valid := c.hello && a != nil && a.ctx.Err() == nil && !a.bound && a.stream == f.stream && a.token == b.BindToken && a.session.id == b.SessionID && c.id == b.ConnectionID && c.epoch == b.Epoch
	if valid {
		a.bound = true
		a.session.mu.Lock()
		a.session.attached = true
		a.session.mu.Unlock()
	}
	c.mu.Unlock()
	if !valid {
		ctx, cancel := context.WithTimeout(c.ctx, time.Second)
		defer cancel()
		_ = writeFrame(&device{file: port.file, ctx: ctx}, frame{kind: abortKind, stream: f.stream, body: jsonBody(failure("FAILED_PRECONDITION"))})
		return errors.New("invalid binding")
	}
	defer func() {
		port.file.Close()
		close(a.done)
		c.detach(a)
	}()
	boundPort := &device{file: port.file, ctx: a.ctx}
	s := a.session
	s.mu.Lock()
	dropped := s.dropped
	s.mu.Unlock()
	if err := writeFrame(boundPort, frame{kind: boundKind, stream: a.stream, body: jsonBody(map[string]any{"droppedBytes": dropped})}); err != nil {
		return err
	}
	finished := make(chan struct{})
	go func() { defer close(finished); c.forwardOutput(a, boundPort) }()
	// 取消 attachment 后先等待输出协程退出，避免旧写者使用新端口。
	defer func() { a.cancel(); <-finished }()
	inputEnded := false
	for {
		f, err = readFrame(boundPort, true)
		if err != nil {
			return err
		}
		if f.stream != a.stream {
			return errors.New("unknown stream")
		}
		switch f.kind {
		case dataKind:
			if inputEnded {
				return errors.New("data after fin")
			}
			if err := writeBytes(&device{file: s.pty.file, ctx: a.ctx}, f.body); err != nil {
				return err
			}
		case finKind:
			inputEnded = true
		case abortKind:
			return nil
		default:
			return errors.New("unexpected terminal kind")
		}
	}
}

// writeBytes 将单个有界终端数据块完整写入 PTY。
func writeBytes(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// forwardOutput 使用单帧待写缓冲，终端慢读不占用控制循环。
func (c *connection) forwardOutput(a *attachment, port *device) {
	s := a.session
	outputEnded := false
	for {
		s.mu.Lock()
		size := min(len(s.output), terminalLimit)
		data := append([]byte(nil), s.output[:size]...)
		s.output = s.output[size:]
		s.mu.Unlock()
		s.wakeSpace()
		if len(data) > 0 {
			if err := writeFrame(port, frame{kind: dataKind, stream: a.stream, body: data}); err != nil {
				a.cancel()
				return
			}
			continue
		}
		if outputEnded {
			if err := writeFrame(port, frame{kind: finKind, stream: a.stream}); err != nil {
				a.cancel()
			}
			return
		}
		select {
		case <-a.ctx.Done():
			return
		case <-s.outputDone:
			// 收集结束后再检查缓冲，尾部字节全部写完才能发送 Fin。
			outputEnded = true
		case <-s.changed:
		}
	}
}
