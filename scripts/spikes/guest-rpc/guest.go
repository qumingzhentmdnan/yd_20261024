package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
)

// request 是控制端口的最小请求 envelope。
type request struct {
	// RequestID 是当前连接内严格递增的十进制标识。
	RequestID string `json:"requestID"`
	// Epoch 对应 Host 确认的 VM 启动代次。
	Epoch string `json:"epoch"`
	// ConnectionID 标识当前逻辑控制连接。
	ConnectionID string `json:"connectionID"`
	// Method 是固定方法名；Cancel 中为空。
	Method string `json:"method,omitempty"`
	// TimeoutMs 是接收完整请求后开始的处理预算。
	TimeoutMs int `json:"timeoutMs,omitempty"`
	// Params 是由具体方法严格解码的参数对象。
	Params json.RawMessage `json:"params,omitempty"`
}

// params 是原型固定方法的参数；正式 schema 在验证后独立定义。
type params struct {
	// VMRef 是 Host 传入的稳定资源标识。
	VMRef string `json:"vmRef,omitempty"`
	// Challenge 用于关联 Hello，不是远程证明。
	Challenge string `json:"challenge,omitempty"`
	// OperationID 对 Prepare/Start 提供 boot 内去重。
	OperationID string `json:"operationID,omitempty"`
	// Manifest 固定为镜像内置的 probe-v1。
	Manifest string `json:"manifest,omitempty"`
	// WorkspaceDigest 固定为原型内置工作区摘要。
	WorkspaceDigest string `json:"workspaceDigest,omitempty"`
	// PreparedID 引用同一 boot 内的准备结果。
	PreparedID string `json:"preparedID,omitempty"`
	// SessionID 是 Host 为本次工作负载分配的标识。
	SessionID string `json:"sessionID,omitempty"`
	// StreamID 是 Host 分配且不复用的终端 attachment 标识。
	StreamID uint32 `json:"streamID,omitempty"`
	// Rows 是初始或调整后的行数。
	Rows uint16 `json:"rows,omitempty"`
	// Cols 是初始或调整后的列数。
	Cols uint16 `json:"cols,omitempty"`
	// GraceMs 是停止进程组的宽限期。
	GraceMs int `json:"graceMs,omitempty"`
	// ForceAfterTimeout 显式允许超时后强制终止。
	ForceAfterTimeout bool `json:"forceAfterTimeout,omitempty"`
}

// rpcError 表示不会携带宿主秘密的固定业务错误。
type rpcError struct {
	// Code 是候选协议错误码。
	Code string `json:"code"`
	// Message 仅使用原型内部固定文本。
	Message string `json:"message"`
}

// operation 保留同一操作的参数和固定结果。
type operation struct {
	// method 限制 operationID 不能跨方法复用。
	method string
	// input 是已解码参数，用结构化比较消除 JSON 字段顺序差异。
	input params
	// result 是提交后的固定成功结果。
	result any
}

// outgoing 将帧与可选的发送确认关联，关闭通知不能在写出前取消自己。
type outgoing struct {
	// frame 是交给唯一写循环的消息。
	frame frame
	// written 在需要确认时接收实际写结果，为空时不等待确认。
	written chan error
}

// guest 保存跨逻辑连接的 boot 状态。
type guest struct {
	// mu 保护 boot 状态、操作记录和当前连接。
	mu sync.Mutex
	// epoch 由首个 Hello 固定，不允许同 boot 替换。
	epoch string
	// vmRef 固定首个 Hello 的 Host 资源标识。
	vmRef string
	// prepared 引用本实验唯一的固定 Manifest 准备结果。
	prepared string
	// operations 保留最多 256 个操作，不做 TTL 淘汰。
	operations map[string]operation
	// sessions 保留已创建 session 和退出结果。
	sessions map[string]*session
	// current 是当前或最近一次 session。
	current *session
	// connection 是当前已握手控制连接。
	connection *connection
	// starts 用于断言去重没有创建额外进程。
	starts int
}

