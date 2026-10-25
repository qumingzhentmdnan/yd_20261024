package vm

import (
	"context"
	"io"
)

// Driver 提供 Manager 使用的后端执行边界，不分配 epoch 或保存生命周期历史。
type Driver interface {
	// Acquire 取得覆盖 Manager 生命周期的资源独占锁，关闭句柄释放。
	Acquire() (io.Closer, error)
	// Identity 返回不可变的资源根目录身份，防止数据库绑定另一套磁盘。
	Identity() string
	// Prepare 幂等准备或核验私有资源，不能覆盖已有差分盘。
	Prepare(context.Context, VMConfig) error
	// Launch 使用已持久化的实例启动 VMM；错误返回时保证没有本次遗留进程。
	Launch(context.Context, VMConfig, VMInstance) (Process, error)
	// Remove 只删除该后端拥有的私有资源，不删除基础盘。
	Remove(context.Context, VMRef) error
}

// Process 是一次运行的句柄，真实退出只回收一次。
type Process interface {
	// Stop 请求停止 VMM；请求失败不代表进程退出。
	Stop(StopOptions) error
	// Wait 返回真实退出结果，不受调用方等待取消影响。
	Wait() (VMExit, error)
}
