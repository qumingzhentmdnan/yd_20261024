package protocol

import "safeagent/internal/workspacebundle"

const (
	MethodListChanges     Method = "ListChanges"
	MethodReadChangeChunk Method = "ReadChangeChunk"
	ExportChunkBytes             = 32768
)

// ListChangesParams 限定工作副本分页游标。
type ListChangesParams struct {
	// Cursor 是已排序清单的起始位置，零从头读取。
	Cursor int `json:"cursor"`
}

// ChangePage 仅返回受限相对路径元数据。
type ChangePage struct {
	// Entries 至多包含8个普通文件。
	Entries []workspacebundle.Entry `json:"entries"`
	// Next 为下一页位置，零表示结束。
	Next int `json:"next"`
}

// ReadChangeChunkParams 指定工作副本内的文件和有界偏移。
type ReadChangeChunkParams struct {
	// Path 是经过路径校验的工作副本相对路径。
	Path string `json:"path"`
	// Offset 是从文件头计算的字节偏移。
	Offset int64 `json:"offset"`
}

// ChangeChunk 保存原始文件数据，JSON使用base64。
type ChangeChunk struct {
	// Data 最多包含ExportChunkBytes个字节。
	Data []byte `json:"data"`
}
