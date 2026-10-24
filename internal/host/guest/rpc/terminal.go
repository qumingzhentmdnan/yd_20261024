package rpc

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/host/guest"
	"safeagent/internal/protocol"
)

// Terminal 拥有单次已完成 Bind/Bound 的授权。
type Terminal struct {
	// client 绑定当前控制连接。
	client *Client
	// session 是本次授权的工作负载。
	session protocol.OpaqueID
	// stream 在连接内不复用。
	stream protocol.StreamID
	// port 是当前 attachment 的物理连接。
	port net.Conn
	// mu 保护有界输出及结束状态。
	mu sync.Mutex
	// output 最多保存一个 Data 帧。
	output []byte
	// changed 广播输出状态变化。
	changed chan struct{}
	// end 固定输出终止错误，正常 Fin 为 EOF。
	end error
	// write 串行化 Data 与 Host Fin。
	write chan struct{}
	// finDone 在首次 CloseWrite 尝试完成后关闭。
	finDone chan struct{}
	// finStarted 表示第一次 CloseWrite 已被接纳。
	finStarted bool
	// finErr 保存首次 CloseWrite 结果。
	finErr error
	// closeOnce 固定唯一清理任务。
	closeOnce sync.Once
	// closeErr 保存首次清理结果。
	closeErr error
	// readerDone 在终端读循环退出时关闭。
	readerDone chan struct{}
	// bound 表示初始化已完成，只有此时才能启动排空任务。
	bound bool
	// drainOnce 每次授权只启动一次退出排空计时。
	drainOnce sync.Once
}

var _ guest.Terminal = (*Terminal)(nil)

// AttachTerminal 登记流、发送 Bind 并等待 Bound，失败时有界撤销授权。
func (c *Client) AttachTerminal(ctx context.Context, id protocol.OpaqueID) (guest.Terminal, error) {
	c.mu.Lock()
	if c.terminal != nil {
		c.mu.Unlock()
		return nil, protocol.Failure(protocol.CodeFailedPrecondition)
	}
	c.stream++
	stream := c.stream
	t := &Terminal{client: c, session: id, stream: stream, changed: make(chan struct{}), write: make(chan struct{}, 1), finDone: make(chan struct{}), readerDone: make(chan struct{})}
	c.terminal = t
	c.mu.Unlock()
	var result protocol.AttachTerminalResult
	if err := c.call(ctx, protocol.MethodAttachTerminal, protocol.AttachTerminalParams{SessionID: id, StreamID: stream}, &result); err != nil {
		c.mu.Lock()
		if c.terminal == t {
			c.terminal = nil
		}
		c.mu.Unlock()
		return nil, err
	}
	fail := func(err error) (guest.Terminal, error) {
		if t.port != nil {
			t.port.Close()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), guestwire.Budget)
		defer cancel()
		detachErr := c.call(cleanup, protocol.MethodDetachTerminal, protocol.DetachTerminalParams{SessionID: id, StreamID: stream}, &protocol.Empty{})
		if detachErr != nil {
			c.fail(detachErr)
		}
		c.mu.Lock()
		if c.terminal == t {
			c.terminal = nil
		}
		c.mu.Unlock()
		return nil, err
	}
	if result.StreamID != stream {
		return fail(protocol.ErrFrame)
	}
	port, err := c.open(ctx)
	if err != nil {
		return fail(err)
	}
	t.mu.Lock()
	t.port = port
	deadBeforeBind := t.end != nil
	t.mu.Unlock()
	if deadBeforeBind {
		return fail(guest.ErrDisconnected)
	}
	// 从 Bind 开始等待 Bound；调用取消主动关闭端口，避免空闲 Read 无法退出。
	handshake, cancel := context.WithDeadline(ctx, guestwire.Deadline(ctx))
	defer cancel()
	stop := context.AfterFunc(handshake, func() { port.Close() })
	defer stop()
	raw, err := protocol.Encode(protocol.Bind{SessionID: id, Epoch: protocol.Epoch(c.instance.Epoch), ConnectionID: c.id, BindToken: result.BindToken})
	if err != nil {
		return fail(err)
	}
	if err = guestwire.Write(port, protocol.Terminal, protocol.HostToGuest, protocol.Frame{Kind: protocol.BindKind, StreamID: stream, Payload: raw}, guestwire.Deadline(handshake)); err != nil {
		return fail(err)
	}
	frame, err := guestwire.Read(port, protocol.Terminal, protocol.GuestToHost)
	if err != nil {
		return fail(err)
	}
	var bound protocol.Bound
	if frame.Kind != protocol.BoundKind || frame.StreamID != stream {
		return fail(protocol.ErrFrame)
	}
	if err = protocol.Decode(frame.Payload, &bound); err != nil {
		return fail(err)
	}
	if !stop() && handshake.Err() != nil {
		return fail(handshake.Err())
	}
	t.mu.Lock()
	t.bound = true
	t.mu.Unlock()
	go t.readLoop()
	c.mu.Lock()
	_, exited := c.exits[id]
	dead := c.ctx.Err() != nil
	c.mu.Unlock()
	if exited {
		t.drain()
	}
	if dead {
		t.abort(guest.ErrDisconnected)
	}
	return t, nil
}

