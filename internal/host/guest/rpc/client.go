// Package rpc 实现绑定可信 VMInstance 的真实 Guest 控制客户端。
package rpc

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
)

// OpenTerminal 打开当前可信实例的独立终端通道。
type OpenTerminal func(context.Context) (net.Conn, error)

// pending 保存已发请求及固定响应状态，连接内不淘汰。
type pending struct {
	// request 用于严格验证迟到响应的方法及身份。
	request protocol.Request
	// done 只交付一次响应。
	done chan protocol.Response
	// received 标识第一个响应已经消费。
	received bool
}

// Client 管理唯一控制读循环与有界请求账本。
type Client struct {
	// instance 由可信装配层提供，不接受业务覆盖。
	instance vm.VMInstance
	// id 标识本逻辑连接。
	id protocol.UUID
	// control 是独立的控制物理连接。
	control net.Conn
	// open 打开独立终端端口。
	open OpenTerminal
	// ctx 随连接失效而取消。
	ctx context.Context
	// cancel 结束连接任务。
	cancel context.CancelFunc
	// write 串行化序号分配、请求及取消帧。
	write chan struct{}
	// slots 限制在途请求为十六个。
	slots chan struct{}
	// mu 保护账本和不可逆退出状态。
	mu sync.Mutex
	// requests 保留本连接全部已发送请求。
	requests map[protocol.Sequence]*pending
	// next 是最近已分配请求序号。
	next protocol.Sequence
	// stream 是最近已分配终端流号。
	stream protocol.StreamID
	// event 是最近接收事件序号。
	event protocol.Sequence
	// exits 保存固定主进程退出结果。
	exits map[protocol.OpaqueID]protocol.SessionExit
	// changed 在退出状态改变后广播。
	changed chan struct{}
	// terminal 是当前本地授权，空表示没有授权。
	terminal *Terminal
	// failure 保存连接首次失败。
	failure error
	// readerDone 在唯一读循环退出后关闭。
	readerDone chan struct{}
	// closeOnce 保证连接只关闭一次。
	closeOnce sync.Once
}

var _ guest.Client = (*Client)(nil)

// New 接管可信装配层校验镜像后交付的连接，开始读取但不自动 Hello。
func New(instance vm.VMInstance, control net.Conn, open OpenTerminal) (*Client, error) {
	if err := instance.Validate(); err != nil {
		return nil, err
	}
	if err := protocol.Validate(protocol.Epoch(instance.Epoch)); err != nil {
		return nil, err
	}
	id, err := protocol.NewUUID()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{instance: instance, id: id, control: control, open: open, ctx: ctx, cancel: cancel, write: make(chan struct{}, 1), slots: make(chan struct{}, protocol.MaxPending), requests: make(map[protocol.Sequence]*pending), exits: make(map[protocol.OpaqueID]protocol.SessionExit), changed: make(chan struct{}), readerDone: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

// fail 固定连接错误并关闭全部本地物理句柄，不停止工作负载。
func (c *Client) fail(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.failure = err
		t := c.terminal
		c.mu.Unlock()
		c.cancel()
		c.control.Close()
		if t != nil {
			t.abort(guest.ErrDisconnected)
		}
	})
}

// disconnected 返回可辨认的本地断线错误。
func (c *Client) disconnected() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return errors.Join(guest.ErrDisconnected, c.failure)
}

