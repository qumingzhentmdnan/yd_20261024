// Package credential 提供仅驻留 Host 进程内存的会话凭证。
package credential

import (
	"context"
	"errors"
	"strings"
	"sync"
)

var (
	ErrInvalidRef    = errors.New("凭证引用无效")
	ErrInvalidSecret = errors.New("凭证内容无效")
	ErrNotFound      = errors.New("凭证不存在")
	ErrClosed        = errors.New("凭证存储已关闭")
)

// Store 保存宿主会话凭证，必须通过 New 创建；副本共享生命周期。
type Store struct {
	// state 间接保存敏感状态，使 Store 的值副本也能安全格式化。
	state *storeState
}

// storeState 保存同一会话共享的可变状态。
type storeState struct {
	// mu 串行化凭证访问与关闭。
	mu sync.Mutex
	// secrets 保存到删除、覆盖或关闭为止的凭证引用，关闭后为 nil。
	secrets map[string]string
	// closed 表示会话已关闭，零值表示尚未关闭。
	closed bool
}

// New 创建不读取环境、文件或外部凭证服务的内存存储。
func New() *Store {
	return &Store{state: &storeState{secrets: make(map[string]string)}}
}

// String 返回不包含引用和密钥的固定描述。
func (s Store) String() string { return "credential.Store（内容已隐藏）" }

// GoString 使 Go 语法格式化同样隐藏引用和密钥。
func (s Store) GoString() string { return s.String() }

// validRef 限定 Host 本地引用为字母数字开头及字母数字、点、下划线、连字符。
func validRef(ref string) bool {
	if ref == "" {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return false
	}
	return true
}

// Put 保存显式提供的密钥，已存在的引用被原子覆盖。
func (s *Store) Put(ctx context.Context, ref, secret string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validRef(ref) {
		return ErrInvalidRef
	}
	if secret == "" || strings.ContainsAny(secret, "\r\n\x00") {
		return ErrInvalidSecret
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	// 等待锁期间发生的取消不得提交更换。
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.state.closed {
		return ErrClosed
	}
	s.state.secrets[ref] = secret
	return nil
}

// Resolve 仅解析指定引用，取消或不存在时不返回任何密钥。
func (s *Store) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !validRef(ref) {
		return "", ErrInvalidRef
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.state.closed {
		return "", ErrClosed
	}
	secret, ok := s.state.secrets[ref]
	if !ok {
		return "", ErrNotFound
	}
	return secret, nil
}

// Delete 移除指定引用，未知引用视为已经删除。
func (s *Store) Delete(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validRef(ref) {
		return ErrInvalidRef
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.state.closed {
		return ErrClosed
	}
	delete(s.state.secrets, ref)
	return nil
}

// Close 幂等关闭存储并清除所持引用，不承诺物理擦除 Go 字符串。
func (s *Store) Close() error {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	s.state.closed = true
	s.state.secrets = nil
	return nil
}
