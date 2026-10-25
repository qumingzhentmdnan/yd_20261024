// Package workspace 保存 Host 侧工作区基线及确定性的导入包。
package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"safeagent/internal/workspacebundle"
)

var (
	ErrInvalidSelection    = workspacebundle.ErrInvalidSelection
	ErrLimitExceeded       = workspacebundle.ErrLimitExceeded
	ErrSourceChanged       = errors.New("复制期间源文件发生变化")
	ErrNotFound            = errors.New("基线不存在")
	ErrCorruptBaseline     = workspacebundle.ErrCorruptBaseline
	ErrUnsupportedPlatform = errors.New("当前平台不支持工作区安全复制")
)

// BaselineID 标识已发布的工作区基线，不是路径。
type BaselineID string

// Limits 复用Host与Guest统一的资源限额。
type Limits = workspacebundle.Limits

// ImportSelection 记录调用方已确认的精确文件选择。
type ImportSelection struct {
	// Root 指定 Host 绝对源目录，不发送给 Guest。
	Root string
	// Paths 列出相对普通文件路径；空列表代表空工作区。
	Paths []string
	// Generated 保存仅导入 Guest 的公开运行配置，不能与源文件重名，也不参与导出。
	Generated map[string][]byte
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

// Entry 复用规范包文件元数据。
type Entry = workspacebundle.Entry

// Manifest 复用规范包清单。
type Manifest = workspacebundle.Manifest

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
