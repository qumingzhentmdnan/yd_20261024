package fakeguest

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCloseWriteTimeoutIsStable 验证等待输入写锁超时后释放背压也不能改写首次结果。
func TestCloseWriteTimeoutIsStable(t *testing.T) {
	// 空令牌通道表示先前 Write 仍持有输入锁；此场景不会触及 session。
	a := &terminal{client: &Client{boot: &Boot{}, done: make(chan struct{})}, done: make(chan struct{}), writeToken: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := a.CloseWrite(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	a.writeToken <- struct{}{}
	if err := a.CloseWrite(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if a.inputClosed {
		t.Fatal("失败的 CloseWrite 关闭了输入")
	}
}
