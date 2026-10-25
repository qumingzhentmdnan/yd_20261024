package vm

import (
	"fmt"
	"path/filepath"
	"time"
)

type Architecture string

const (
	AMD64 Architecture = "amd64"
	ARM64 Architecture = "arm64"
)

type Machine string

const (
	MicroVM Machine = "microvm"
	Virt    Machine = "virt"
)

type Accelerator string

const (
	KVM  Accelerator = "kvm"
	HVF  Accelerator = "hvf"
	WHPX Accelerator = "whpx"
	TCG  Accelerator = "tcg"
)

// Image 由 Host 解析为可信路径，不直接接收 HTTP 输入。InitrdPath 可为空。
// Validate 只检查结构，真实后端负责文件存在性、来源与权限。
type Image struct {
	// KernelPath 是已由 Host 解析的内核绝对路径。
	KernelPath string `json:"kernel_path"`
	// InitrdPath 是可选 initramfs 的绝对路径，空值表示不使用。
	InitrdPath string `json:"initrd_path,omitempty"`
	// BaseDiskPath 是不可变基础磁盘的绝对路径，销毁 VM 时不得删除。
	BaseDiskPath string `json:"base_disk_path"`
}

// VMConfig 是 Host 内部配置，不开放任意 QEMU 参数、命令、共享目录或网络。
// 各平台的实际支持情况由真实后端检查。
type VMConfig struct {
	// ID 是稳定的资源标识，也是 Create 的去重键。
	ID string `json:"id"`
	// Architecture 是 Guest CPU 架构。
	Architecture Architecture `json:"architecture"`
	// Machine 是与 Guest 架构匹配的 QEMU 机器类型。
	Machine Machine `json:"machine"`
	// Accelerator 必须显式选择，不支持自动回退。
	Accelerator Accelerator `json:"accelerator"`
	// VCPUs 是分配给 Guest 的虚拟 CPU 数量。
	VCPUs uint16 `json:"vcpus"`
	// MemoryMiB 是分配给 Guest 的内存，单位为 MiB。
	MemoryMiB uint32 `json:"memory_mib"`
	// Image 是经过 Host 解析的镜像文件位置。
	Image Image `json:"image"`
}

// Validate 检查配置结构，不访问磁盘或探测虚拟化能力。
func (c VMConfig) Validate() error {
	// 先验证资源身份和架构组合，再检查运行参数与文件路径。
	if err := (VMRef{ID: c.ID}).Validate(); err != nil {
		return err
	}
	if !((c.Architecture == AMD64 && c.Machine == MicroVM) ||
		(c.Architecture == ARM64 && c.Machine == Virt)) {
		return fmt.Errorf("%w: architecture/machine combination", ErrInvalidConfig)
	}
	switch c.Accelerator {
	case KVM, HVF, WHPX, TCG:
	default:
		return fmt.Errorf("%w: accelerator must be explicit", ErrInvalidConfig)
	}
	if c.VCPUs == 0 || c.MemoryMiB == 0 {
		return fmt.Errorf("%w: CPU and memory must be positive", ErrInvalidConfig)
	}
	if !filepath.IsAbs(c.Image.KernelPath) || !filepath.IsAbs(c.Image.BaseDiskPath) {
		return fmt.Errorf("%w: kernel/base disk paths must be absolute", ErrInvalidConfig)
	}
	if c.Image.InitrdPath != "" && !filepath.IsAbs(c.Image.InitrdPath) {
		return fmt.Errorf("%w: initrd path must be absolute", ErrInvalidConfig)
	}
	return nil
}

// VMRef 标识持久资源，不随每次启动改变。
type VMRef struct {
	// ID 对应创建时的稳定资源标识。
	ID string
}

// Validate 检查资源标识的长度和允许字符，阻止路径形式的标识。
func (r VMRef) Validate() error {
	if len(r.ID) == 0 || len(r.ID) > 64 {
		return fmt.Errorf("%w: VM ID length must be 1..64", ErrInvalidConfig)
	}
	for i, ch := range []byte(r.ID) {
		alphaNum := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
		if !alphaNum && (i == 0 || ch != '-' && ch != '_') {
			return fmt.Errorf("%w: VM ID contains invalid characters", ErrInvalidConfig)
		}
	}
	return nil
}

// VMInstance 标识一次运行。即使销毁后重新创建，同一 VMRef 也不能复用 epoch。
type VMInstance struct {
	// Ref 指向本次运行使用的持久资源。
	Ref VMRef
	// Epoch 由生命周期实现为本次运行生成，不能跨启动复用。
	Epoch string
}

// Validate 检查实例是否包含有效资源标识和非空 epoch。
func (i VMInstance) Validate() error {
	if err := i.Ref.Validate(); err != nil {
		return err
	}
	if i.Epoch == "" {
		return fmt.Errorf("%w: empty epoch", ErrInvalidConfig)
	}
	return nil
}

type VMState string

const (
	StateCreated  VMState = "created"
	StateStarting VMState = "starting"
	StateRunning  VMState = "running"
	StateStopping VMState = "stopping"
	StateStopped  VMState = "stopped"
	StateUnknown  VMState = "unknown"
)

// VMStatus 是查询快照。首次启动前 Instance 为零值；已观察到真实退出结果时 Exit 有值。
// 返回的指针不能引用可修改的内部状态；Unknown 不能作为安全删除的依据。
type VMStatus struct {
	// Ref 标识本次查询的资源。
	Ref VMRef
	// State 是当前观察到的 VM 生命周期状态。
	State VMState
	// Instance 是当前或最近一次运行，首次启动前为零值。
	Instance VMInstance
	// Exit 是已经观察到的退出结果，未退出或恢复后结果丢失时为空。
	Exit *VMExit
}

// StopOptions 要求正数宽限期。不支持优雅关机时返回 ErrShutdownUnsupported；
// 只有显式允许 ForceAfterTimeout 才可立即强停。实现须遵守更早的 context 时限。
type StopOptions struct {
	// GracePeriod 是等待优雅关机的最大时长，必须为正数。
	GracePeriod time.Duration
	// ForceAfterTimeout 表示允许在超时或不支持优雅关机时强制终止。
	ForceAfterTimeout bool
}

// Validate 检查调用方是否提供了有界的正数宽限期。
func (o StopOptions) Validate() error {
	if o.GracePeriod <= 0 {
		return fmt.Errorf("%w: grace period must be positive", ErrInvalidConfig)
	}
	return nil
}

type ExitReason string

const (
	ExitProcess ExitReason = "process-exited"
	ExitStopped ExitReason = "stopped"
	ExitForced  ExitReason = "forced"
)

// VMExit 描述 VMM 的退出结果。被信号终止等没有正常退出码的情况使用 Code=-1；
// 正常退出时 Signal 为空。这里不描述 Agent 进程。
type VMExit struct {
	// Instance 标识已经结束的那一次运行。
	Instance VMInstance
	// Code 是 VMM 的退出码，没有正常退出码时为 -1。
	Code int
	// Signal 是终止信号，正常退出时为空。
	Signal string
	// Reason 区分自行退出、请求停止和强制终止。
	Reason ExitReason
}
