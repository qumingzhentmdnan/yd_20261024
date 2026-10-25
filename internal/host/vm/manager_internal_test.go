package vm

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitContext 记录 Wait 已进入等待选择，保证关闭竞争测试没有提前返回。
type waitContext struct {
	// Context 提供无取消的父上下文。
	context.Context
	// entered 在 Wait 读取 Done 时发布一次。
	entered chan struct{}
	// once 保证等待通知只发送一次。
	once sync.Once
}

// Done 通知测试等待者已经经过资源查询阶段。
func (c *waitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return nil
}

// TestWaitAlreadyBlockedSurvivesClose 精确构造等待者已阻塞、结果提交、Close、等待者恢复的顺序。
func TestWaitAlreadyBlockedSurvivesClose(t *testing.T) {
	epoch := "wait-test"
	i := VMInstance{Ref: VMRef{ID: "waiter"}, Epoch: epoch}
	run := &managedRun{done: make(chan struct{})}
	m := &Manager{locks: make(map[string]chan struct{}), running: map[VMInstance]*managedRun{i: run}}
	ctx := &waitContext{Context: context.Background(), entered: make(chan struct{})}
	result := make(chan VMExit, 1)
	errs := make(chan error, 1)
	go func() { exit, err := m.waitRun(ctx, i, run); result <- exit; errs <- err }()
	<-ctx.entered
	// 同时阻止资源锁与 Manager 锁；若 Wait 仍试图重新取锁，会在 closed 检查失败。
	m.mu.Lock()
	m.closed = true
	run.exit = VMExit{Instance: i, Code: 7, Reason: ExitProcess}
	run.persisted.Store(true)
	close(run.done)
	m.mu.Unlock()
	select {
	case exit := <-result:
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if exit != run.exit {
			t.Fatalf("fixed exit changed: %+v", exit)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter blocked after close")
	}
}
