//go:build !linux

package workspace

import "context"

// Apply 在尚未实现安全写回的平台明确拒绝。
func (r *Review) Apply(ctx context.Context, approvedID string) error { return ErrUnsupportedPlatform }
