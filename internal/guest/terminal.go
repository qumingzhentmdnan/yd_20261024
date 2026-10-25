package guest

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/protocol"
)

// attachment 拥有一次终端授权及其全部旧读写任务。
type attachment struct {
	// session 是授权所属工作负载。
	session *session
	// stream 在本连接内不可复用。
	stream protocol.StreamID
	// token 是三十二字节一次性随机值。
	token protocol.Nonce
	// state 由 boot.mu 保护。
	state protocol.AttachmentState
	// ctx 单独取消终端，不影响控制面。
	ctx context.Context
	// cancel 结束全部终端任务。
	cancel context.CancelFunc
	// mu 保护物理句柄发布和关闭。
	mu sync.Mutex
	// port 是当前已消费 Bind 的终端通道。
	port net.Conn
	// done 在输入输出任务全部退出后关闭。
	done chan struct{}
	// once 串行化最终释放。
	once sync.Once
	// released 在授权完全释放后关闭。
	released chan struct{}
}

// closePort 关闭已发布物理句柄，打断背压。
func (a *attachment) closePort() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.port != nil {
		a.port.Close()
	}
}

// attach 登记待绑定授权，成功响应完整写出后才开始绑定计时。
func (c *connection) attach(p protocol.AttachTerminalParams) (any, func(), error) {
	token, err := protocol.NewNonce()
	if err != nil {
		return nil, nil, err
	}
	b := c.boot
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.attachment != nil {
		return nil, nil, protocol.Failure(protocol.CodeFailedPrecondition)
	}
	if c.streams[p.StreamID] {
		return nil, nil, protocol.Failure(protocol.CodeConflict)
	}
	s := b.sessions[p.SessionID]
	if s == nil {
		return nil, nil, protocol.Failure(protocol.CodeNotFound)
	}
	ctx, cancel := context.WithCancel(c.ctx)
	a := &attachment{session: s, stream: p.StreamID, token: token, state: protocol.AttachmentStateAwaitingBind, ctx: ctx, cancel: cancel, done: make(chan struct{}), released: make(chan struct{})}
	c.streams[p.StreamID] = true
	c.attachment = a
	sent := func() {
		c.tasks.Add(1)
		go func() {
			defer c.tasks.Done()
			timer := time.NewTimer(guestwire.Budget)
			defer timer.Stop()
			select {
			case <-timer.C:
				b.mu.Lock()
				awaiting := c.attachment == a && a.state == protocol.AttachmentStateAwaitingBind
				b.mu.Unlock()
				if awaiting {
					c.detach(a)
				}
			case <-a.ctx.Done():
			}
		}()
	}
	return protocol.AttachTerminalResult{StreamID: p.StreamID, BindToken: token}, sent, nil
}

// detach 取消旧任务，等待设备关闭和全部旧循环结束后再释放授权。
func (c *connection) detach(a *attachment) {
	a.once.Do(func() {
		b := c.boot
		b.mu.Lock()
		a.state = protocol.AttachmentStateDetaching
		a.cancel()
		a.session.notifyLocked()
		b.mu.Unlock()
		a.mu.Lock()
		port := a.port
		if port != nil {
			port.Close()
		}
		a.mu.Unlock()
		if port != nil {
			<-a.done
		}
		b.mu.Lock()
		if c.attachment == a {
			c.attachment = nil
			c.released[a.stream] = a.session.info.SessionID
		}
		a.session.notifyLocked()
		b.mu.Unlock()
		close(a.released)
	})
	<-a.released
}

// acceptTerminals 只允许一个物理终端握手读者，坏流只失效 attachment。
func (c *connection) acceptTerminals() {
	defer c.tasks.Done()
	for {
		port, err := c.terminal.Accept()
		if err != nil {
			return
		}
		c.serveTerminal(port)
	}
}