// notifyLocked 唤醒输出生产者和消费者，调用方持有 mu。
func (t *Terminal) notifyLocked() { close(t.changed); t.changed = make(chan struct{}) }

// abort 固定异常输出状态并关闭物理端口，唤醒阻塞的读写。
func (t *Terminal) abort(err error) {
	t.mu.Lock()
	if t.end == nil {
		t.end = err
	}
	t.notifyLocked()
	p := t.port
	t.mu.Unlock()
	if p != nil {
		p.Close()
	}
}

// readLoop 读取单个待交付帧，慢读只阻塞终端自身。
func (t *Terminal) readLoop() {
	defer close(t.readerDone)
	for {
		t.mu.Lock()
		for len(t.output) > 0 && t.end == nil {
			changed := t.changed
			t.mu.Unlock()
			<-changed
			t.mu.Lock()
		}
		ended := t.end != nil
		t.mu.Unlock()
		if ended {
			return
		}
		f, err := guestwire.Read(t.port, protocol.Terminal, protocol.GuestToHost)
		if err != nil {
			t.abort(guest.ErrOutputIncomplete)
			return
		}
		if f.StreamID != t.stream {
			t.abort(protocol.ErrFrame)
			return
		}
		switch f.Kind {
		case protocol.DataKind:
			t.mu.Lock()
			t.output = f.Payload
			t.notifyLocked()
			t.mu.Unlock()
		case protocol.FinKind:
			t.mu.Lock()
			if t.end == nil {
				t.end = io.EOF
			}
			t.notifyLocked()
			t.mu.Unlock()
			return
		case protocol.AbortKind:
			t.abort(guest.ErrOutputIncomplete)
			return
		default:
			t.abort(protocol.ErrFrame)
			return
		}
	}
}

// drain 在已知退出后限制等待尾部 Fin 的时间，业务不 Read 时也生效。
func (t *Terminal) drain() {
	t.mu.Lock()
	bound := t.bound
	t.mu.Unlock()
	if !bound {
		return
	}
	t.drainOnce.Do(func() {
		go func() {
			timer := time.NewTimer(guestwire.Budget)
			defer timer.Stop()
			select {
			case <-t.readerDone:
				return
			case <-timer.C:
				t.abort(guest.ErrOutputIncomplete)
				t.Close()
			}
		}()
	})
}

// Read 返回原始字节，只有完整 Guest Fin 才返回 EOF。
func (t *Terminal) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		if len(t.output) > 0 {
			n := copy(p, t.output)
			t.output = t.output[n:]
			t.notifyLocked()
			return n, nil
		}
		if t.end != nil {
			return 0, t.end
		}
		changed := t.changed
		t.mu.Unlock()
		<-changed
		t.mu.Lock()
	}
}