// connection 保存只在一个控制连接内有效的权限和请求。
type connection struct {
	// guest 指向 boot 范围状态。
	guest *guest
	// ctx 在控制断开或协议失效时取消。
	ctx context.Context
	// cancel 唤醒本连接全部设备等待。
	cancel context.CancelFunc
	// port 是当前控制设备句柄。
	port *device
	// outgoing 是容量固定为 32 的控制发送队列。
	outgoing chan outgoing
	// mu 保护连接标识、请求和终端绑定。
	mu sync.Mutex
	// id 在 Hello 成功后固定。
	id string
	// epoch 是握手成功后固定的代次。
	epoch string
	// hello 表示当前连接已经完成握手。
	hello bool
	// pending 允许 Cancel 找到已接纳的请求。
	pending map[string]context.CancelFunc
	// lastRequest 拒绝重复或倒退请求标识。
	lastRequest uint64
	// requests 统计本连接已收到的请求数。
	requests int
	// eventID 是事件序号。
	eventID uint64
	// attachment 是当前待绑定或已绑定的终端。
	attachment *attachment
	// streams 记录本连接已使用过的 streamID。
	streams map[uint32]bool
	// workers 保证取消后旧处理任务不跨入新连接。
	workers sync.WaitGroup
}

// failure 构造固定业务错误。
func failure(code string) *rpcError { return &rpcError{Code: code, Message: code} }

// send 将消息放入有界队列，超过两秒则使逻辑连接失效。
func (c *connection) send(f frame) bool {
	select {
	case c.outgoing <- outgoing{frame: f}:
		return true
	case <-c.ctx.Done():
		return false
	case <-time.After(2 * time.Second):
		c.cancel()
		return false
	}
}

// sendConfirmed 在连接失效前确认最后一帧已写出，等待仍有明确上限。
func (c *connection) sendConfirmed(f frame) {
	written := make(chan error, 1)
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	select {
	case c.outgoing <- outgoing{frame: f, written: written}:
	case <-ctx.Done():
		return
	}
	select {
	case <-written:
	case <-ctx.Done():
	}
}

// response 为每个请求生成唯一响应，发送完成由独立循环负责。
func (c *connection) response(r request, result any, problem *rpcError) {
	message := map[string]any{"requestID": r.RequestID, "epoch": r.Epoch, "connectionID": r.ConnectionID}
	if problem != nil {
		message["error"] = problem
	} else {
		message["result"] = result
	}
	c.send(frame{kind: responseKind, body: jsonBody(message)})
}

// event 将固定状态事件发送给当前连接。
func (c *connection) event(name string, value any) {
	c.mu.Lock()
	c.eventID++
	message := map[string]any{"epoch": c.epoch, "connectionID": c.id, "eventID": strconv.FormatUint(c.eventID, 10), "event": name, "params": value}
	c.mu.Unlock()
	c.send(frame{kind: eventKind, body: jsonBody(message)})
}

// writer 串行写控制消息，每帧的设备写预算固定为两秒。
func (c *connection) writer() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case message := <-c.outgoing:
			ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
			err := writeFrame(&device{file: c.port.file, ctx: ctx}, message.frame)
			cancel()
			if message.written != nil {
				message.written <- err
			}
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

// state 返回当前 session 快照，退出结果不依赖事件是否送达。
func (g *guest) state() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := map[string]any{"preparedID": g.prepared, "starts": g.starts, "operationCount": len(g.operations)}
	if g.current != nil {
		result["session"] = g.current.snapshot()
	}
	return result
}

