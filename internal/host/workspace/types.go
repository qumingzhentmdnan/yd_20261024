// Package workspace 保存 Host 侧工作区基线及确定性的导入包。
package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

var (
	ErrInvalidSelection    = errors.New("工作区选择无效")
	ErrLimitExceeded       = errors.New("工作区超过限额")
	ErrSourceChanged       = errors.New("复制期间源文件发生变化")
	ErrNotFound            = errors.New("基线不存在")
	ErrCorruptBaseline     = errors.New("基线内容损坏")
	ErrUnsupportedPlatform = errors.New("当前平台不支持工作区安全复制")
)

// BaselineID 标识已发布的工作区基线，不是路径。
type BaselineID string

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

// ImportSelection 记录调用方已确认的精确文件选择。
type ImportSelection struct {
	// Root 指定 Host 绝对源目录，不发送给 Guest。
	Root string
	// Paths 列出相对普通文件路径；空列表代表空工作区。
	Paths []string
}

// Baseline 描述已持久化的不可变快照。
type Baseline struct {
	// ID 作为基线查询标识，并可在导入成功后用作 workspaceRef。
	ID BaselineID `json:"id"`
	// SourceRef 在 Host 内部关联源目录，不是路径。
	SourceRef string `json:"sourceRef"`
	// Digest 绑定规范清单及其记录的文件摘要。
	Digest string `json:"digest"`
	// FileCount 记录普通文件数量。
	FileCount int `json:"fileCount"`
	// TotalBytes 记录文件内容总字节数。
	TotalBytes int64 `json:"totalBytes"`
	// ArchiveBytes 记录完整导入包字节数。
	ArchiveBytes int64 `json:"archiveBytes"`
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

// safeError 去除文件系统错误携带的绝对路径并保留底层原因。
func safeError(op, relative string, err error) error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		clean := make([]error, len(causes))
		for i, cause := range causes {
			clean[i] = safeError(op, relative, cause)
		}
		return errors.Join(clean...)
	}
	var pe *fs.PathError
	var le *os.LinkError
	if errors.As(err, &pe) {
		err = pe.Err
	} else if errors.As(err, &le) {
		err = le.Err
	}
	return fmt.Errorf("%s %q: %w", op, relative, err)
}
