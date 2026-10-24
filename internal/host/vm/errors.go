package vm

import "errors"

// 后端可为错误补充上下文，调用方用 errors.Is 判断；context 错误保留原有语义。
var (
	ErrInvalidConfig       = errors.New("vm: invalid configuration")
	ErrConflict            = errors.New("vm: configuration conflict")
	ErrNotFound            = errors.New("vm: not found")
	ErrInvalidState        = errors.New("vm: invalid state")
	ErrStaleInstance       = errors.New("vm: stale instance")
	ErrExitUnavailable     = errors.New("vm: exit result unavailable after recovery")
	ErrClosed              = errors.New("vm: manager closed")
	ErrShutdownUnsupported = errors.New("vm: graceful shutdown unsupported")
)