// execute 实现原型必需的方法，不提供任意命令执行入口。
func (c *connection) execute(ctx context.Context, r request, p params) (any, *rpcError) {
	g := c.guest
	switch r.Method {
	case "GetState":
		return g.state(), nil
	case "PrepareAgent", "StartSession":
		if p.OperationID == "" {
			return nil, failure("INVALID_ARGUMENT")
		}
		// 检查和提交由同一把锁串行化，重复操作不能启动第二个进程。
		g.mu.Lock()
		defer g.mu.Unlock()
		if old, ok := g.operations[p.OperationID]; ok {
			if old.method != r.Method || old.input != p {
				return nil, failure("CONFLICT")
			}
			return old.result, nil
		}
		if len(g.operations) == 256 {
			return nil, failure("RESOURCE_EXHAUSTED")
		}
		if err := ctx.Err(); err != nil {
			return nil, failure("CANCELLED")
		}
		if p.WorkspaceDigest != "sha256:probe-workspace" {
			return nil, failure("FAILED_PRECONDITION")
		}
		var result any
		if r.Method == "PrepareAgent" {
			if p.Manifest != "probe-v1" {
				return nil, failure("INVALID_ARGUMENT")
			}
			if g.prepared == "" {
				g.prepared = newID()
			}
			result = map[string]any{"preparedID": g.prepared}
		} else {
			if p.PreparedID == "" || p.PreparedID != g.prepared || p.SessionID == "" || p.Rows == 0 || p.Cols == 0 {
				return nil, failure("FAILED_PRECONDITION")
			}
			if _, ok := g.sessions[p.SessionID]; ok {
				return nil, failure("CONFLICT")
			}
			if g.current != nil {
				select {
				case <-g.current.done:
				default:
					return nil, failure("FAILED_PRECONDITION")
				}
			}
			s, err := startSession(p.SessionID, p.Rows, p.Cols, g.exited)
			if err != nil {
				fmt.Fprintln(os.Stderr, "START_ERROR", err)
				return nil, failure("INTERNAL")
			}
			g.current = s
			g.sessions[s.id] = s
			g.starts++
			result = map[string]any{"sessionID": s.id, "pid": s.command.Process.Pid}
		}
		g.operations[p.OperationID] = operation{method: r.Method, input: p, result: result}
		return result, nil
	case "AttachTerminal":
		return c.attach(p)
	case "DetachTerminal":
		c.mu.Lock()
		a := c.attachment
		if a != nil && (a.stream != p.StreamID || a.session.id != p.SessionID) {
			c.mu.Unlock()
			return nil, failure("NOT_FOUND")
		}
		c.mu.Unlock()
		if a != nil {
			c.detach(a)
		}
		return map[string]any{}, nil
	case "ResizeTerminal", "StopSession":
		g.mu.Lock()
		s := g.sessions[p.SessionID]
		g.mu.Unlock()
		if s == nil {
			return nil, failure("NOT_FOUND")
		}
		if r.Method == "ResizeTerminal" {
			c.mu.Lock()
			a := c.attachment
			c.mu.Unlock()
			if a == nil || a.session != s || a.stream != p.StreamID || p.Rows == 0 || p.Cols == 0 {
				return nil, failure("FAILED_PRECONDITION")
			}
			if err := s.resize(p.Rows, p.Cols); err != nil {
				return nil, failure("INTERNAL")
			}
		} else {
			if p.GraceMs < 1 || p.GraceMs > 30000 {
				return nil, failure("INVALID_ARGUMENT")
			}
			if err := s.stop(ctx, time.Duration(p.GraceMs)*time.Millisecond, p.ForceAfterTimeout); err != nil {
				fmt.Fprintln(os.Stderr, "STOP_ERROR", err)
				if errors.Is(err, context.Canceled) {
					return nil, failure("CANCELLED")
				}
				return nil, failure("DEADLINE_EXCEEDED")
			}
		}
		return map[string]any{}, nil
	default:
		return nil, failure("UNIMPLEMENTED")
	}
}

// exited 在退出状态提交后向当前控制连接通知，重连后仍能查询结果。
func (g *guest) exited(s *session) {
	g.mu.Lock()
	c := g.connection
	g.mu.Unlock()
	if c != nil {
		c.event("SessionExited", s.snapshot()["exit"])
	}
}

// helloRequest 在读取后同步完成握手，避免后续请求抢先执行。
func (c *connection) helloRequest(r request, p params) *rpcError {
	if r.Method != "Hello" {
		return failure("FAILED_PRECONDITION")
	}
	if r.Epoch == "" || r.ConnectionID == "" || p.VMRef == "" || p.Challenge == "" {
		return failure("INVALID_ARGUMENT")
	}
	g := c.guest
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.epoch != "" && (g.epoch != r.Epoch || g.vmRef != p.VMRef) {
		return failure("STALE_INSTANCE")
	}
	g.epoch, g.vmRef = r.Epoch, p.VMRef
	c.mu.Lock()
	c.id, c.epoch, c.hello = r.ConnectionID, r.Epoch, true
	c.mu.Unlock()
	g.connection = c
	return nil
}

