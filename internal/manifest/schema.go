package manifest

// manifestSpec 是清单支持的精确字段，不开放任意执行扩展。
type manifestSpec struct {
	// APIVersion 固定为 safeagent.io/v1。
	APIVersion string `yaml:"apiVersion"`
	// Kind 固定为 AgentManifest。
	Kind string `yaml:"kind"`
	// Metadata 绑定内置引用、平台和来源。
	Metadata metadataSpec `yaml:"metadata"`
	// Runtime 描述固定执行计划。
	Runtime runtimeSpec `yaml:"runtime"`
	// Files 描述相对隔离 HOME 的配置产物。
	Files []fileSpec `yaml:"files"`
	// Capabilities 仅描述本轮允许的测试协议。
	Capabilities capabilitiesSpec `yaml:"capabilities"`
}

// metadataSpec 保存清单身份与支持声明。
type metadataSpec struct {
	// ID 是内置名称。
	ID string `yaml:"id"`
	// Version 是确切三段版本。
	Version string `yaml:"version"`
	// DisplayName 是展示名称。
	DisplayName string `yaml:"displayName"`
	// Support 保存平台及终端声明。
	Support supportSpec `yaml:"support"`
	// Provenance 引用内置包策略。
	Provenance provenanceSpec `yaml:"provenance"`
}

// supportSpec 限定本轮已支持的平台声明。
type supportSpec struct {
	// OS 本轮只接受 linux。
	OS string `yaml:"os"`
	// Architectures 本轮只接受 amd64。
	Architectures []string `yaml:"architectures"`
	// Modes 本轮只接受 pty。
	Modes []string `yaml:"modes"`
}

// provenanceSpec 将清单绑定到可信包登记。
type provenanceSpec struct {
	// Source 必须为 builtin。
	Source string `yaml:"source"`
	// PackageRef 必须与包登记一致。
	PackageRef string `yaml:"packageRef"`
}

// runtimeSpec 描述不含宿主权限或命令解释器的启动数据。
type runtimeSpec struct {
	// Executable 必须等于包策略中的 Guest 可执行路径。
	Executable string `yaml:"executable"`
	// Args 保存不会交给 shell 解释的参数。
	Args []string `yaml:"args"`
	// Cwd 必须为工作区变量。
	Cwd string `yaml:"cwd"`
	// Terminal 固定为 pty。
	Terminal string `yaml:"terminal"`
	// User 固定为 agent。
	User string `yaml:"user"`
	// Environment 只接受包策略的名称及有限变量。
	Environment map[string]string `yaml:"environment"`
}

// fileSpec 描述静态目标及内置结构模板。
type fileSpec struct {
	// Destination 必须以 home 变量开头，后缀不得含变量。
	Destination string `yaml:"destination"`
	// Template 指向 templates 内的 JSON 结构文件。
	Template string `yaml:"template"`
	// Format 指定最终配置序列化格式。
	Format string `yaml:"format"`
}

// capabilitiesSpec 声明本轮配置协议，不表示真实 CLI 兼容验证。
type capabilitiesSpec struct {
	// Model 仅描述固定测试 HTTP 协议。
	Model modelSpec `yaml:"model"`
}

// modelSpec 保存模型协议标签。
type modelSpec struct {
	// Protocol 描述固定包使用的模型协议。
	Protocol string `yaml:"protocol"`
}
