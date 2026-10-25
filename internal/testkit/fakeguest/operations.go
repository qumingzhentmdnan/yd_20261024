package fakeguest

import (
	"context"
	"errors"
	"sort"
	"sync"

	"safeagent/internal/host/guest"
	"safeagent/internal/protocol"
)

// Point 指定已接纳操作的故障阶段。
type Point string

const (
	PrepareBeforeCommit Point = "prepare-before-commit"
	StartBeforeCommit   Point = "start-before-commit"
	StartAfterCommit    Point = "start-after-commit"
)

// Gate 用显式到达与释放控制时序，不依赖 sleep 猜测执行阶段。
type Gate struct {
	// entered 在首次进入时关闭。
	entered chan struct{}
	// release 在测试放行时关闭。
	release chan struct{}
	// enterOnce 保证到达通知只关闭一次。
	enterOnce sync.Once
	// releaseOnce 保证重复放行安全。
	releaseOnce sync.Once
}

// NewGate 创建尚未到达且未放行的闸门。
func NewGate() *Gate { return &Gate{entered: make(chan struct{}), release: make(chan struct{})} }

// Entered 返回确定到达通知。
func (g *Gate) Entered() <-chan struct{} { return g.entered }

// Release 放行当前及后续等待。
func (g *Gate) Release() { g.releaseOnce.Do(func() { close(g.release) }) }

