package guest

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/protocol"
)

// outgoing 是容量固定的控制写队列项。
type outgoing struct {
	// frame 是经过公共编码器验证的帧。
	frame protocol.Frame
	// sent 在完整写出后启动 Bind 时限，空表示不需要确认。
	sent func()
}

// connection 仅拥有本代请求及授权，不拥有 boot 中的工作负载。
type connection struct {
	// boot 保存跨连接的状态。
	boot *Boot
	// port 是独立控制通道。
	port net.Conn
	// terminal 只接受本连接授权的终端。
	terminal net.Listener
	// ctx 在任一控制错误时失效。
	ctx context.Context
	// cancel 取消所有当前请求。
	cancel context.CancelFunc
	// once 串行化连接关闭。
	once sync.Once
	// mu 保护握手、请求取消表及事件序号。
	mu sync.Mutex
	// hello 表示已成功握手。
	hello bool
	// id 是握手固定的连接身份。
	id protocol.UUID
	// epoch 是握手固定的 VM 代次。
	epoch protocol.Epoch
	// last 是最大已接收请求号。
	last protocol.Sequence
	// count 是非 GetState 历史请求数，与 requestID 数值独立。
	count uint32
	// eventID 是已分配事件号。
	eventID protocol.Sequence
	// pending 保存最多十六个有界业务任务。
	pending map[protocol.Sequence]context.CancelFunc
	// outgoing 串行化响应和事件，容量为三十二。
	outgoing chan outgoing
	// tasks 等待 writer、accept 及所有请求退出。
	tasks sync.WaitGroup
	// attachment 由 boot.mu 保护，空表示本连接无授权。
	attachment *attachment
	// streams 保留所有已使用流号，由 boot.mu 保护。
	streams map[protocol.StreamID]bool
	// released 保存已释放授权，用于幂等 Detach。
	released map[protocol.StreamID]protocol.OpaqueID
}

// newConnection 分配本连接的有界请求和响应资源。
func newConnection(b *Boot, port net.Conn, terminal net.Listener) *connection {
	ctx, cancel := context.WithCancel(b.ctx)
	return &connection{boot: b, port: port, terminal: terminal, ctx: ctx, cancel: cancel, pending: make(map[protocol.Sequence]context.CancelFunc), outgoing: make(chan outgoing, 32), streams: make(map[protocol.StreamID]bool), released: make(map[protocol.StreamID]protocol.OpaqueID)}
}

// close 失效连接并打断物理读写，不停止工作负载。
func (c *connection) close() {
	c.once.Do(func() {
		c.cancel()
		c.port.Close()
		c.terminal.Close()
		c.boot.mu.Lock()
		a := c.attachment
		c.boot.mu.Unlock()
		if a != nil {
			a.cancel()
			a.closePort()
		}
	})
}

// enqueue 有界入队，队列饱和或连接失败不派生写任务。
func (c *connection) enqueue(item outgoing) error {
	timer := time.NewTimer(guestwire.Budget)
	defer timer.Stop()
	select {
	case c.outgoing <- item:
		return nil
	case <-c.ctx.Done():
		return c.ctx.Err()
	case <-timer.C:
		c.close()
		return context.DeadlineExceeded
	}
}

// response 严格关联原方法编码，错误不能伪造成功结果。
func (c *connection) response(r protocol.Request, result any, err error, sent func()) error {
	response := protocol.Response{RequestID: r.RequestID, Epoch: r.Epoch, ConnectionID: r.ConnectionID}
	if err != nil {
		response.Error = fixedError(err)
	} else {
		var encodeErr error
		response.Result, encodeErr = protocol.Encode(result)
		if encodeErr != nil {
			return encodeErr
		}
	}
	data, encodeErr := protocol.EncodeResponse(response, r)
	if encodeErr != nil {
		return encodeErr
	}
	return c.enqueue(outgoing{frame: protocol.Frame{Kind: protocol.ResponseKind, Payload: data}, sent: sent})
}

// writer 唯一控制写者，实际写预算与入队预算分别计时。
func (c *connection) writer() {
	defer c.tasks.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case item := <-c.outgoing:
			if err := guestwire.Write(c.port, protocol.Control, protocol.GuestToHost, item.frame, time.Now().Add(guestwire.Budget)); err != nil {
				c.close()
				return
			}
			if item.sent != nil {
				item.sent()
			}
		}
	}
}

