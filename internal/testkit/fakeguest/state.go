// Package fakeguest 提供不启动 VM、进程或网络的有状态 Guest 替代实现。
package fakeguest

import (
	"context"
	"sync"

	"safeagent/internal/host/guest"
	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
)

// Config 显式提供固定测试镜像与唯一登记内容，不使用生产凭据或隐式回退。
type Config struct {
	// Instance 绑定现有 VM 生命周期产生的实例。
	Instance vm.VMInstance
	// Content 是可由 PrepareAgent 激活的固定登记内容和预定 preparedID。
	Content protocol.Preparation
	// GuestBuild 是调用方指定的 Fake 构建标识。
	GuestBuild protocol.OpaqueID
	// ImageDigest 是显式的测试镜像诊断摘要。
	ImageDigest protocol.Digest
}

// Boot 保存一个启动代次的状态；关闭连接不清空去重或退出记录。
type Boot struct {
	// mu 保护所有状态和连接，等待时必须释放。
	mu sync.Mutex
	// config 在构造时校验后不可修改。
	config Config
	// current 标识唯一活动连接，空表示尚未连接或已关闭。
	current *Client
	// prepared 在首次成功准备后有值。
	prepared *protocol.Preparation
	// operations 保存包括已接纳失败在内的最多 256 个操作。
	operations map[protocol.UUID]*operation
	// sessions 保存最多 64 个 session 与固定退出状态。
	sessions map[protocol.OpaqueID]*session
	// latest 指向最近创建的 session，空表示尚未创建。
	latest protocol.OpaqueID
	// faults 保存各阶段下一次操作的故障。
	faults map[Point]Fault
}

// operation 在执行开始前预留，结果提交后关闭 done。
type operation struct {
	// method 防止 operationID 跨方法复用。
	method protocol.Method
	// input 是已校验的可比较参数结构。
	input any
	// result 固定成功值，调用方读取时复制可变成员。
	result any
	// err 固定已接纳操作的业务失败。
	err *protocol.Error
	// done 在固定结果提交后关闭。
	done chan struct{}
}

// session 记录模拟工作负载与有界输入输出，不创建实际进程。
type session struct {
	// info 包含当前主进程与进程组状态。
	info protocol.SessionInfo
	// changed 在每次状态或缓冲变更后关闭并替换。
	changed chan struct{}
	// output 保存至多 64 KiB 待读字节。
	output []byte
	// input 保存至多 64 KiB 待测试端取走的输入。
	input []byte
	// outputEnded 表示测试端已模拟 Guest Fin。
	outputEnded bool
	// dropped 统计未绑定期间丢弃的最旧输出字节。
	dropped protocol.Counter
	// terminal 是当前已绑定的唯一终端，空表示未绑定。
	terminal *terminal
	// rows 保存最近一次实际模拟的窗口行数。
	rows protocol.Dimension
	// cols 保存最近一次实际模拟的窗口列数。
	cols protocol.Dimension
	// ignoreTERM 为显式故障控制，零值表示 TERM 能停止进程组。
	ignoreTERM bool
}

// Client 保存一个连接代次；新连接必须重新 Hello。
type Client struct {
	// boot 保存跨连接状态。
	boot *Boot
	// id 是 Host 模拟端生成的连接 UUID。
	id protocol.UUID
	// done 在本连接断开时关闭。
	done chan struct{}
	// closed 由 boot.mu 保护，防止重复关闭。
	closed bool
	// ready 表示本连接已成功 Hello。
	ready bool
	// stream 按连接递增且不复用。
	stream protocol.StreamID
	// attachment 是当前终端授权，空表示尚未绑定。
	attachment *terminal
}

var _ guest.Client = (*Client)(nil)

// New 校验固定测试数据并创建独立 boot，不启动任何后台任务。
func New(config Config) (*Boot, error) {
	if err := config.Instance.Validate(); err != nil {
		return nil, err
	}
	for _, value := range []any{protocol.VMRef(config.Instance.Ref.ID), protocol.Epoch(config.Instance.Epoch), config.Content, config.GuestBuild, config.ImageDigest} {
		if err := protocol.Validate(value); err != nil {
			return nil, err
		}
	}
	config.Content = protocol.ClonePreparation(config.Content)
	return &Boot{config: config, operations: map[protocol.UUID]*operation{}, sessions: map[protocol.OpaqueID]*session{}, faults: map[Point]Fault{}}, nil
}

