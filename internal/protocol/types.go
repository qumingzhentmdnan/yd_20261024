// Package protocol 定义 Host、Guest 与 Fake 共用的候选线协议。
package protocol

// OpaqueID 表示候选契约中的 OpaqueID 值；具体约束由 Validate 检查。
type OpaqueID string

// VMRef 表示候选契约中的 VMRef 值；具体约束由 Validate 检查。
type VMRef string

// Epoch 表示候选契约中的 Epoch 值；具体约束由 Validate 检查。
type Epoch string

// UUID 表示候选契约中的 UUID 值；具体约束由 Validate 检查。
type UUID string

// Nonce 表示候选契约中的 Nonce 值；具体约束由 Validate 检查。
type Nonce string

// Digest 表示候选契约中的 Digest 值；具体约束由 Validate 检查。
type Digest string

// Sequence 表示候选契约中的 Sequence 值；具体约束由 Validate 检查。
type Sequence uint64

// StreamID 表示候选契约中的 StreamID 值；具体约束由 Validate 检查。
type StreamID uint32

// Dimension 表示候选契约中的 Dimension 值；具体约束由 Validate 检查。
type Dimension uint16

// Counter 使用十进制 JSON 字符串传递累计计数，允许零值。
type Counter uint64

// Millis 表示必填的 1..30000 毫秒预算。
type Millis uint32

// Method 限定候选契约的 Method 枚举。
type Method string

const (
	MethodHello          Method = "Hello"
	MethodGetState       Method = "GetState"
	MethodPrepareAgent   Method = "PrepareAgent"
	MethodStartSession   Method = "StartSession"
	MethodAttachTerminal Method = "AttachTerminal"
	MethodResizeTerminal Method = "ResizeTerminal"
	MethodDetachTerminal Method = "DetachTerminal"
	MethodStopSession    Method = "StopSession"
)

// Code 限定候选契约的 Code 枚举。
type Code string

const (
	CodeInvalidArgument    Code = "INVALID_ARGUMENT"
	CodeUnimplemented      Code = "UNIMPLEMENTED"
	CodeFailedPrecondition Code = "FAILED_PRECONDITION"
	CodeStaleInstance      Code = "STALE_INSTANCE"
	CodeConflict           Code = "CONFLICT"
	CodeNotFound           Code = "NOT_FOUND"
	CodeResourceExhausted  Code = "RESOURCE_EXHAUSTED"
	CodeCancelled          Code = "CANCELLED"
	CodeDeadlineExceeded   Code = "DEADLINE_EXCEEDED"
	CodeInternal           Code = "INTERNAL"
)

// SessionState 限定候选契约的 SessionState 枚举。
type SessionState string

const (
	SessionStateRunning SessionState = "running"
	SessionStateExited  SessionState = "exited"
)

// GroupState 限定候选契约的 GroupState 枚举。
type GroupState string

const (
	GroupStateRunning GroupState = "running"
	GroupStateEmpty   GroupState = "empty"
	GroupStateUnknown GroupState = "unknown"
)

// AttachmentState 限定候选契约的 AttachmentState 枚举。
type AttachmentState string

const (
	AttachmentStateAwaitingBind AttachmentState = "awaiting-bind"
	AttachmentStateBound        AttachmentState = "bound"
	AttachmentStateDetaching    AttachmentState = "detaching"
)

// ExitReason 限定候选契约的 ExitReason 枚举。
type ExitReason string

const (
	ExitReasonProcessExited ExitReason = "process-exited"
	ExitReasonStopped       ExitReason = "stopped"
	ExitReasonForced        ExitReason = "forced"
)

// Error 定义 Error 的精确负载字段。
type Error struct {
	// Code 记录正常退出码，信号退出为 -1。
	Code Code `json:"code"`
	// Message 提供不含秘密的错误文本，最多 256 字符。
	Message string `json:"message"`
}

// SessionExit 定义 Exit 的精确负载字段。
type SessionExit struct {
	// SessionID 标识 boot 内不复用的工作负载。
	SessionID OpaqueID `json:"sessionID"`
	// Code 记录正常退出码，信号退出为 -1。
	Code int `json:"code"`
	// Signal 记录 Linux 符号信号名，正常退出为空。
	Signal string `json:"signal"`
	// Reason 区分自行退出、停止和强停。
	Reason ExitReason `json:"reason"`
}

