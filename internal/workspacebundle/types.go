// Package workspacebundle 定义Host和Guest共用的规范工作区包。
package workspacebundle

import "errors"

var (
	ErrInvalidSelection    = errors.New("工作区选择无效")
	ErrLimitExceeded       = errors.New("工作区超过限额")
	ErrCorruptBaseline     = errors.New("基线内容损坏")
	ErrUnsupportedPlatform = errors.New("当前平台不支持工作区安全提取")
)

// Limits 限定一次快照及归档的资源使用。
type Limits struct {
	// MaxFiles 限制选择文件的数量，必须为正。
	MaxFiles int
	// MaxFileBytes 限制单文件字节数，必须为正。
	MaxFileBytes int64
	// MaxTotalBytes 限制所有文件内容的总字节数，必须为正。
	MaxTotalBytes int64
	// MaxPathBytes 限制相对路径的 UTF-8 字节数，必须为正。
	MaxPathBytes int
	// MaxArchiveBytes 限制完整归档字节数，必须为正。
	MaxArchiveBytes int64
}

// Entry 记录一个文件的规范元数据。
type Entry struct {
	// Path 使用斜杠分隔的相对文件路径。
	Path string `json:"path"`
	// Size 记录文件内容字节数。
	Size int64 `json:"size"`
	// Executable 表示是否保留可执行权限。
	Executable bool `json:"executable"`
	// Digest 记录文件内容的 SHA-256 摘要。
	Digest string `json:"digest"`
}

// Manifest 记录导入包的规范文件清单。
type Manifest struct {
	// Version 当前固定为 1。
	Version int `json:"version"`
	// Entries 按路径原始字节排序，空清单编码为数组。
	Entries []Entry `json:"entries"`
}

// GuestLimits 返回首版Guest固定上限，不修改调用方配置。
func GuestLimits() Limits {
	return Limits{MaxFiles: 4096, MaxFileBytes: 16 << 20, MaxTotalBytes: 64 << 20, MaxPathBytes: 1024, MaxArchiveBytes: 128 << 20}
}

// Intersect 返回两组明确限额的逐项较小值。
func Intersect(a, b Limits) Limits {
	return Limits{MaxFiles: min(a.MaxFiles, b.MaxFiles), MaxFileBytes: min(a.MaxFileBytes, b.MaxFileBytes), MaxTotalBytes: min(a.MaxTotalBytes, b.MaxTotalBytes), MaxPathBytes: min(a.MaxPathBytes, b.MaxPathBytes), MaxArchiveBytes: min(a.MaxArchiveBytes, b.MaxArchiveBytes)}
}
