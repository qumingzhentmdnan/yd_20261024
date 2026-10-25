package fakeguest

import (
	"context"
	"io"
	"time"

	"safeagent/internal/host/guest"
	"safeagent/internal/protocol"
)

// terminal 使用 boot.mu 保护自身状态，不创建后台读写任务。
type terminal struct {
	// client 绑定授权所属连接。
	client *Client
	// session 指向共享工作负载，不随 detach 销毁。
	session *session
	// stream 是连接内单调分配的非零标识。
	stream protocol.StreamID
	// done 在本 attachment 被关闭时关闭。
	done chan struct{}
	// closed 阻止本授权再次使用。
	closed bool
	// inputClosed 在 Host Fin 后禁止继续 Write。
	inputClosed bool
	// writeToken 串行化输入帧与 CloseWrite，等待可被关闭或 context 中断。
	writeToken chan struct{}
	// closeWriteDone 在首次关闭尝试完成后关闭，空表示尚未尝试。
	closeWriteDone chan struct{}
	// closeWriteErr 保存首次关闭结果，完成后不可变。
	closeWriteErr error
}

var _ guest.Terminal = (*terminal)(nil)

// AttachTerminal 模拟控制授权与 Bind/Bound，返回时已经可以收发数据。
func (c *Client) AttachTerminal(ctx context.Context, id protocol.OpaqueID) (guest.Terminal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := protocol.Validate(id); err != nil {
		return nil, err
	}
	b := c.boot
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := c.checkLocked(true); err != nil {
		return nil, err
	}
	s, err := b.findLocked(id)
	if err != nil {
		return nil, err
	}
	if c.attachment != nil {
		return nil, protocol.Failure(protocol.CodeFailedPrecondition)
	}
	if c.stream == ^protocol.StreamID(0) {
		return nil, protocol.Failure(protocol.CodeResourceExhausted)
	}
	c.stream++
	a := &terminal{client: c, session: s, stream: c.stream, done: make(chan struct{}), writeToken: make(chan struct{}, 1)}
	a.writeToken <- struct{}{}
	c.attachment = a
	s.terminal = a
	s.notifyLocked()
	return a, nil
}

// closeLocked 同步释放旧授权并唤醒读写；返回后新 attachment 不会遇到旧消费者。
func (a *terminal) closeLocked() {
	if a.closed {
		return
	}
	a.closed = true
	close(a.done)
	a.client.attachment = nil
	a.session.terminal = nil
	a.session.notifyLocked()
}

// Close 只释放终端，两向读写结束后才能创建新授权，不停止工作负载。
func (a *terminal) Close() error {
	b := a.client.boot
	b.mu.Lock()
	defer b.mu.Unlock()
	a.closeLocked()
	return nil
}

// checkLocked 优先区分连接失效与本地授权关闭。
func (a *terminal) checkLocked() error {
	if err := a.client.checkLocked(true); err != nil {
		return err
	}
	if a.closed {
		return io.ErrClosedPipe
	}
	return nil
}

// Read 在有界输出缓冲中等待数据；主进程退出不自动等同于 Fin。
func (a *terminal) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b := a.client.boot
	for {
		b.mu.Lock()
		if err := a.checkLocked(); err != nil {
			b.mu.Unlock()
			return 0, err
		}
		s := a.session
		if len(s.output) > 0 {
			n := copy(p, s.output[:min(len(s.output), protocol.TerminalLimit)])
			s.output = s.output[n:]
			s.notifyLocked()
			b.mu.Unlock()
			return n, nil
		}
		if s.outputEnded {
			b.mu.Unlock()
			return 0, io.EOF
		}
		changed := s.changed
		b.mu.Unlock()
		select {
		case <-a.client.done:
			return 0, guest.ErrDisconnected
		case <-a.done:
			return 0, io.ErrClosedPipe
		case <-changed:
		}
	}
}

// Write 向最多 64 KiB 的测试输入队列发送原始字节，满时只阻塞本终端。
func (a *terminal) Write(p []byte) (int, error) {
	select {
	case <-a.client.done:
		return 0, guest.ErrDisconnected
	case <-a.done:
		return 0, io.ErrClosedPipe
	case <-a.writeToken:
	}
	defer func() { a.writeToken <- struct{}{} }()
	b := a.client.boot
	total := 0
	for total < len(p) {
		b.mu.Lock()
		if err := a.checkLocked(); err != nil {
			b.mu.Unlock()
			return total, err
		}
		if a.inputClosed {
			b.mu.Unlock()
			return total, guest.ErrInputClosed
		}
		s := a.session
		if s.info.GroupState == protocol.GroupStateEmpty {
			b.mu.Unlock()
			return total, io.ErrClosedPipe
		}
		size := min(len(p)-total, protocol.OutputLimit-len(s.input), protocol.TerminalLimit)
		if size > 0 {
			s.input = append(s.input, p[total:total+size]...)
			total += size
			s.notifyLocked()
			b.mu.Unlock()
			continue
		}
		changed := s.changed
		b.mu.Unlock()
		select {
		case <-a.client.done:
			return total, guest.ErrDisconnected
		case <-a.done:
			return total, io.ErrClosedPipe
		case <-changed:
		}
	}
	return total, nil
}