// SessionInfo 定义 SessionInfo 的精确负载字段。
type SessionInfo struct {
	// SessionID 标识 boot 内不复用的工作负载。
	SessionID OpaqueID `json:"sessionID"`
	// PreparedID 引用不可变准备记录。
	PreparedID OpaqueID `json:"preparedID"`
	// WorkspaceRef 引用已完成导入的工作区。
	WorkspaceRef OpaqueID `json:"workspaceRef"`
	// WorkspaceDigest 绑定工作区内容摘要。
	WorkspaceDigest Digest `json:"workspaceDigest"`
	// State 表示主进程或授权状态。
	State SessionState `json:"state"`
	// GroupState 描述进程组，unknown 不能当作已清空。
	GroupState GroupState `json:"groupState"`
	// Exit 保存固定主进程退出结果，运行中为空。
	Exit *SessionExit `json:"exit"`
}

// Preparation 定义 Preparation 的精确负载字段。
type Preparation struct {
	// PreparedID 引用不可变准备记录。
	PreparedID OpaqueID `json:"preparedID"`
	// ManifestRef 引用镜像登记的 Manifest。
	ManifestRef OpaqueID `json:"manifestRef"`
	// ManifestDigest 绑定 Manifest 内容摘要。
	ManifestDigest Digest `json:"manifestDigest"`
	// ConfigDigest 绑定已登记限域配置。
	ConfigDigest Digest `json:"configDigest"`
	// WorkspaceRef 引用已完成导入的工作区。
	WorkspaceRef OpaqueID `json:"workspaceRef"`
	// WorkspaceDigest 绑定工作区内容摘要。
	WorkspaceDigest Digest `json:"workspaceDigest"`
}

// GuestState 定义 GuestState 的精确负载字段。
type GuestState struct {
	// Prepared 保存最近准备结果，尚未准备时为空。
	Prepared *Preparation `json:"prepared"`
	// Sessions 保留 boot 内全部历史，最多 64 个。
	Sessions []SessionInfo `json:"sessions"`
	// CurrentSessionID 指向最近创建的 session，未创建时为空。
	CurrentSessionID *OpaqueID `json:"currentSessionID"`
	// Attachment 保存当前连接授权，不暴露 token，未授权时为空。
	Attachment *AttachmentInfo `json:"attachment"`
}

// AttachmentInfo 定义 AttachmentInfo 的精确负载字段。
type AttachmentInfo struct {
	// SessionID 标识 boot 内不复用的工作负载。
	SessionID OpaqueID `json:"sessionID"`
	// StreamID 标识连接内不复用的终端授权。
	StreamID StreamID `json:"streamID"`
	// ConnectionID 标识当前控制连接。
	ConnectionID UUID `json:"connectionID"`
	// State 表示主进程或授权状态。
	State AttachmentState `json:"state"`
}

// HelloParams 定义 HelloParams 的精确负载字段。
type HelloParams struct {
	// VMRef 表示协议字段。
	VMRef VMRef `json:"vmRef"`
	// Challenge 关联本次握手，不是远程证明。
	Challenge Nonce `json:"challenge"`
	// SupportedVersions 声明候选版本，当前只能包含 0.1。
	SupportedVersions []string `json:"supportedVersions"`
}

// PrepareAgentParams 定义 PrepareAgentParams 的精确负载字段。
type PrepareAgentParams struct {
	// OperationID 标识 boot 内可重试操作。
	OperationID UUID `json:"operationID"`
	// ManifestRef 引用镜像登记的 Manifest。
	ManifestRef OpaqueID `json:"manifestRef"`
	// ManifestDigest 绑定 Manifest 内容摘要。
	ManifestDigest Digest `json:"manifestDigest"`
	// ConfigDigest 绑定已登记限域配置。
	ConfigDigest Digest `json:"configDigest"`
	// WorkspaceRef 引用已完成导入的工作区。
	WorkspaceRef OpaqueID `json:"workspaceRef"`
	// WorkspaceDigest 绑定工作区内容摘要。
	WorkspaceDigest Digest `json:"workspaceDigest"`
}