// serveTerminal 在 Bound 完整写出后启动 Data 转发，保证旧循环不消费下一次 Bind。
func (c *connection) serveTerminal(port net.Conn) {
	// 空闲连接也只能占用一个绑定预算，防止无首字节占住终端 accept。
	stopControl := context.AfterFunc(c.ctx, func() { port.Close() })
	defer stopControl()
	timer := time.AfterFunc(guestwire.Budget, func() { port.Close() })
	frame, err := guestwire.Read(port, protocol.Terminal, protocol.HostToGuest)
	var bind protocol.Bind
	if err == nil {
		if frame.Kind != protocol.BindKind {
			err = protocol.ErrFrame
		} else {
			err = protocol.Decode(frame.Payload, &bind)
		}
	}
	b := c.boot
	b.mu.Lock()
	a := c.attachment
	valid := err == nil && a != nil && a.state == protocol.AttachmentStateAwaitingBind && a.ctx.Err() == nil && frame.StreamID == a.stream && bind.SessionID == a.session.info.SessionID && bind.Epoch == c.epoch && bind.ConnectionID == c.id && bind.BindToken == a.token
	if !valid {
		b.mu.Unlock()
		timer.Stop()
		port.Close()
		if a != nil {
			c.detach(a)
		}
		return
	}
	a.mu.Lock()
	a.port = port
	a.mu.Unlock()
	a.state = protocol.AttachmentStateBound
	dropped := a.session.dropped
	b.mu.Unlock()
	raw, err := protocol.Encode(protocol.Bound{DroppedBytes: dropped})
	if err == nil {
		err = guestwire.Write(port, protocol.Terminal, protocol.GuestToHost, protocol.Frame{Kind: protocol.BoundKind, StreamID: a.stream, Payload: raw}, time.Now().Add(guestwire.Budget))
	}
	timer.Stop()
	if err != nil {
		close(a.done)
		c.detach(a)
		return
	}
	var tasks sync.WaitGroup
	tasks.Add(2)
	go func() { defer tasks.Done(); c.input(a, port) }()
	go func() { defer tasks.Done(); c.output(a, port) }()
	tasks.Wait()
	close(a.done)
	c.detach(a)
}

// input 转发原始字节，Host Fin 只关闭输入方向而不注入 Ctrl+D。
func (c *connection) input(a *attachment, port net.Conn) {
	defer func() { a.cancel(); a.closePort(); c.boot.mu.Lock(); a.session.notifyLocked(); c.boot.mu.Unlock() }()
	fin := false
	for {
		f, err := guestwire.Read(port, protocol.Terminal, protocol.HostToGuest)
		if err != nil {
			return
		}
		if f.StreamID != a.stream {
			return
		}
		switch f.Kind {
		case protocol.DataKind:
			if fin {
				return
			}
			if err := c.writeInput(a, f.Payload); err != nil {
				return
			}
		case protocol.FinKind:
			if fin {
				return
			}
			fin = true
		case protocol.AbortKind:
			return
		default:
			return
		}
	}
}

// writeInput 在可取消的 PTY 使用资格内写完单帧，所有返回路径统一释放资格。
func (c *connection) writeInput(a *attachment, data []byte) error {
	s := a.session
	select {
	case s.ioMu <- struct{}{}:
	case <-a.ctx.Done():
		return a.ctx.Err()
	}
	defer func() { <-s.ioMu }()
	c.boot.mu.Lock()
	ended := s.outputDone
	c.boot.mu.Unlock()
	if ended {
		return io.ErrClosedPipe
	}
	for len(data) > 0 {
		n, err := s.process.Write(a.ctx, data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		data = data[n:]
	}
	return nil
}

// output 独立处理终端背压，输出完成且缓冲排空后才发送 Fin。
func (c *connection) output(a *attachment, port net.Conn) {
	b := c.boot
	s := a.session
	for {
		b.mu.Lock()
		if a.ctx.Err() != nil {
			b.mu.Unlock()
			return
		}
		if len(s.output) > 0 {
			n := len(s.output)
			if n > protocol.TerminalLimit {
				n = protocol.TerminalLimit
			}
			data := append([]byte(nil), s.output[:n]...)
			s.output = s.output[n:]
			s.notifyLocked()
			b.mu.Unlock()
			if err := guestwire.Write(port, protocol.Terminal, protocol.GuestToHost, protocol.Frame{Kind: protocol.DataKind, StreamID: a.stream, Payload: data}, time.Time{}); err != nil {
				a.cancel()
				a.closePort()
				return
			}
			continue
		}
		done := s.outputDone
		err := s.outputErr
		changed := s.changed
		b.mu.Unlock()
		if done {
			kind := protocol.FinKind
			var data []byte
			if err != nil {
				kind = protocol.AbortKind
				data, _ = protocol.Encode(protocol.Failure(protocol.CodeInternal))
			}
			if writeErr := guestwire.Write(port, protocol.Terminal, protocol.GuestToHost, protocol.Frame{Kind: kind, StreamID: a.stream, Payload: data}, time.Time{}); writeErr != nil {
				a.cancel()
				a.closePort()
			}
			return
		}
		select {
		case <-changed:
		case <-a.ctx.Done():
			return
		}
	}
}