// readLoop 唯一读取响应及事件，拒绝未知、重复和身份不匹配的帧。
func (c *Client) readLoop() {
	defer close(c.readerDone)
	for {
		f, err := guestwire.Read(c.control, protocol.Control, protocol.GuestToHost)
		if err != nil {
			c.fail(err)
			return
		}
		switch f.Kind {
		case protocol.ResponseKind:
			var envelope protocol.Response
			if err = protocol.Decode(f.Payload, &envelope); err != nil {
				c.fail(err)
				return
			}
			c.mu.Lock()
			p := c.requests[envelope.RequestID]
			duplicate := p != nil && p.received
			c.mu.Unlock()
			if p == nil || duplicate {
				c.fail(protocol.ErrFrame)
				return
			}
			r, err := protocol.DecodeResponse(f.Payload, p.request)
			if err != nil {
				c.fail(err)
				return
			}
			if p.request.Method == protocol.MethodGetState && r.Error == nil {
				var state protocol.GuestState
				if err = protocol.Decode(r.Result, &state); err != nil {
					c.fail(err)
					return
				}
				for _, s := range state.Sessions {
					if s.Exit != nil {
						if err = c.mergeExit(*s.Exit); err != nil {
							c.fail(err)
							return
						}
					}
				}
			}
			c.mu.Lock()
			p.received = true
			c.mu.Unlock()
			p.done <- r
		case protocol.EventKind:
			var event protocol.SessionExitedEvent
			if err = protocol.Decode(f.Payload, &event); err != nil {
				c.fail(err)
				return
			}
			if event.Epoch != protocol.Epoch(c.instance.Epoch) || event.ConnectionID != c.id || event.EventID <= c.event {
				c.fail(protocol.ErrFrame)
				return
			}
			c.event = event.EventID
			if err = c.mergeExit(event.Params); err != nil {
				c.fail(err)
				return
			}
		}
	}
}

// mergeExit 合并不可逆退出状态，并独立启动当前终端的尾部排空预算。
func (c *Client) mergeExit(exit protocol.SessionExit) error {
	c.mu.Lock()
	if old, ok := c.exits[exit.SessionID]; ok {
		c.mu.Unlock()
		if old != exit {
			return protocol.ErrFrame
		}
		return nil
	}
	c.exits[exit.SessionID] = exit
	close(c.changed)
	c.changed = make(chan struct{})
	t := c.terminal
	c.mu.Unlock()
	if t != nil && t.session == exit.SessionID {
		t.drain()
	}
	return nil
}

// call 以严格序号写请求；取消后保留账本供迟到响应验证。
func (c *Client) call(ctx context.Context, method protocol.Method, params any, result any) error {
	return c.callWithCancellation(ctx, method, params, result, true)
}

// callWithCancellation 显式区分普通请求取消与终端清理到期即失效连接的策略。
func (c *Client) callWithCancellation(ctx context.Context, method protocol.Method, params any, result any, sendCancel bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.disconnected()
	}
	defer func() { <-c.slots }()
	p, err := c.sendRequest(ctx, method, params)
	if err != nil {
		return err
	}
	select {
	case response := <-p.done:
		if response.Error != nil {
			return response.Error
		}
		return protocol.Decode(response.Result, result)
	case <-ctx.Done():
		// Cancel 由当前调用有界发送，避免每次取消产生无上限后台写任务。
		if !sendCancel {
			c.fail(ctx.Err())
		} else {
			c.sendCancel(p.request.RequestID)
		}
		return ctx.Err()
	case <-c.ctx.Done():
		return c.disconnected()
	}
}

// sendRequest 串行编码、登记和发送请求，离开本阶段即释放写资格。
func (c *Client) sendRequest(ctx context.Context, method protocol.Method, params any) (*pending, error) {
	select {
	case c.write <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.disconnected()
	}
	defer func() { <-c.write }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.next >= protocol.MaxRequests {
		c.mu.Unlock()
		return nil, protocol.Failure(protocol.CodeResourceExhausted)
	}
	id := c.next + 1
	c.mu.Unlock()
	timeout := 30 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			return nil, context.DeadlineExceeded
		}
		if timeout > 30*time.Second {
			timeout = 30 * time.Second
		}
	}
	ms := (timeout + time.Millisecond - 1) / time.Millisecond
	raw, err := protocol.Encode(params)
	if err != nil {
		return nil, err
	}
	r := protocol.Request{RequestID: id, Epoch: protocol.Epoch(c.instance.Epoch), ConnectionID: c.id, Method: method, TimeoutMs: protocol.Millis(ms), Params: raw}
	data, err := protocol.EncodeRequest(r)
	if err != nil {
		return nil, err
	}
	p := &pending{request: r, done: make(chan protocol.Response, 1)}
	c.mu.Lock()
	c.next = id
	c.requests[id] = p
	c.mu.Unlock()
	err = guestwire.Write(c.control, protocol.Control, protocol.HostToGuest, protocol.Frame{Kind: protocol.RequestKind, Payload: data}, guestwire.Deadline(ctx))
	if err != nil {
		c.fail(err)
		return nil, c.disconnected()
	}

	return p, nil
}

