package credential

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"safeagent/internal/host/gateway"
)

var _ gateway.CredentialStore = (*Store)(nil)

// checkedContext 在首次读取取消状态后通知测试，保留该次检查的结果。
type checkedContext struct {
	// Context 提供实际取消状态及其余上下文行为。
	context.Context
	// checked 在首次 Err 读取完成时关闭。
	checked chan struct{}
	// once 保证后续锁内检查不会重复关闭信号。
	once sync.Once
}

// Err 先读取底层状态再发出信号，使测试可在首次检查确定后取消。
func (c *checkedContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

// TestLifecycle 验证覆盖、删除、关闭及关闭后的不可用状态。
func TestLifecycle(t *testing.T) {
	s := New()
	ctx := context.Background()
	for _, secret := range []string{"fake-first", "fake-replacement"} {
		if err := s.Put(ctx, "host.key-1", secret); err != nil {
			t.Fatal(err)
		}
		got, err := s.Resolve(ctx, "host.key-1")
		if err != nil || got != secret {
			t.Fatal("解析未返回当前假凭证")
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.Delete(ctx, "host.key-1"); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.Resolve(ctx, "host.key-1"); got != "" || !errors.Is(err, ErrNotFound) {
		t.Fatal("删除未生效")
	}
	if err := s.Put(ctx, "key", "fake-secret"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if s.state.secrets != nil {
		t.Fatal("关闭仍持有凭证表")
	}
	if err := s.Put(ctx, "key", "fake-secret"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "key"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if got, err := s.Resolve(ctx, "key"); got != "" || !errors.Is(err, ErrClosed) {
		t.Fatal("关闭后仍可解析")
	}
}

// TestInputAndFormatting 验证输入边界及错误和常见格式化不暴露假密钥。
func TestInputAndFormatting(t *testing.T) {
	s := New()
	ctx := context.Background()
	secret := "fake-sensitive-marker"
	if err := s.Put(ctx, "key", secret); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{s, *s} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			if strings.Contains(fmt.Sprintf(format, value), secret) {
				t.Fatal("格式化泄露凭证")
			}
		}
	}
	for _, ref := range []string{"", " key", "key ", ".key", "a/b", "a:b", "中文", "a\r", "a\n", "a\x00"} {
		if err := s.Put(ctx, ref, secret); !errors.Is(err, ErrInvalidRef) {
			t.Fatal("接受无效引用")
		}
		if _, err := s.Resolve(ctx, ref); !errors.Is(err, ErrInvalidRef) {
			t.Fatal("接受无效解析引用")
		}
		if err := s.Delete(ctx, ref); !errors.Is(err, ErrInvalidRef) {
			t.Fatal("接受无效删除引用")
		}
	}
	for _, bad := range []string{"", secret + "\r", secret + "\n", secret + "\x00"} {
		err := s.Put(ctx, "key", bad)
		if !errors.Is(err, ErrInvalidSecret) || strings.Contains(err.Error(), secret) {
			t.Fatal("密钥校验或错误脱敏失败")
		}
	}
	if got, _ := s.Resolve(ctx, "key"); got != secret {
		t.Fatal("无效输入改变了现有密钥")
	}
}

// TestCancellation 验证预先取消、超时和等待锁期间取消均不改变凭证。
func TestCancellation(t *testing.T) {
	s := New()
	ctx := context.Background()
	if err := s.Put(ctx, "key", "fake-original"); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer stop()
	for _, c := range []context.Context{canceled, expired} {
		if err := s.Put(c, "key", "fake-new"); !errors.Is(err, c.Err()) {
			t.Fatal(err)
		}
		if got, err := s.Resolve(c, "key"); got != "" || !errors.Is(err, c.Err()) {
			t.Fatal("取消解析返回了凭证")
		}
		if err := s.Delete(c, "key"); !errors.Is(err, c.Err()) {
			t.Fatal(err)
		}
	}
	for _, operation := range []func(context.Context) error{
		func(c context.Context) error { return s.Put(c, "key", "fake-new") },
		func(c context.Context) error { _, err := s.Resolve(c, "key"); return err },
		func(c context.Context) error { return s.Delete(c, "key") },
	} {
		waiting, cancel := context.WithCancel(ctx)
		checked := &checkedContext{Context: waiting, checked: make(chan struct{})}
		s.state.mu.Lock()
		done := make(chan error, 1)
		go func() { done <- operation(checked) }()
		// 首次检查已读取未取消状态，持锁期间取消只能由锁后检查观察。
		<-checked.checked
		cancel()
		s.state.mu.Unlock()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if got, _ := s.Resolve(ctx, "key"); got != "fake-original" {
		t.Fatal("取消操作改变了密钥")
	}
}

// TestConcurrentLifecycle 验证并发读写删除和关闭仅返回约定结果。
func TestConcurrentLifecycle(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	ready.Add(16)
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Put(context.Background(), "key", "fake-secret"); err != nil {
				t.Error(err)
			}
			ready.Done()
			<-start
			for j := 0; j < 100; j++ {
				if err := s.Put(context.Background(), "key", "fake-secret"); err != nil && !errors.Is(err, ErrClosed) {
					t.Error(err)
				}
				got, err := s.Resolve(context.Background(), "key")
				if err == nil && got != "fake-secret" {
					t.Error("读取到未登记值")
				}
				if err != nil && !errors.Is(err, ErrClosed) && !errors.Is(err, ErrNotFound) {
					t.Error(err)
				}
				if err := s.Delete(context.Background(), "key"); err != nil && !errors.Is(err, ErrClosed) {
					t.Error(err)
				}
			}
		}()
	}
	ready.Wait()
	close(start)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}
