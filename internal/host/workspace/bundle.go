package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"safeagent/internal/workspacebundle"
)

// contextReader 在每次底层读取前检查本次调用是否已取消。
type contextReader struct {
	// ctx 限制当前读取生命周期。
	ctx context.Context
	// reader 提供实际字节流。
	reader io.Reader
}

// Read 在取消后停止继续读取。
func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// digestBytes 计算带算法前缀的小写摘要。
func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// hashDigest 将流式哈希转换为公共摘要格式。
func hashDigest(h hash.Hash) string {
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// ValidateBundle 复用Host与Guest唯一的规范解析器。
func ValidateBundle(ctx context.Context, source io.Reader, limits Limits) (Manifest, error) {
	return workspacebundle.ValidateBundle(ctx, source, limits)
}
