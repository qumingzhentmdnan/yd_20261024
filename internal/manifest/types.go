// Package manifest 加载内置清单并生成纯运行计划，不执行程序或写入文件。
package manifest

import "errors"

var (
	ErrUnknown  = errors.New("Manifest 引用不存在")
	ErrDigest   = errors.New("Manifest 摘要不匹配")
	ErrPlatform = errors.New("Manifest 不支持该平台")
	ErrManifest = errors.New("Manifest 内容无效")
	ErrLimit    = errors.New("Manifest 资源超过限额")
	ErrValues   = errors.New("Manifest 渲染输入无效")
)

const (
	maxSourceBytes    = 64 << 10
	maxNodes          = 2048
	maxDepth          = 16
	maxStringBytes    = 4096
	maxValueBytes     = 2048
	maxExpandedString = 16 << 10
	maxExpandedTotal  = 256 << 10
	maxFileBytes      = 256 << 10
	maxFilesBytes     = 1 << 20
	maxPlanBytes      = 2 << 20
)

// Platform 标识声明的 Guest 操作系统和架构，不表示已验证目标镜像。
type Platform struct {
	// OS 是 Guest 操作系统。
	OS string
	// Architecture 是 Guest CPU 架构。
	Architecture string
}

// Descriptor 描述已校验内置清单的可选择信息。
type Descriptor struct {
	// Ref 是完整 id@version，不接受自动版本选择。
	Ref string
	// DisplayName 是面向使用者的内置名称。
	DisplayName string
	// ManifestDigest 绑定 YAML、模板及预装程序字节。
	ManifestDigest string
	// Platforms 保存清单声明且本模块支持的平台。
	Platforms []Platform
	// ModelProtocol 描述清单声明的模型协议，由固定包登记约束。
	ModelProtocol string
}

// Values 只提供可信 Host 已选择的 Guest 布局与 Bridge 地址。
type Values struct {
	// Home 必须位于 Guest /home/agent 内，不是 Host HOME。
	Home string
	// Workspace 必须位于 Guest /workspace/project 内，不是 Host 源路径。
	Workspace string
	// ModelGatewayURL 是 Guest loopback HTTP Bridge 地址，不是上游地址。
	ModelGatewayURL string
}

// Environment 保存一个显式环境项，不继承父进程环境。
type Environment struct {
	// Name 是包策略允许的环境名称。
	Name string
	// Value 是一次替换后的字面值。
	Value string
}

// File 保存相对隔离 HOME 的配置产物，不代表已安全落盘。
type File struct {
	// Path 是加载时固定并校验的 HOME 相对路径。
	Path string
	// Format 是 json 或 toml。
	Format string
	// Mode 固定为 0600，后续安装器仍需使用受限根句柄。
	Mode uint32
	// Content 是该次渲染独立拥有的配置字节。
	Content []byte
}

// Plan 保存生成时完整运行产物，调用方修改后摘要不再有效。
type Plan struct {
	// Version 固定为运行计划版本 1。
	Version int
	// ManifestRef 标识原始内置清单版本。
	ManifestRef string
	// ManifestDigest 绑定清单及所有资源。
	ManifestDigest string
	// PackageRef 引用可信镜像必须提供的预装包。
	PackageRef string
	// Executable 是包白名单中的确切 Guest 路径。
	Executable string
	// ExecutableDigest 绑定预装程序字节，不代替安装时实际校验。
	ExecutableDigest string
	// OS 是被选择的 Guest 操作系统。
	OS string
	// Architecture 是被选择的 Guest 架构。
	Architecture string
	// User 固定为由未来 Guest 安装层解析的非 root 用户 agent。
	User string
	// Terminal 固定为 pty。
	Terminal string
	// Home 指定隔离 Guest HOME。
	Home string
	// Directory 指定 Guest 工作目录。
	Directory string
	// Args 是独立 argv，不经过 shell 拼接。
	Args []string
	// Environment 按 Name 排序，不包含继承的 Host 环境。
	Environment []Environment
	// Files 按相对路径排序，字节和切片不引用共享定义。
	Files []File
	// PlanDigest 绑定将此字段设为空字符串后的完整 JSON 字节。
	PlanDigest string
}
