//go:build !linux

package workspacebundle

import (
	"context"
	"io"
	"os"
)

// Extract 明确拒绝未验证平台，不使用普通路径操作替代受限根。
func Extract(context.Context, io.Reader, Limits, *os.File, int, int) (Manifest, error) {
	return Manifest{}, ErrUnsupportedPlatform
}