// sendCancel 在原请求写出后串行发送取消，预算超时使连接失效。
func (c *Client) sendCancel(id protocol.Sequence) {
	timer := time.NewTimer(guestwire.Budget)
	defer timer.Stop()
	select {
	case c.write <- struct{}{}:
	case <-c.ctx.Done():
		return
	case <-timer.C:
		c.fail(context.DeadlineExceeded)
		return
	}
	defer func() { <-c.write }()
	data, err := protocol.Encode(protocol.Cancel{RequestID: id, Epoch: protocol.Epoch(c.instance.Epoch), ConnectionID: c.id})
	if err == nil {
		err = guestwire.Write(c.control, protocol.Control, protocol.HostToGuest, protocol.Frame{Kind: protocol.CancelKind, Payload: data}, time.Now().Add(guestwire.Budget))
	}
	if err != nil {
		c.fail(err)
	}
}

// Hello 校验 challenge/version 后查询并合并 boot 状态。
func (c *Client) Hello(ctx context.Context) (protocol.HelloResult, error) {
	var result protocol.HelloResult
	challenge, err := protocol.NewNonce()
	if err != nil {
		return result, err
	}
	if err = c.call(ctx, protocol.MethodHello, protocol.HelloParams{VMRef: protocol.VMRef(c.instance.Ref.ID), Challenge: challenge, SupportedVersions: []string{"0.1"}}, &result); err != nil {
		return result, err
	}
	if result.Challenge != challenge || result.Version != "0.1" {
		c.fail(protocol.ErrFrame)
		return result, c.disconnected()
	}
	if _, err = c.GetState(ctx); err != nil {
		c.fail(err)
		return result, err
	}
	return result, nil
}

// PrepareAgent 固定登记内容的准备结果。
func (c *Client) PrepareAgent(ctx context.Context, p protocol.PrepareAgentParams) (protocol.Preparation, error) {
	var r protocol.Preparation
	err := c.call(ctx, protocol.MethodPrepareAgent, p, &r)
	return r, err
}

// StartSession 以调用方 operationID 提交唯一受控工作负载。
func (c *Client) StartSession(ctx context.Context, p protocol.StartSessionParams) (protocol.SessionInfo, error) {
	var r protocol.SessionInfo
	err := c.call(ctx, protocol.MethodStartSession, p, &r)
	return r, err
}

// GetState 返回当前 boot 的独立状态。
func (c *Client) GetState(ctx context.Context) (protocol.GuestState, error) {
	var r protocol.GuestState
	err := c.call(ctx, protocol.MethodGetState, protocol.Empty{}, &r)
	return r, err
}

// StopSession 显式停止工作负载组。
func (c *Client) StopSession(ctx context.Context, p protocol.StopSessionParams) error {
	return c.call(ctx, protocol.MethodStopSession, p, &protocol.Empty{})
}

// WaitSession 先订阅再查询，取消不影响其他等待者及进程。
func (c *Client) WaitSession(ctx context.Context, id protocol.OpaqueID) (protocol.SessionExit, error) {
	if err := protocol.Validate(id); err != nil {
		return protocol.SessionExit{}, err
	}
	if err := ctx.Err(); err != nil {
		return protocol.SessionExit{}, err
	}
	c.mu.Lock()
	changed := c.changed
	c.mu.Unlock()
	state, err := c.GetState(ctx)
	if err != nil {
		return protocol.SessionExit{}, err
	}
	found := false
	for _, s := range state.Sessions {
		if s.SessionID == id {
			found = true
			break
		}
	}
	if !found {
		return protocol.SessionExit{}, protocol.Failure(protocol.CodeNotFound)
	}
	for {
		c.mu.Lock()
		exit, ok := c.exits[id]
		c.mu.Unlock()
		if ok {
			return exit, nil
		}
		select {
		case <-changed:
			c.mu.Lock()
			changed = c.changed
			c.mu.Unlock()
		case <-ctx.Done():
			return protocol.SessionExit{}, ctx.Err()
		case <-c.ctx.Done():
			return protocol.SessionExit{}, c.disconnected()
		}
	}
}

// Close 关闭连接并等待唯一控制读循环退出，保留固定结果。
func (c *Client) Close() error { c.fail(guest.ErrDisconnected); <-c.readerDone; return nil }
