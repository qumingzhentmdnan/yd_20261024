package fakevm

import (
	"context"
	"sync"
)

// Gate 通过 Entered 通知操作已到达，调用 Release 后放行，测试不依赖 sleep 猜时序。
// 到达通知与放行都只执行一次。
type Gate struct {
	// entered 在操作首次到达闸门时关闭。
	entered chan struct{}
	// released 在测试允许操作继续时关闭。
	released chan struct{}
	// enterOnce 保证到达通知只发送一次。
	enterOnce sync.Once
	// releaseOnce 保证重复放行不会重复关闭通道。
	releaseOnce sync.Once
}

// NewGate 创建尚未到达、尚未放行的测试闸门。
func NewGate() *Gate {
	return &Gate{entered: make(chan struct{}), released: make(chan struct{})}
}

// Entered 返回首次到达通知，供测试同步操作时序。
func (g *Gate) Entered() <-chan struct{} { return g.entered }

// Release 放行当前及之后到达闸门的操作。
func (g *Gate) Release() { g.releaseOnce.Do(func() { close(g.released) }) }

// wait 通知到达后等待放行，调用取消时立即结束等待。
func (g *Gate) wait(ctx context.Context) error {
	g.enterOnce.Do(func() { close(g.entered) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.released:
		return ctx.Err()
	}
}
