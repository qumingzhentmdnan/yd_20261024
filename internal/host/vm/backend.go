// Package vm 定义宿主侧的虚拟机生命周期契约，不实现虚拟化、Guest RPC 或 Agent 管理。
package vm

import "context"

// VMBackend 管理持久资源及每次运行。实现须支持并发调用；context 必须非空，
// 取消后本次调用须在有界时间内返回。
type VMBackend interface {
	// Create 只准备私有资源。同标识、同配置返回已有资源，配置不同返回 ErrConflict。
	Create(context.Context, VMConfig) (VMRef, error)

	// Start 在 VMM 运行后返回，不等待 Guest 就绪。运行中重复调用返回同一实例；
	// 真正重启产生新 epoch。成功返回后取消 context 不停止 VM。
	Start(context.Context, VMRef) (VMInstance, error)

	// Inspect 查询 VM 状态，不能将查询失败当作已停止，也不返回 Agent 状态。
	// 允许观察到 Starting/Stopping，但不保证每次过渡都可见，完成条件以操作结果为准。
	Inspect(context.Context, VMRef) (VMStatus, error)

	// Stop 仅操作指定 epoch，VMM 退出后才成功，且保留磁盘。重复停止成功；
	// 资源存在但 epoch 不是当前实例时返回 ErrStaleInstance，包括未知 epoch。
	// 资源不存在时返回 ErrNotFound；失败或取消后须查询实际状态。
	Stop(context.Context, VMInstance, StopOptions) error

	// Wait 的并发调用获得相同退出结果；取消只结束本次等待。非零退出码放在
	// VMExit 中，不当作查询错误。结果跨重启保留，Destroy 成功后可释放。
	// 指定运行没有记录时返回 ErrNotFound，即使对应资源仍然存在。
	Wait(context.Context, VMInstance) (VMExit, error)

	// Destroy 只删除 Created 或 Stopped 的私有资源，不删除基础镜像；其他状态返回 ErrInvalidState。
	// 资源不存在时成功；清理失败时保留可查询、可重试的记录。
	Destroy(context.Context, VMRef) error
}
