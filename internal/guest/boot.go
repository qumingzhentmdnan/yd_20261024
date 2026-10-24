// Package guest 实现独立于连接存活的 Guest boot 状态与固定工作负载 Handler。
package guest

import (
	"context"
	"errors"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	"safeagent/internal/guest/runner"
	"safeagent/internal/protocol"
)

// Config 由可信镜像登记层加载，不由业务请求构造。
type Config struct {
	// Content 是启动前校验真实内容摘要后的固定登记记录。
	Content protocol.Preparation
	// Build 是守护进程构建标识。
	Build protocol.OpaqueID
	// ImageDigest 是镜像诊断摘要。
	ImageDigest protocol.Digest
	// Runner 是登记工作负载的固定执行配置。
	Runner runner.Config
}

// operation 保存接纳后不可更改的操作参数和最终结果。
type operation struct {
	// method 参与跨方法冲突判断。
	method protocol.Method
	// input 保存可比较的强类型参数。
	input any
	// fingerprint 复用公共规范摘要，结构化input仍作为相等性依据。
	fingerprint string
	// result 是提交时独立快照。
	result any
	// err 是已固定业务错误。
	err *protocol.Error
	// done 在最终结果提交后关闭。
	done chan struct{}
}

// session 保存固定状态、资源与有界输出缓冲。
type session struct {
	// info 由 boot.mu 保护。
	info protocol.SessionInfo
	// process 拥有唯一 Wait 和 PTY。
	process *runner.Process
	// output 是最多 64KiB 的待发原始字节。
	output []byte
	// dropped 是未绑定时丢弃的累计字节。
	dropped protocol.Counter
	// outputDone 表示 PTY 收集确实已经结束。
	outputDone bool
	// outputErr 保存异常收集错误。
	outputErr error
	// changed 在输出缓冲或绑定状态改变后广播。
	changed chan struct{}
	// ioMu 协调 PTY 输入、Resize 与输出结束后的 Close。
	ioMu chan struct{}
	// exitCommitted 在唯一boot退出状态提交后关闭。
	exitCommitted chan struct{}
}

// Boot 在连接断开后继续拥有操作记录及工作负载。
type Boot struct {
	// config 是不可变固定登记内容。
	config Config
	// mu 是操作预留、资源提交和状态快照的串行边界。
	mu sync.Mutex
	// ref 在首次 Hello 后固定。
	ref protocol.VMRef
	// epoch 在首次 Hello 后固定。
	epoch protocol.Epoch
	// operations 保存整个 boot 的操作结果，不淘汰。
	operations map[protocol.UUID]*operation
	// preparations 保存不可变准备记录。
	preparations map[protocol.OpaqueID]protocol.Preparation
	// prepared 指向最近一次准备，空表示尚未准备。
	prepared *protocol.Preparation
	// sessions 保存全部历史 session。
	sessions map[protocol.OpaqueID]*session
	// latest 指向最近创建的 session。
	latest protocol.OpaqueID
	// reserved 标识正在执行的唯一准备或启动操作。
	reserved bool
	// connection 是当前唯一控制连接。
	connection *connection
	// ctx 只在整体 boot 关闭时取消。
	ctx context.Context
	// beforeCreate 是包内测试的提交前故障闸门，生产零值不安装。
	beforeCreate func(context.Context) error
	// afterCreate 是包内测试的真实创建后登记前闸门，生产零值不安装。
	afterCreate func()
	// beforeExitCommit 是包内测试的退出结果提交闸门，生产零值不安装。
	beforeExitCommit func()
	// cancel 关闭整体 boot 的后台资源。
	cancel context.CancelFunc
	// tasks 等待所有输出收集及进程等待任务。
	tasks sync.WaitGroup
}

