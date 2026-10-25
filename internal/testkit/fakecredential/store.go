// Package fakecredential 只解析调用方显式提供的测试凭证，不访问环境或 Vault。
package fakecredential

import (
	"context"
	"errors"
	"maps"
	"sync"
)

// ErrNotFound 表示测试未登记该凭证引用。
var ErrNotFound = errors.New("测试凭证不存在")

// Store 保存固定假值及可观察调用计数。
type Store struct {
	// mu 保护调用计数。
	mu sync.Mutex
	// values 保存构造时复制的显式假凭证。
	values map[string]string
	// calls 统计 Resolve 调用次数，包括未知引用。
	calls int
}

// New 复制测试登记内容，不设置任何默认凭证。
func New(values map[string]string) *Store {
	return &Store{values: maps.Clone(values)}
}

// Resolve 查询固定假值，未知引用报错，取消不访问登记表。
func (s *Store) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	value, exists := s.values[ref]
	if !exists {
		return "", ErrNotFound
	}
	return value, nil
}

// Calls 返回独立的调用计数快照。
func (s *Store) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