// CloseWrite 固定首次关闭尝试的结果；调用前已取消也算首次尝试，不注入 PTY 字符。
func (a *terminal) CloseWrite(ctx context.Context) error {
	b := a.client.boot
	b.mu.Lock()
	if done := a.closeWriteDone; done != nil {
		b.mu.Unlock()
		// 已完成时优先返回固定结果；等待中的其他调用可单独取消等待。
		select {
		case <-done:
			return a.closeWriteErr
		default:
		}
		select {
		case <-done:
			return a.closeWriteErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.closeWriteDone = make(chan struct{})
	b.mu.Unlock()
	err := a.finishCloseWrite(ctx)
	b.mu.Lock()
	a.closeWriteErr = err
	close(a.closeWriteDone)
	b.mu.Unlock()
	return err
}

// finishCloseWrite 等待先前输入完成后提交一次 Fin，失败时保持输入开放。
func (a *terminal) finishCloseWrite(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.client.done:
		return guest.ErrDisconnected
	case <-a.done:
		return io.ErrClosedPipe
	case <-a.writeToken:
	}
	defer func() { a.writeToken <- struct{}{} }()
	b := a.client.boot
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := a.checkLocked(); err != nil {
		return err
	}
	a.inputClosed = true
	a.session.notifyLocked()
	return nil
}

// Resize 修改实际模拟窗口，并拒绝已结束或不匹配的授权。
func (a *terminal) Resize(ctx context.Context, rows, cols protocol.Dimension) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := protocol.Validate(protocol.ResizeTerminalParams{SessionID: a.session.info.SessionID, StreamID: a.stream, Rows: rows, Cols: cols}); err != nil {
		return err
	}
	b := a.client.boot
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := a.checkLocked(); err != nil {
		return err
	}
	if a.session.info.GroupState == protocol.GroupStateEmpty {
		return protocol.Failure(protocol.CodeFailedPrecondition)
	}
	a.session.rows = rows
	a.session.cols = cols
	return nil
}

// FeedOutput 模拟 PTY 输出；已绑定时背压，未绑定时保留最近 64 KiB。
func (b *Boot) FeedOutput(ctx context.Context, id protocol.OpaqueID, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		b.mu.Lock()
		s, err := b.findLocked(id)
		if err != nil {
			b.mu.Unlock()
			return total, err
		}
		if s.outputEnded {
			b.mu.Unlock()
			return total, io.ErrClosedPipe
		}
		size := min(len(p)-total, protocol.TerminalLimit)
		if s.terminal != nil {
			size = min(size, protocol.OutputLimit-len(s.output))
		}
		if size > 0 {
			s.output = append(s.output, p[total:total+size]...)
			total += size
			if extra := len(s.output) - protocol.OutputLimit; extra > 0 {
				s.output = append([]byte(nil), s.output[extra:]...)
				s.dropped += protocol.Counter(extra)
			}
			s.notifyLocked()
			b.mu.Unlock()
			continue
		}
		changed := s.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-changed:
		}
	}
	return total, nil
}

// EndOutput 模拟 Guest 输出 Fin，保留缓冲尾部直到读取完毕。
func (b *Boot) EndOutput(id protocol.OpaqueID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.findLocked(id)
	if err != nil {
		return err
	}
	s.outputEnded = true
	s.notifyLocked()
	return nil
}

// TakeInput 取走已经接受的输入并释放背压，不假装 PTY 支持半关闭。
func (b *Boot) TakeInput(id protocol.OpaqueID) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.findLocked(id)
	if err != nil {
		return nil, err
	}
	data := append([]byte(nil), s.input...)
	s.input = nil
	s.notifyLocked()
	return data, nil
}

// DroppedBytes 返回未绑定输出环形缓冲的累计丢弃字节数。
func (b *Boot) DroppedBytes(id protocol.OpaqueID) (protocol.Counter, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.findLocked(id)
	if err != nil {
		return 0, err
	}
	return s.dropped, nil
}

// stopLocked 提交模拟进程组终止；主进程已退出时保留其原结果。
func (s *session) stopLocked(force bool) {
	if s.info.Exit == nil {
		signal, reason := "SIGTERM", protocol.ExitReasonStopped
		if force {
			signal, reason = "SIGKILL", protocol.ExitReasonForced
		}
		s.info.Exit = &protocol.SessionExit{SessionID: s.info.SessionID, Code: -1, Signal: signal, Reason: reason}
		s.info.State = protocol.SessionStateExited
	}
	s.info.GroupState = protocol.GroupStateEmpty
	s.outputEnded = true
	s.notifyLocked()
}

// StopSession 模拟 TERM、宽限期与显式 KILL，不依赖终端读取推进。
func (c *Client) StopSession(ctx context.Context, p protocol.StopSessionParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := protocol.Validate(p); err != nil {
		return err
	}
	b := c.boot
	b.mu.Lock()
	if err := c.checkLocked(true); err != nil {
		b.mu.Unlock()
		return err
	}
	s, err := b.findLocked(p.SessionID)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	if s.info.GroupState == protocol.GroupStateEmpty {
		b.mu.Unlock()
		return nil
	}
	if !s.ignoreTERM {
		s.stopLocked(false)
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()
	timer := time.NewTimer(time.Duration(p.GraceMs) * time.Millisecond)
	defer timer.Stop()
	for {
		b.mu.Lock()
		if err := c.checkLocked(true); err != nil {
			b.mu.Unlock()
			return err
		}
		if s.info.GroupState == protocol.GroupStateEmpty {
			b.mu.Unlock()
			return nil
		}
		changed := s.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return guest.ErrDisconnected
		case <-changed:
		case <-timer.C:
			b.mu.Lock()
			if err := ctx.Err(); err != nil {
				b.mu.Unlock()
				return err
			}
			if err := c.checkLocked(true); err != nil {
				b.mu.Unlock()
				return err
			}
			if s.info.GroupState == protocol.GroupStateEmpty {
				b.mu.Unlock()
				return nil
			}
			if !p.ForceAfterTimeout {
				b.mu.Unlock()
				return protocol.Failure(protocol.CodeDeadlineExceeded)
			}
			s.stopLocked(true)
			b.mu.Unlock()
			return nil
		}
	}
}
