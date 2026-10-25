// Package guest 定义 Host 消费的 Guest 控制接口，不实现真实设备或进程。
package guest

import (
	"context"
	"errors"

	"safeagent/internal/protocol"
)

var (
	ErrDisconnected     = errors.New("guest connection disconnected")
	ErrInputClosed      = errors.New("terminal input closed")
	ErrOutputIncomplete = errors.New("terminal output incomplete")
)

// Client 绑定一个已由 Host 确认的 VMInstance，身份不能由业务参数覆盖。
type Client interface {
	// Hello 完成本连接握手，重复握手返回 FAILED_PRECONDITION。
	Hello(context.Context) (protocol.HelloResult, error)
	// PrepareAgent 固定已登记 Manifest、配置与工作区的准备结果。
	PrepareAgent(context.Context, protocol.PrepareAgentParams) (protocol.Preparation, error)
	// StartSession 按 boot 内 operationID 去重创建工作负载。
	StartSession(context.Context, protocol.StartSessionParams) (protocol.SessionInfo, error)
	// GetState 获取含历史退出结果的独立快照。
	GetState(context.Context) (protocol.GuestState, error)
	// AttachTerminal 分配流并完成 Bind/Bound 后返回终端。
	AttachTerminal(context.Context, protocol.OpaqueID) (Terminal, error)
	// StopSession 停止进程组并观察主进程退出与组清空。
	StopSession(context.Context, protocol.StopSessionParams) error
	// WaitSession 等待固定主进程退出，取消不影响其他等待者。
	WaitSession(context.Context, protocol.OpaqueID) (protocol.SessionExit, error)
	// Close 失效本连接并唤醒等待者，不停止工作负载。
	Close() error
}

// Terminal 代表已绑定的单个原始字节终端，不按行或 UTF-8 字符分割。
type Terminal interface {
	// Read 返回原始输出，只有正常 Guest Fin 才返回 EOF。
	Read([]byte) (int, error)
	// Write 发送原始输入，遵守本 attachment 的有界背压。
	Write([]byte) (int, error)
	// Resize 通过控制面改变当前 PTY 尺寸。
	Resize(context.Context, protocol.Dimension, protocol.Dimension) error
	// CloseWrite 发送一次 Host Fin，保存首次尝试结果（包括调用前取消），不停止进程。
	CloseWrite(context.Context) error
	// Close 有界释放授权并结束本地读写，不停止进程。
	Close() error
}