// event 在握手完成后串行分配并入队退出事件。
func (c *connection) event(exit protocol.SessionExit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hello || c.ctx.Err() != nil {
		return
	}
	c.eventID++
	data, err := protocol.Encode(protocol.SessionExitedEvent{Epoch: c.epoch, ConnectionID: c.id, EventID: c.eventID, Event: "SessionExited", Params: exit})
	if err != nil {
		c.close()
		return
	}
	if err = c.enqueue(outgoing{frame: protocol.Frame{Kind: protocol.EventKind, Payload: data}}); err != nil {
		c.close()
	}
}

// handshake 固定 boot 身份与本连接身份，必须在控制读循环内串行执行。
func (c *connection) handshake(r protocol.Request, p protocol.HelloParams) (protocol.HelloResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hello {
		return protocol.HelloResult{}, protocol.Failure(protocol.CodeFailedPrecondition)
	}
	b := c.boot
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.epoch != "" && (b.epoch != r.Epoch || b.ref != p.VMRef) {
		return protocol.HelloResult{}, protocol.Failure(protocol.CodeStaleInstance)
	}
	b.epoch = r.Epoch
	b.ref = p.VMRef
	c.epoch = r.Epoch
	c.id = r.ConnectionID
	c.hello = true
	return protocol.HelloResult{Version: "0.1", Challenge: p.Challenge, GuestBuild: b.config.Build, ImageDigest: b.config.ImageDigest}, nil
}

// run 验证 envelope 和身份后分派有界任务，控制读者从不等待进程退出。
func (c *connection) run() error {
	c.tasks.Add(2)
	go c.writer()
	go c.acceptTerminals()
	defer func() {
		c.close()
		c.boot.mu.Lock()
		a := c.attachment
		c.boot.mu.Unlock()
		if a != nil {
			c.detach(a)
		}
	}()
	for {
		frame, err := guestwire.Read(c.port, protocol.Control, protocol.HostToGuest)
		if err != nil {
			return err
		}
		if frame.Kind == protocol.CancelKind {
			var cancel protocol.Cancel
			if err = protocol.Decode(frame.Payload, &cancel); err != nil {
				return err
			}
			c.mu.Lock()
			if !c.hello || cancel.Epoch != c.epoch || cancel.ConnectionID != c.id {
				c.mu.Unlock()
				return protocol.ErrFrame
			}
			stop := c.pending[cancel.RequestID]
			c.mu.Unlock()
			if stop != nil {
				stop()
			}
			continue
		}
		r, err := protocol.DecodeRequestEnvelope(frame.Payload)
		if err != nil {
			return err
		}
		c.mu.Lock()
		if r.RequestID <= c.last {
			c.mu.Unlock()
			return protocol.ErrFrame
		}
		c.last = r.RequestID
		if r.Method != protocol.MethodGetState {
			c.count++
		}
		hello := c.hello
		stale := hello && (r.Epoch != c.epoch || r.ConnectionID != c.id)
		c.mu.Unlock()
		if c.count > protocol.MaxRequests || stale {
			return protocol.ErrFrame
		}
		if !hello && r.Method != protocol.MethodHello {
			if err = c.response(r, nil, protocol.Failure(protocol.CodeFailedPrecondition), nil); err != nil {
				return err
			}
			continue
		}
		params, err := r.DecodeParams()
		if err != nil {
			var business *protocol.Error
			if !errors.As(err, &business) {
				err = protocol.Failure(protocol.CodeInvalidArgument)
			}
			if err = c.response(r, nil, err, nil); err != nil {
				return err
			}
			continue
		}
		if r.Method == protocol.MethodHello {
			result, problem := c.handshake(r, *params.(*protocol.HelloParams))
			var e *protocol.Error
			if errors.As(problem, &e) && e.Code == protocol.CodeStaleInstance {
				// 先尽力写出拒绝，再失效；不允许旧身份继续服务。
				done := make(chan struct{})
				if err = c.response(r, nil, problem, func() { close(done) }); err != nil {
					return err
				}
				select {
				case <-done:
				case <-c.ctx.Done():
				}
				return problem
			}
			if err = c.response(r, result, problem, nil); err != nil {
				return err
			}
			continue
		}
		ctx, cancel := context.WithTimeout(c.ctx, time.Duration(r.TimeoutMs)*time.Millisecond)
		c.mu.Lock()
		if len(c.pending) >= protocol.MaxPending {
			c.mu.Unlock()
			cancel()
			if err = c.response(r, nil, protocol.Failure(protocol.CodeResourceExhausted), nil); err != nil {
				return err
			}
			continue
		}
		c.pending[r.RequestID] = cancel
		c.mu.Unlock()
		c.tasks.Add(1)
		go func() {
			defer c.tasks.Done()
			defer cancel()
			result, sent, err := c.execute(ctx, r.Method, params)
			if sendErr := c.response(r, result, err, sent); sendErr != nil {
				c.close()
			}
			c.mu.Lock()
			delete(c.pending, r.RequestID)
			c.mu.Unlock()
		}()
	}
}