// Write 串行化所有输入帧，最多一个待写 Data 帧。
func (t *Terminal) Write(p []byte) (int, error) {
	select {
	case t.write <- struct{}{}:
	case <-t.client.ctx.Done():
		return 0, guest.ErrDisconnected
	}
	defer func() { <-t.write }()
	t.mu.Lock()
	closed := t.finStarted
	end := t.end
	t.mu.Unlock()
	if closed {
		return 0, guest.ErrInputClosed
	}
	if end != nil {
		return 0, end
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > protocol.TerminalLimit {
			n = protocol.TerminalLimit
		}
		if err := guestwire.Write(t.port, protocol.Terminal, protocol.HostToGuest, protocol.Frame{Kind: protocol.DataKind, StreamID: t.stream, Payload: p[:n]}, time.Time{}); err != nil {
			t.abort(guest.ErrOutputIncomplete)
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

// Resize 通过独立控制面调整当前 PTY。
func (t *Terminal) Resize(ctx context.Context, rows, cols protocol.Dimension) error {
	return t.client.call(ctx, protocol.MethodResizeTerminal, protocol.ResizeTerminalParams{SessionID: t.session, StreamID: t.stream, Rows: rows, Cols: cols}, &protocol.Empty{})
}

// CloseWrite 固定首次 Fin 发送结果；其他并发等待者可单独取消。
func (t *Terminal) CloseWrite(ctx context.Context) error {
	t.mu.Lock()
	if t.finStarted {
		done := t.finDone
		t.mu.Unlock()
		select {
		case <-done:
			return t.finErr
		default:
		}
		select {
		case <-done:
			return t.finErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.finStarted = true
	t.mu.Unlock()
	err := ctx.Err()
	if err == nil {
		select {
		case t.write <- struct{}{}:
			err = t.writeFin(ctx)
			<-t.write
		case <-ctx.Done():
			err = ctx.Err()
		case <-t.client.ctx.Done():
			err = guest.ErrDisconnected
		}
	}
	t.mu.Lock()
	t.finErr = err
	close(t.finDone)
	t.mu.Unlock()
	return err
}

// writeFin 在输入写资格内发送唯一 Fin，只有已发送部分帧的失败才中止输出。
func (t *Terminal) writeFin(ctx context.Context) error {
	done := make(chan struct{})
	t.port.SetWriteDeadline(guestwire.Deadline(ctx))
	stop := context.AfterFunc(ctx, func() { defer close(done); t.port.SetWriteDeadline(time.Now()) })
	counter := &countWriter{port: t.port}
	err := protocol.WriteFrame(counter, protocol.Terminal, protocol.HostToGuest, protocol.Frame{Kind: protocol.FinKind, StreamID: t.stream})
	if !stop() {
		<-done
	}
	t.port.SetWriteDeadline(time.Time{})
	if err != nil && counter.written > 0 {
		t.abort(guest.ErrOutputIncomplete)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if deadline, ok := ctx.Deadline(); ok && err != nil && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return err
}

// Close 等待有界 Detach 及旧本地读循环退出，不停止进程。
func (t *Terminal) Close() error {
	t.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), guestwire.Budget)
		defer cancel()
		t.abort(guest.ErrDisconnected)
		t.closeErr = t.client.callWithCancellation(ctx, protocol.MethodDetachTerminal, protocol.DetachTerminalParams{SessionID: t.session, StreamID: t.stream}, &protocol.Empty{}, false)
		<-t.readerDone
		if t.closeErr != nil {
			t.client.fail(t.closeErr)
		}
		t.client.mu.Lock()
		if t.client.terminal == t {
			t.client.terminal = nil
		}
		t.client.mu.Unlock()
	})
	return t.closeErr
}

// countWriter 区分 Fin 发送前取消与已发送部分头的不可恢复失败。
type countWriter struct {
	// port 是串行输入写者独占的连接。
	port net.Conn
	// written 累计本帧已经发送的字节数。
	written int
}

// Write 记录底层实际接受的字节数。
func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.port.Write(p)
	w.written += n
	return n, err
}