// StartSessionParams 定义 StartSessionParams 的精确负载字段。
type StartSessionParams struct {
	// OperationID 标识 boot 内可重试操作。
	OperationID UUID `json:"operationID"`
	// SessionID 标识 boot 内不复用的工作负载。
	SessionID OpaqueID `json:"sessionID"`
	// PreparedID 引用不可变准备记录。
	PreparedID OpaqueID `json:"preparedID"`
	// WorkspaceRef 引用已完成导入的工作区。
	WorkspaceRef OpaqueID `json:"workspaceRef"`
	// WorkspaceDigest 绑定工作区内容摘要。
	WorkspaceDigest Digest `json:"workspaceDigest"`
	// Rows 表示 PTY 行数，范围 1..65535。
	Rows Dimension `json:"rows"`
	// Cols 表示 PTY 列数，范围 1..65535。
	Cols Dimension `json:"cols"`
}

// AttachTerminalParams 定义 AttachTerminalParams 的精确负载字段。
type AttachTerminalParams struct {
	// SessionID 标识 boot 内不复用的工作负载。
	SessionID OpaqueID `json:"sessionID"`
	// StreamID 标识连接内不复用的终端授权。
	StreamID StreamID `json:"streamID"`
}

// ResizeTerminalParams 定义 ResizeTerminalParams 的精确负载字段。
type ResizeTerminalParams struct {
	// SessionID 标识 boot 内不复用的工作负载。
	SessionID OpaqueID `json:"sessionID"`
	// StreamID 标识连接内不复用的终端授权。
	StreamID StreamID `json:"streamID"`
	// Rows 表示 PTY 行数，范围 1..65535。
	Rows Dimension `json:"rows"`
	// Cols 表示 PTY 列数，范围 1..65535。
	Cols Dimension `json:"cols"`
}

// DetachTerminalParams 定义 DetachTerminalParams 的精确负载字段。
type DetachTerminalParams struct {
	// SessionID 标识 boot 内不复用的工作负载。
	SessionID OpaqueID `json:"sessionID"`
	// StreamID 标识连接内不复用的终端授权。
	StreamID StreamID `json:"streamID"`
}

// StopSessionParams 定义 StopSessionParams 的精确负载字段。
type StopSessionParams struct {
	// SessionID 标识 boot 内不复用的工作负载。
	SessionID OpaqueID `json:"sessionID"`
	// GraceMs 限定 TERM 后等待时间，单位毫秒。
	GraceMs Millis `json:"graceMs"`
	// ForceAfterTimeout 显式允许宽限期届满后发送 KILL。
	ForceAfterTimeout bool `json:"forceAfterTimeout"`
}

// HelloResult 定义 HelloResult 的精确负载字段。
type HelloResult struct {
	// Version 固定为候选版本 0.1。
	Version string `json:"version"`
	// Challenge 关联本次握手，不是远程证明。
	Challenge Nonce `json:"challenge"`
	// GuestBuild 记录 Guest 构建标识。
	GuestBuild OpaqueID `json:"guestBuild"`
	// ImageDigest 提供镜像诊断摘要，不是身份凭据。
	ImageDigest Digest `json:"imageDigest"`
}

// AttachTerminalResult 定义 AttachTerminalResult 的精确负载字段。
type AttachTerminalResult struct {
	// StreamID 标识连接内不复用的终端授权。
	StreamID StreamID `json:"streamID"`
	// BindToken 授权一次 Bind，不得跨连接复用。
	BindToken Nonce `json:"bindToken"`
}

// Bind 定义 Bind 的精确负载字段。
type Bind struct {
	// SessionID 标识 boot 内不复用的工作负载。
	SessionID OpaqueID `json:"sessionID"`
	// Epoch 绑定现有 VM 启动代次，不能重新生成。
	Epoch Epoch `json:"epoch"`
	// ConnectionID 标识当前控制连接。
	ConnectionID UUID `json:"connectionID"`
	// BindToken 授权一次 Bind，不得跨连接复用。
	BindToken Nonce `json:"bindToken"`
}

// Bound 定义 Bound 的精确负载字段。
type Bound struct {
	// DroppedBytes 累计未绑定时丢弃的最旧输出字节数，零表示未发生此类丢弃。
	DroppedBytes Counter `json:"droppedBytes"`
}

// Empty 表示必须编码为空对象的参数或成功结果。
type Empty struct{}