// execute 执行精确方法参数；Stop 等待在有界请求任务中进行。
func (c *connection) execute(ctx context.Context, method protocol.Method, params any) (any, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	switch method {
	case protocol.MethodListChanges, protocol.MethodReadChangeChunk:
		v, err := c.exportWorkspace(ctx, params)
		return v, nil, err
	case protocol.MethodGetState:
		return c.state(), nil, nil
	case protocol.MethodPrepareAgent:
		v, err := c.operate(ctx, method, *params.(*protocol.PrepareAgentParams))
		return v, nil, err
	case protocol.MethodStartSession:
		v, err := c.operate(ctx, method, *params.(*protocol.StartSessionParams))
		return v, nil, err
	case protocol.MethodAttachTerminal:
		return c.attach(*params.(*protocol.AttachTerminalParams))
	case protocol.MethodResizeTerminal:
		p := params.(*protocol.ResizeTerminalParams)
		b := c.boot
		b.mu.Lock()
		a := c.attachment
		if a == nil || a.session.info.SessionID != p.SessionID || a.stream != p.StreamID {
			b.mu.Unlock()
			return nil, nil, protocol.Failure(protocol.CodeNotFound)
		}
		s := a.session
		b.mu.Unlock()
		select {
		case s.ioMu <- struct{}{}:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		b.mu.Lock()
		ended := s.outputDone
		valid := c.attachment == a && a.state == protocol.AttachmentStateBound && a.ctx.Err() == nil
		if ctx.Err() != nil {
			b.mu.Unlock()
			<-s.ioMu
			return nil, nil, ctx.Err()
		}
		defer func() { <-s.ioMu }()
		if ended || !valid {
			b.mu.Unlock()
			return nil, nil, protocol.Failure(protocol.CodeFailedPrecondition)
		}
		// ioctl 是短系统调用，最终授权复核与应用尺寸共用状态边界，Detach 不能穿过。
		err := s.process.Resize(p.Rows, p.Cols)
		b.mu.Unlock()
		return protocol.Empty{}, nil, err
	case protocol.MethodDetachTerminal:
		p := params.(*protocol.DetachTerminalParams)
		b := c.boot
		b.mu.Lock()
		a := c.attachment
		if a == nil {
			released := c.released[p.StreamID] == p.SessionID
			b.mu.Unlock()
			if released {
				return protocol.Empty{}, nil, nil
			}
			return nil, nil, protocol.Failure(protocol.CodeNotFound)
		}
		if a.session.info.SessionID != p.SessionID || a.stream != p.StreamID {
			b.mu.Unlock()
			return nil, nil, protocol.Failure(protocol.CodeNotFound)
		}
		b.mu.Unlock()
		c.detach(a)
		return protocol.Empty{}, nil, nil
	case protocol.MethodStopSession:
		p := params.(*protocol.StopSessionParams)
		b := c.boot
		b.mu.Lock()
		s := b.sessions[p.SessionID]
		b.mu.Unlock()
		if s == nil {
			return nil, nil, protocol.Failure(protocol.CodeNotFound)
		}
		err := s.process.Stop(ctx, time.Duration(p.GraceMs)*time.Millisecond, p.ForceAfterTimeout)
		if err == nil {
			select {
			case <-s.exitCommitted:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			b.mu.Lock()
			s.info.GroupState = protocol.GroupStateEmpty
			b.mu.Unlock()
		}
		return protocol.Empty{}, nil, err
	}
	return nil, nil, protocol.Failure(protocol.CodeUnimplemented)
}