// New 校验镜像登记值，不在请求时凭客户端摘要创造资源。
func New(config Config) (*Boot, error) {
	if err := protocol.Validate(config.Content); err != nil {
		return nil, err
	}
	if err := protocol.Validate(config.Build); err != nil {
		return nil, err
	}
	if err := protocol.Validate(config.ImageDigest); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Boot{config: config, operations: make(map[protocol.UUID]*operation), preparations: make(map[protocol.OpaqueID]protocol.Preparation), sessions: make(map[protocol.OpaqueID]*session), ctx: ctx, cancel: cancel}, nil
}

// fixedError 在实际失败处将已接纳操作的错误固定为公共错误码。
func fixedError(err error) *protocol.Error {
	var e *protocol.Error
	if errors.As(err, &e) {
		copy := *e
		return &copy
	}
	if errors.Is(err, context.Canceled) {
		return protocol.Failure(protocol.CodeCancelled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return protocol.Failure(protocol.CodeDeadlineExceeded)
	}
	return protocol.Failure(protocol.CodeInternal)
}

// cloneInfo 返回不共享退出指针的状态快照。
func cloneInfo(info protocol.SessionInfo) protocol.SessionInfo {
	if info.Exit != nil {
		exit := *info.Exit
		info.Exit = &exit
	}
	return info
}

// notifyLocked 唤醒当前 session 的输出任务，调用方持有 boot.mu。
func (s *session) notifyLocked() { close(s.changed); s.changed = make(chan struct{}) }

// operate 原子预留唯一执行资格，真实进程创建后无条件登记成功。
func (c *connection) operate(ctx context.Context, method protocol.Method, input any) (any, error) {
	b := c.boot
	var id protocol.UUID
	switch p := input.(type) {
	case protocol.PrepareAgentParams:
		id = p.OperationID
	case protocol.StartSessionParams:
		id = p.OperationID
	}
	b.mu.Lock()
	if old := b.operations[id]; old != nil {
		if old.method != method || old.input != input {
			b.mu.Unlock()
			return nil, protocol.Failure(protocol.CodeConflict)
		}
		b.mu.Unlock()
		select {
		case <-old.done:
			if old.err != nil {
				copy := *old.err
				return nil, &copy
			}
			return old.result, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := b.checkNewLocked(input); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	_, fingerprint, err := protocol.Fingerprint(method, input)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	op := &operation{method: method, input: input, fingerprint: fingerprint, done: make(chan struct{})}
	b.operations[id] = op
	b.reserved = true
	b.mu.Unlock()
	value, err := b.executeOperation(ctx, input)
	b.mu.Lock()
	b.reserved = false
	if err == nil && method == protocol.MethodPrepareAgent {
		if err = ctx.Err(); err == nil {
			prepared := value.(protocol.Preparation)
			b.preparations[prepared.PreparedID] = prepared
			b.prepared = &prepared
		}
	}
	if err != nil {
		op.err = fixedError(err)
	} else {
		op.result = value
	}
	close(op.done)
	b.mu.Unlock()
	if op.err != nil {
		return nil, op.err
	}
	return value, nil
}

// executeOperation 执行已预留操作，准备结果由 operate 提交，启动结果由 start 提交。
func (b *Boot) executeOperation(ctx context.Context, input any) (any, error) {
	// 调用入口仅传入这两种已解码参数，内部不另设未知类型兜底。
	if p, ok := input.(protocol.StartSessionParams); ok {
		return b.start(ctx, p)
	}
	id, err := protocol.NewUUID()
	if err != nil {
		return nil, err
	}
	prepared := b.config.Content
	prepared.PreparedID = protocol.OpaqueID(id)
	return prepared, nil
}

// start 创建实际进程后立即登记，后续取消不回滚已经发生的启动。
func (b *Boot) start(ctx context.Context, p protocol.StartSessionParams) (protocol.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return protocol.SessionInfo{}, err
	}
	if b.beforeCreate != nil {
		if err := b.beforeCreate(ctx); err != nil {
			return protocol.SessionInfo{}, err
		}
	}
	process, err := runner.Start(b.config.Runner, p.SessionID, p.Rows, p.Cols)
	if err != nil {
		return protocol.SessionInfo{}, err
	}
	if b.afterCreate != nil {
		b.afterCreate()
	}
	info := protocol.SessionInfo{
		SessionID: p.SessionID, PreparedID: p.PreparedID,
		WorkspaceRef: p.WorkspaceRef, WorkspaceDigest: p.WorkspaceDigest,
		State: protocol.SessionStateRunning, GroupState: protocol.GroupStateRunning,
	}
	s := &session{info: info, process: process, changed: make(chan struct{}), ioMu: make(chan struct{}, 1), exitCommitted: make(chan struct{})}
	// 登记必须先于退出发布，真实创建后的路径不再检查请求 context。
	b.mu.Lock()
	b.sessions[p.SessionID] = s
	b.latest = p.SessionID
	b.mu.Unlock()
	b.tasks.Add(2)
	go b.collect(s)
	go b.wait(s)
	return info, nil
}

// checkNewLocked 检查引用、生命周期及容量后才能预留，调用方持有 boot.mu。
func (b *Boot) checkNewLocked(input any) error {
	if b.reserved {
		return protocol.Failure(protocol.CodeFailedPrecondition)
	}
	if len(b.operations) >= protocol.MaxOperations {
		return protocol.Failure(protocol.CodeResourceExhausted)
	}
	for _, s := range b.sessions {
		if s.info.GroupState != protocol.GroupStateEmpty {
			return protocol.Failure(protocol.CodeFailedPrecondition)
		}
	}
	switch p := input.(type) {
	case protocol.PrepareAgentParams:
		expected := b.config.Content
		if p.ManifestRef != expected.ManifestRef || p.ManifestDigest != expected.ManifestDigest || p.ConfigDigest != expected.ConfigDigest || p.WorkspaceRef != expected.WorkspaceRef || p.WorkspaceDigest != expected.WorkspaceDigest {
			return protocol.Failure(protocol.CodeFailedPrecondition)
		}
	case protocol.StartSessionParams:
		if b.prepared == nil {
			return protocol.Failure(protocol.CodeFailedPrecondition)
		}
		prepared, ok := b.preparations[p.PreparedID]
		if !ok {
			return protocol.Failure(protocol.CodeNotFound)
		}
		if p.WorkspaceRef != prepared.WorkspaceRef || p.WorkspaceDigest != prepared.WorkspaceDigest {
			return protocol.Failure(protocol.CodeFailedPrecondition)
		}
		if b.sessions[p.SessionID] != nil {
			return protocol.Failure(protocol.CodeConflict)
		}
		if len(b.sessions) >= protocol.MaxSessions {
			return protocol.Failure(protocol.CodeResourceExhausted)
		}
	}
	return nil
}

// state 复制同一时刻 boot 状态，不返回可修改内部指针。
func (c *connection) state() protocol.GuestState {
	b := c.boot
	b.mu.Lock()
	defer b.mu.Unlock()
	state := protocol.GuestState{Sessions: []protocol.SessionInfo{}}
	if b.prepared != nil {
		copy := *b.prepared
		state.Prepared = &copy
	}
	for _, s := range b.sessions {
		state.Sessions = append(state.Sessions, cloneInfo(s.info))
	}
	sort.Slice(state.Sessions, func(i, j int) bool { return state.Sessions[i].SessionID < state.Sessions[j].SessionID })
	if b.latest != "" {
		id := b.latest
		state.CurrentSessionID = &id
	}
	if a := c.attachment; a != nil {
		state.Attachment = &protocol.AttachmentInfo{SessionID: a.session.info.SessionID, StreamID: a.stream, ConnectionID: c.id, State: a.state}
	}
	return state
}

// wait 在唯一进程等待完成后固定主进程状态，组状态继续独立观察。
func (b *Boot) wait(s *session) {
	defer b.tasks.Done()
	exit, err := s.process.Wait(b.ctx)
	if err != nil {
		return
	}
	if b.beforeExitCommit != nil {
		b.beforeExitCommit()
	}
	b.mu.Lock()
	s.info.State = protocol.SessionStateExited
	s.info.Exit = &exit
	close(s.exitCommitted)
	c := b.connection
	b.mu.Unlock()
	if c != nil {
		c.event(exit)
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		state, err := s.process.Group()
		b.mu.Lock()
		s.info.GroupState = state
		b.mu.Unlock()
		if err != nil || state == protocol.GroupStateEmpty {
			return
		}
		select {
		case <-tick.C:
		case <-b.ctx.Done():
			return
		}
	}
}

// collect 持续收集 PTY 输出；已绑定时有界背压，未绑定时丢弃最旧字节。
func (b *Boot) collect(s *session) {
	defer b.tasks.Done()
	buf := make([]byte, protocol.TerminalLimit)
	for {
		b.mu.Lock()
		bound := b.connection != nil && b.connection.attachment != nil && b.connection.attachment.session == s && b.connection.attachment.state == protocol.AttachmentStateBound
		if bound && len(s.output) >= protocol.OutputLimit {
			changed := s.changed
			b.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-b.ctx.Done():
				return
			}
		}
		room := len(buf)
		if bound && protocol.OutputLimit-len(s.output) < room {
			room = protocol.OutputLimit - len(s.output)
		}
		b.mu.Unlock()
		n, err := s.process.Read(buf[:room])
		b.mu.Lock()
		if n > 0 {
			s.output = append(s.output, buf[:n]...)
			if len(s.output) > protocol.OutputLimit {
				drop := len(s.output) - protocol.OutputLimit
				s.output = append([]byte(nil), s.output[drop:]...)
				s.dropped += protocol.Counter(drop)
			}
		}
		if err != nil {
			s.outputDone = true
			if !errors.Is(err, io.EOF) {
				s.outputErr = err
			}
		}
		s.notifyLocked()
		b.mu.Unlock()
		if err != nil {
			s.ioMu <- struct{}{}
			s.process.Close()
			<-s.ioMu
			return
		}
	}
}

// Close 结束整个 boot，只用于入口退出或测试独立清理，不等同于控制断线。
func (b *Boot) Close(ctx context.Context) error {
	b.mu.Lock()
	c := b.connection
	sessions := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.mu.Unlock()
	if c != nil {
		c.close()
	}
	var result error
	for _, s := range sessions {
		if err := s.process.Stop(ctx, 100*time.Millisecond, true); err != nil {
			result = errors.Join(result, err)
		}
		s.process.Close()
		if err := s.process.Reap(ctx); err != nil {
			result = errors.Join(result, err)
		}
	}
	b.cancel()
	b.mu.Lock()
	for _, s := range sessions {
		s.notifyLocked()
	}
	b.mu.Unlock()
	done := make(chan struct{})
	go func() { b.tasks.Wait(); close(done) }()
	select {
	case <-done:
		return result
	case <-ctx.Done():
		return errors.Join(result, ctx.Err())
	}
}

// Serve 接管本轮独立控制和终端监听器，控制重连必须由装配层证明代次隔离。
func (b *Boot) Serve(ctx context.Context, control net.Conn, terminal net.Listener) error {
	c := newConnection(b, control, terminal)
	b.mu.Lock()
	if b.connection != nil {
		b.mu.Unlock()
		return protocol.Failure(protocol.CodeFailedPrecondition)
	}
	b.connection = c
	b.mu.Unlock()
	stop := context.AfterFunc(ctx, c.close)
	defer stop()
	err := c.run()
	c.close()
	c.tasks.Wait()
	b.mu.Lock()
	b.connection = nil
	b.mu.Unlock()
	return err
}