// run 处理控制请求、取消和连接失效，处理任务与设备读循环分离。
func (c *connection) run() (runErr error) {
	c.workers.Add(2)
	go func() { defer c.workers.Done(); c.writer() }()
	go func() { defer c.workers.Done(); c.terminalLoop() }()
	defer func() {
		if runErr != nil && !errors.Is(runErr, io.EOF) && !errors.Is(runErr, context.Canceled) {
			c.mu.Lock()
			if c.hello {
				c.eventID++
				message := map[string]any{"epoch": c.epoch, "connectionID": c.id, "eventID": strconv.FormatUint(c.eventID, 10), "event": "ConnectionClosed", "params": map[string]any{"code": "PROTOCOL_ERROR"}}
				c.mu.Unlock()
				c.sendConfirmed(frame{kind: eventKind, body: jsonBody(message)})
			} else {
				c.mu.Unlock()
			}
		}
		c.cancel()
		c.mu.Lock()
		a := c.attachment
		c.mu.Unlock()
		if a != nil {
			c.detach(a)
		}
		c.workers.Wait()
		c.guest.mu.Lock()
		if c.guest.connection == c {
			c.guest.connection = nil
		}
		c.guest.mu.Unlock()
	}()
	for {
		f, err := readFrame(c.port, false)
		if err != nil {
			return err
		}
		if f.kind != requestKind && f.kind != cancelKind {
			return errors.New("unexpected control direction")
		}
		var r request
		if err := decodeJSON(f.body, &r); err != nil {
			return err
		}
		c.mu.Lock()
		hello, epoch, id := c.hello, c.epoch, c.id
		c.mu.Unlock()
		if f.kind == cancelKind {
			if !hello || r.Epoch != epoch || r.ConnectionID != id {
				return errors.New("invalid cancel identity")
			}
			c.mu.Lock()
			cancel := c.pending[r.RequestID]
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			continue
		}
		number, err := strconv.ParseUint(r.RequestID, 10, 64)
		if err != nil || number == 0 || strconv.FormatUint(number, 10) != r.RequestID || number <= c.lastRequest || c.requests >= 4096 {
			return errors.New("invalid request id")
		}
		c.lastRequest = number
		c.requests++
		if r.TimeoutMs < 1 || r.TimeoutMs > 30000 {
			c.response(r, nil, failure("INVALID_ARGUMENT"))
			continue
		}
		var p params
		if err := decodeJSON(r.Params, &p); err != nil {
			c.response(r, nil, failure("INVALID_ARGUMENT"))
			continue
		}
		if !hello {
			problem := c.helloRequest(r, p)
			if problem == nil {
				c.response(r, map[string]any{"challenge": p.Challenge, "version": "0.1"}, nil)
			} else {
				c.response(r, nil, problem)
			}
			continue
		}
		if r.Epoch != epoch || r.ConnectionID != id {
			return errors.New("stale epoch/connection")
		}
		if r.Method == "Hello" {
			c.response(r, nil, failure("FAILED_PRECONDITION"))
			continue
		}
		c.mu.Lock()
		if len(c.pending) >= 16 {
			c.mu.Unlock()
			c.response(r, nil, failure("RESOURCE_EXHAUSTED"))
			continue
		}
		ctx, cancel := context.WithTimeout(c.ctx, time.Duration(r.TimeoutMs)*time.Millisecond)
		c.pending[r.RequestID] = cancel
		c.mu.Unlock()
		c.workers.Add(1)
		go func() {
			defer c.workers.Done()
			defer cancel()
			result, problem := c.execute(ctx, r, p)
			c.mu.Lock()
			delete(c.pending, r.RequestID)
			c.mu.Unlock()
			c.response(r, result, problem)
		}()
	}
}

// main 只运行镜像内实验服务，协议错误退出 guestd，由 Host 重建连接与进程。
func main() {
	g := &guest{operations: map[string]operation{}, sessions: map[string]*session{}}
	for {
		ctx, cancel := context.WithCancel(context.Background())
		port, err := openDevice(ctx, "/dev/safeagent.control")
		if err != nil {
			panic(err)
		}
		c := &connection{guest: g, ctx: ctx, cancel: cancel, port: port, outgoing: make(chan outgoing, 32), pending: map[string]context.CancelFunc{}, streams: map[uint32]bool{}}
		err = c.run()
		port.file.Close()
		if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "CONTROL_PROTOCOL_ERROR", err)
		}
		fmt.Fprintln(os.Stderr, "CONTROL_CLOSED:"+c.id)
		// 设备关闭不一定关闭 Host socket；实验要求 Host 主动关闭并重新 Hello。
		time.Sleep(20 * time.Millisecond)
	}
}