// Connect 绑定可信实例并创建新连接，旧连接未关闭时拒绝第二个消费者。
func (b *Boot) Connect(instance vm.VMInstance) (*Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if instance != b.config.Instance {
		return nil, protocol.Failure(protocol.CodeStaleInstance)
	}
	if b.current != nil {
		return nil, protocol.Failure(protocol.CodeFailedPrecondition)
	}
	id, err := protocol.NewUUID()
	if err != nil {
		return nil, err
	}
	c := &Client{boot: b, id: id, done: make(chan struct{})}
	b.current = c
	return c, nil
}

// checkLocked 检查连接权限，调用方必须持有 boot.mu。
func (c *Client) checkLocked(hello bool) error {
	if c.closed {
		return guest.ErrDisconnected
	}
	if hello && !c.ready {
		return protocol.Failure(protocol.CodeFailedPrecondition)
	}
	return nil
}

// Hello 固定本连接就绪状态；身份在 Connect 时已由 Host 绑定。
func (c *Client) Hello(ctx context.Context) (protocol.HelloResult, error) {
	if err := ctx.Err(); err != nil {
		return protocol.HelloResult{}, err
	}
	c.boot.mu.Lock()
	defer c.boot.mu.Unlock()
	if err := c.checkLocked(false); err != nil {
		return protocol.HelloResult{}, err
	}
	if c.ready {
		return protocol.HelloResult{}, protocol.Failure(protocol.CodeFailedPrecondition)
	}
	nonce, err := protocol.NewNonce()
	if err != nil {
		return protocol.HelloResult{}, err
	}
	c.ready = true
	return protocol.HelloResult{Version: "0.1", Challenge: nonce, GuestBuild: c.boot.config.GuestBuild, ImageDigest: c.boot.config.ImageDigest}, nil
}

// Close 关闭本连接与授权，唤醒等待者但保留进程、缓冲与 boot 去重记录。
func (c *Client) Close() error {
	b := c.boot
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.done)
	if c.attachment != nil {
		c.attachment.closeLocked()
	}
	b.current = nil
	return nil
}

// notifyLocked 发布单个 session 的状态变化，调用方必须持有 boot.mu。
func (s *session) notifyLocked() { close(s.changed); s.changed = make(chan struct{}) }

// cloneInfo 返回不共享退出指针的独立快照。
func cloneInfo(info protocol.SessionInfo) protocol.SessionInfo {
	if info.Exit != nil {
		exit := *info.Exit
		info.Exit = &exit
	}
	return info
}

// findLocked 查询场景控制使用的 session，调用方必须持有 boot.mu。
func (b *Boot) findLocked(id protocol.OpaqueID) (*session, error) {
	s, ok := b.sessions[id]
	if !ok {
		return nil, protocol.Failure(protocol.CodeNotFound)
	}
	return s, nil
}

// Exit 模拟唯一主进程等待结果；groupRunning 显式描述残留组成员，不隐式发送 Fin。
func (b *Boot) Exit(exit protocol.SessionExit, groupRunning bool) error {
	if err := protocol.Validate(exit); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.findLocked(exit.SessionID)
	if err != nil {
		return err
	}
	if s.info.Exit != nil {
		if *s.info.Exit == exit {
			return nil
		}
		return protocol.Failure(protocol.CodeConflict)
	}
	s.info.State = protocol.SessionStateExited
	s.info.Exit = &exit
	if groupRunning {
		s.info.GroupState = protocol.GroupStateRunning
	} else {
		s.info.GroupState = protocol.GroupStateEmpty
	}
	s.notifyLocked()
	return nil
}

// IgnoreTERM 控制后续 Stop 是否必须等待宽限期，不影响已有退出结果。
func (b *Boot) IgnoreTERM(id protocol.OpaqueID, ignore bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.findLocked(id)
	if err != nil {
		return err
	}
	s.ignoreTERM = ignore
	return nil
}

// Window 返回最近一次初始尺寸或 Resize 的模拟结果。
func (b *Boot) Window(id protocol.OpaqueID) (protocol.Dimension, protocol.Dimension, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.findLocked(id)
	if err != nil {
		return 0, 0, err
	}
	return s.rows, s.cols, nil
}