// wait 等待测试放行，同时响应请求取消和连接断开。
func (g *Gate) wait(ctx context.Context, done <-chan struct{}) error {
	g.enterOnce.Do(func() { close(g.entered) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return guest.ErrDisconnected
	case <-g.release:
		return ctx.Err()
	}
}

// Fault 是下一次到达指定阶段的单次故障，零值不等待且不失败。
type Fault struct {
	// Gate 非空时在该阶段阻塞，直到测试放行或调用失效。
	Gate *Gate
	// Error 在提交前作为固定业务失败，提交后只用于模拟响应丢失。
	Error *protocol.Error
}

// Inject 配置单次故障；提交后的失败不得回滚已创建 session。
func (b *Boot) Inject(point Point, fault Fault) error {
	if point != PrepareBeforeCommit && point != StartBeforeCommit && point != StartAfterCommit {
		return protocol.Failure(protocol.CodeInvalidArgument)
	}
	if fault.Error != nil {
		if err := protocol.Validate(*fault.Error); err != nil {
			return err
		}
		copy := *fault.Error
		fault.Error = &copy
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults[point] = fault
	return nil
}

// takeFaultLocked 取走一次故障，调用方必须持有 boot.mu。
func (b *Boot) takeFaultLocked(point Point) Fault {
	f := b.faults[point]
	delete(b.faults, point)
	return f
}

// runFault 执行指定阶段的闸门与业务故障，不持有 boot 状态锁。
func runFault(ctx context.Context, done <-chan struct{}, fault Fault) error {
	if fault.Gate != nil {
		if err := fault.Gate.wait(ctx, done); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-done:
		return guest.ErrDisconnected
	default:
	}
	if fault.Error != nil {
		copy := *fault.Error
		return &copy
	}
	return nil
}

// fixedError 将已接纳操作的取消或内部失败固定为 wire 错误，供新请求重试。
func fixedError(err error) *protocol.Error {
	var business *protocol.Error
	if errors.As(err, &business) {
		copy := *business
		return &copy
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, guest.ErrDisconnected) {
		return protocol.Failure(protocol.CodeCancelled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return protocol.Failure(protocol.CodeDeadlineExceeded)
	}
	return protocol.Failure(protocol.CodeInternal)
}

// operate 原子查重和预留操作，在提交前后提供确定的故障边界。
func (c *Client) operate(ctx context.Context, method protocol.Method, input any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := protocol.Validate(input); err != nil {
		return nil, err
	}
	var id protocol.UUID
	point := PrepareBeforeCommit
	switch p := input.(type) {
	case protocol.PrepareAgentParams:
		id = p.OperationID
	case protocol.StartSessionParams:
		id = p.OperationID
		point = StartBeforeCommit
	}
	b := c.boot
	b.mu.Lock()
	if err := c.checkLocked(true); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if old := b.operations[id]; old != nil {
		if old.method != method || old.input != input {
			b.mu.Unlock()
			return nil, protocol.Failure(protocol.CodeConflict)
		}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, guest.ErrDisconnected
		case <-old.done:
		}
		if old.err != nil {
			copy := *old.err
			return nil, &copy
		}
		return old.result, nil
	}
	if err := b.checkNewLocked(input); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	op := &operation{method: method, input: input, done: make(chan struct{})}
	b.operations[id] = op
	before := b.takeFaultLocked(point)
	b.mu.Unlock()
	err := runFault(ctx, c.done, before)
	b.mu.Lock()
	if err == nil {
		err = c.checkLocked(true)
	}
	if err == nil {
		err = ctx.Err()
	}
	// 新操作可能在闸门期间被其他操作改变前置状态，提交点再次串行核对。
	if err == nil {
		err = b.checkCommitLocked(input)
	}
	if err != nil {
		op.err = fixedError(err)
		close(op.done)
		b.mu.Unlock()
		return nil, err
	}
	switch p := input.(type) {
	case protocol.PrepareAgentParams:
		content := b.config.Content
		b.prepared = &content
		op.result = content
	case protocol.StartSessionParams:
		info := protocol.SessionInfo{SessionID: p.SessionID, PreparedID: p.PreparedID, WorkspaceRef: p.WorkspaceRef, WorkspaceDigest: p.WorkspaceDigest, State: protocol.SessionStateRunning, GroupState: protocol.GroupStateRunning}
		b.sessions[p.SessionID] = &session{info: info, rows: p.Rows, cols: p.Cols, changed: make(chan struct{})}
		b.latest = p.SessionID
		op.result = info
	}
	// 结果先固定；之后调用取消或响应丢失不能改写去重记录。
	close(op.done)
	after := Fault{}
	if method == protocol.MethodStartSession {
		after = b.takeFaultLocked(StartAfterCommit)
	}
	b.mu.Unlock()
	if err := runFault(ctx, c.done, after); err != nil {
		return nil, err
	}
	return op.result, nil
}

// checkNewLocked 在预留前检查资源与操作容量，重复操作不会经过这里。
func (b *Boot) checkNewLocked(input any) error {
	if err := b.checkCommitLocked(input); err != nil {
		return err
	}
	if len(b.operations) >= protocol.MaxOperations {
		return protocol.Failure(protocol.CodeResourceExhausted)
	}
	return nil
}

// checkCommitLocked 校验会随并发操作变化的引用、生命周期与 session 容量。
func (b *Boot) checkCommitLocked(input any) error {
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
		if p.PreparedID != b.prepared.PreparedID {
			return protocol.Failure(protocol.CodeNotFound)
		}
		if p.WorkspaceRef != b.prepared.WorkspaceRef || p.WorkspaceDigest != b.prepared.WorkspaceDigest {
			return protocol.Failure(protocol.CodeFailedPrecondition)
		}
		if b.sessions[p.SessionID] != nil {
			return protocol.Failure(protocol.CodeConflict)
		}
		if len(b.sessions) >= protocol.MaxSessions {
			return protocol.Failure(protocol.CodeResourceExhausted)
		}
	}
	for _, s := range b.sessions {
		if s.info.GroupState != protocol.GroupStateEmpty {
			return protocol.Failure(protocol.CodeFailedPrecondition)
		}
	}
	return nil
}

// PrepareAgent 按已登记内容准备，返回不可变准备记录。
func (c *Client) PrepareAgent(ctx context.Context, p protocol.PrepareAgentParams) (protocol.Preparation, error) {
	v, err := c.operate(ctx, protocol.MethodPrepareAgent, p)
	if err != nil {
		return protocol.Preparation{}, err
	}
	return protocol.ClonePreparation(v.(protocol.Preparation)), nil
}

// StartSession 去重创建虚拟工作负载，不执行任意命令。
func (c *Client) StartSession(ctx context.Context, p protocol.StartSessionParams) (protocol.SessionInfo, error) {
	v, err := c.operate(ctx, protocol.MethodStartSession, p)
	if err != nil {
		return protocol.SessionInfo{}, err
	}
	return cloneInfo(v.(protocol.SessionInfo)), nil
}

// GetState 返回按 sessionID 排序的独立状态快照。
func (c *Client) GetState(ctx context.Context) (protocol.GuestState, error) {
	if err := ctx.Err(); err != nil {
		return protocol.GuestState{}, err
	}
	b := c.boot
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := c.checkLocked(true); err != nil {
		return protocol.GuestState{}, err
	}
	state := protocol.GuestState{Sessions: []protocol.SessionInfo{}}
	if b.prepared != nil {
		copy := protocol.ClonePreparation(*b.prepared)
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
		state.Attachment = &protocol.AttachmentInfo{SessionID: a.session.info.SessionID, StreamID: a.stream, ConnectionID: c.id, State: protocol.AttachmentStateBound}
	}
	return state, nil
}

// WaitSession 等待固定结果，单个等待者取消不影响工作负载或其他等待者。
func (c *Client) WaitSession(ctx context.Context, id protocol.OpaqueID) (protocol.SessionExit, error) {
	if err := protocol.Validate(id); err != nil {
		return protocol.SessionExit{}, err
	}
	b := c.boot
	for {
		if err := ctx.Err(); err != nil {
			return protocol.SessionExit{}, err
		}
		b.mu.Lock()
		if err := c.checkLocked(true); err != nil {
			b.mu.Unlock()
			return protocol.SessionExit{}, err
		}
		s, err := b.findLocked(id)
		if err != nil {
			b.mu.Unlock()
			return protocol.SessionExit{}, err
		}
		if s.info.Exit != nil {
			exit := *s.info.Exit
			b.mu.Unlock()
			return exit, nil
		}
		changed := s.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return protocol.SessionExit{}, ctx.Err()
		case <-c.done:
			return protocol.SessionExit{}, guest.ErrDisconnected
		case <-changed:
		}
	}
}
