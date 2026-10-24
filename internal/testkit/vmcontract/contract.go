// Package vmcontract 提供共用生命周期测试，真实后端须通过显式集成测试入口接入。
package vmcontract

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"safeagent/internal/host/vm"
)

// Factory 返回独立后端和有效配置。真实后端自行提供本地镜像，不在测试中下载。
type Factory func(*testing.T) (vm.VMBackend, vm.VMConfig)

type fixture struct {
	// backend 是当前场景独享的生命周期实现。
	backend vm.VMBackend
	// config 是该实现可使用的有效配置。
	config vm.VMConfig
	// ctx 为场景设置总时限。
	ctx context.Context
}

// newFixture 创建独立场景，并在测试结束后停止和销毁残留资源。
func newFixture(t *testing.T, factory Factory) fixture {
	t.Helper()
	backend, config := factory(t)
	if err := config.Validate(); err != nil {
		t.Fatalf("factory config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	// 清理使用新的有界 context，不受场景取消影响。
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		ref := vm.VMRef{ID: config.ID}
		status, err := backend.Inspect(cleanup, ref)
		if errors.Is(err, vm.ErrNotFound) {
			return
		}
		if err != nil {
			t.Errorf("cleanup Inspect: %v", err)
			return
		}
		if status.Instance.Epoch != "" {
			if err := backend.Stop(cleanup, status.Instance, stopOptions()); err != nil {
				t.Errorf("cleanup Stop: %v", err)
				return
			}
		}
		if err := backend.Destroy(cleanup, ref); err != nil {
			t.Errorf("cleanup Destroy: %v", err)
		}
	})
	return fixture{backend: backend, config: config, ctx: ctx}
}

// stopOptions 为契约测试提供允许强停的短宽限期。
func stopOptions() vm.StopOptions {
	return vm.StopOptions{GracePeriod: 100 * time.Millisecond, ForceAfterTimeout: true}
}

// create 创建场景资源，失败时终止当前测试。
func create(t *testing.T, f fixture) vm.VMRef {
	t.Helper()
	ref, err := f.backend.Create(f.ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// start 启动场景实例，并核对返回的资源身份。
func start(t *testing.T, f fixture, ref vm.VMRef) vm.VMInstance {
	t.Helper()
	instance, err := f.backend.Start(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Validate(); err != nil || instance.Ref != ref {
		t.Fatalf("invalid instance: %+v, %v", instance, err)
	}
	return instance
}

// requireState 查询并检查资源的预期状态。
func requireState(t *testing.T, f fixture, ref vm.VMRef, want vm.VMState) vm.VMStatus {
	t.Helper()
	status, err := f.backend.Inspect(f.ctx, ref)
	if err != nil || status.State != want || status.Ref != ref {
		t.Fatalf("Inspect: %+v, %v; want state %s", status, err, want)
	}
	return status
}

// Run 检查外部行为，不限定 fake 的时序、epoch 格式、信号名或优雅关机方式。
func Run(t *testing.T, factory Factory) {
	t.Helper()
	// 首次调用就并发进入，避免只验证“已经创建好”的幂等分支。
	t.Run("CreateIdempotentAndConflicting", func(t *testing.T) {
		f := newFixture(t, factory)
		ref := vm.VMRef{ID: f.config.ID}
		const count = 16
		begin := make(chan struct{})
		var wg sync.WaitGroup
		for range count {
			wg.Go(func() {
				<-begin
				got, err := f.backend.Create(f.ctx, f.config)
				if err != nil || got != ref {
					t.Errorf("duplicate Create: %+v, %v", got, err)
				}
			})
		}
		close(begin)
		wg.Wait()
		changed := f.config
		changed.MemoryMiB++
		if _, err := f.backend.Create(f.ctx, changed); !errors.Is(err, vm.ErrConflict) {
			t.Fatalf("different config: %v", err)
		}
		requireState(t, f, ref, vm.StateCreated)
	})
	// 所有启动调用竞争同一个尚未运行的资源，必须返回同一次运行。
	t.Run("ConcurrentFirstStart", func(t *testing.T) {
		f := newFixture(t, factory)
		ref := create(t, f)
		begin := make(chan struct{})
		results := make(chan vm.VMInstance, 16)
		var wg sync.WaitGroup
		for range cap(results) {
			wg.Go(func() {
				<-begin
				instance, err := f.backend.Start(f.ctx, ref)
				if err != nil {
					t.Error(err)
					return
				}
				results <- instance
			})
		}
		close(begin)
		wg.Wait()
		close(results)
		want := requireState(t, f, ref, vm.StateRunning).Instance
		count := 0
		for instance := range results {
			if instance != want {
				t.Errorf("concurrent Start: %+v; want %+v", instance, want)
			}
			count++
		}
		if count != 16 {
			t.Fatalf("only %d starts completed", count)
		}
	})
	// 启动请求已经完成后，取消该请求不能改变实例的运行状态。
	t.Run("StartIdempotentAndRequestCancellation", func(t *testing.T) {
		f := newFixture(t, factory)
		ref := create(t, f)
		startCtx, cancel := context.WithCancel(f.ctx)
		instance, err := f.backend.Start(startCtx, ref)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		if got := requireState(t, f, ref, vm.StateRunning).Instance; got != instance {
			t.Fatalf("unexpected instance: %+v", got)
		}
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				got, err := f.backend.Start(f.ctx, ref)
				if err != nil || got != instance {
					t.Errorf("duplicate Start: %+v, %v", got, err)
				}
			})
		}
		wg.Wait()
	})
	// 并发等待者必须获得同一份结果，单个等待者取消不影响其他调用。
	t.Run("ConcurrentWaitersAndCancelledWait", func(t *testing.T) {
		f := newFixture(t, factory)
		ref := create(t, f)
		instance := start(t, f, ref)
		results := make(chan vm.VMExit, 16)
		var wg sync.WaitGroup
		for range cap(results) {
			wg.Go(func() {
				result, err := f.backend.Wait(f.ctx, instance)
				if err != nil {
					t.Errorf("Wait: %v", err)
					return
				}
				results <- result
			})
		}
		// 其他等待者运行期间，让一个真正阻塞的等待到期，不能取消共享等待。
		waitCtx, cancel := context.WithTimeout(f.ctx, 50*time.Millisecond)
		defer cancel()
		cancelledResult := make(chan error, 1)
		go func() { _, err := f.backend.Wait(waitCtx, instance); cancelledResult <- err }()
		select {
		case err := <-cancelledResult:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked Wait: %v", err)
			}
		case <-f.ctx.Done():
			t.Fatal("blocked Wait ignored its deadline")
		}
		requireState(t, f, ref, vm.StateRunning)
		if err := f.backend.Stop(f.ctx, instance, stopOptions()); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		close(results)
		var want vm.VMExit
		n := 0
		for result := range results {
			if n == 0 {
				want = result
			}
			if result != want || result.Instance != instance {
				t.Errorf("inconsistent exit result: %+v, want %+v", result, want)
			}
			n++
		}
		if n != 16 {
			t.Fatalf("only %d waiters completed", n)
		}
		status := requireState(t, f, ref, vm.StateStopped)
		if status.Exit == nil || *status.Exit != want {
			t.Fatalf("stopped status has no matching exit: %+v", status)
		}
	})
	// 重启保留旧退出结果，但不允许旧实例控制新进程。
	t.Run("RestartRejectsStaleStop", func(t *testing.T) {
		f := newFixture(t, factory)
		ref := create(t, f)
		first := start(t, f, ref)
		if err := f.backend.Stop(f.ctx, first, stopOptions()); err != nil {
			t.Fatal(err)
		}
		if err := f.backend.Stop(f.ctx, first, stopOptions()); err != nil {
			t.Fatalf("repeat Stop: %v", err)
		}
		exit, err := f.backend.Wait(f.ctx, first)
		if err != nil {
			t.Fatal(err)
		}
		second := start(t, f, ref)
		if second.Epoch == first.Epoch {
			t.Fatal("restart reused epoch")
		}
		if err := f.backend.Stop(f.ctx, first, stopOptions()); !errors.Is(err, vm.ErrStaleInstance) {
			t.Fatalf("stale Stop: %v", err)
		}
		status := requireState(t, f, ref, vm.StateRunning)
		if status.Instance != second || status.Exit != nil {
			t.Fatalf("stale request affected current run: %+v", status)
		}
		if got, err := f.backend.Wait(f.ctx, first); err != nil || got != exit {
			t.Fatalf("prior exit was lost: %+v, %v", got, err)
		}
	})
	// 销毁只释放已停止的私有资源，重新创建不能复用旧 epoch。
	t.Run("DestroyRunningRejectedAndRecreate", func(t *testing.T) {
		f := newFixture(t, factory)
		ref := create(t, f)
		first := start(t, f, ref)
		if err := f.backend.Destroy(f.ctx, ref); !errors.Is(err, vm.ErrInvalidState) {
			t.Fatalf("Destroy running: %v", err)
		}
		requireState(t, f, ref, vm.StateRunning)
		if err := f.backend.Stop(f.ctx, first, stopOptions()); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := f.backend.Destroy(f.ctx, ref); err != nil {
				t.Fatalf("Destroy: %v", err)
			}
		}
		if _, err := f.backend.Inspect(f.ctx, ref); !errors.Is(err, vm.ErrNotFound) {
			t.Fatalf("Inspect destroyed: %v", err)
		}
		ref = create(t, f)
		second := start(t, f, ref)
		if first.Epoch == second.Epoch {
			t.Fatal("Destroy/Create reused epoch")
		}
	})
	// 从未启动的资源也允许直接销毁。
	t.Run("DestroyUnstarted", func(t *testing.T) {
		f := newFixture(t, factory)
		ref := create(t, f)
		if err := f.backend.Destroy(f.ctx, ref); err != nil {
			t.Fatal(err)
		}
	})
	// 非法停止参数不能产生退出副作用。
	t.Run("InvalidStopOptions", func(t *testing.T) {
		f := newFixture(t, factory)
		ref := create(t, f)
		instance := start(t, f, ref)
		for _, grace := range []time.Duration{0, -time.Second} {
			options := vm.StopOptions{GracePeriod: grace, ForceAfterTimeout: true}
			if err := f.backend.Stop(f.ctx, instance, options); !errors.Is(err, vm.ErrInvalidConfig) {
				t.Fatalf("invalid stop options: %v", err)
			}
			requireState(t, f, ref, vm.StateRunning)
		}
	})
	// 运行控制判断当前 epoch，历史等待按运行记录查找，错误语义不同。
	t.Run("UnknownEpochOnExistingResource", func(t *testing.T) {
		f := newFixture(t, factory)
		ref := create(t, f)
		instance := start(t, f, ref)
		unknown := vm.VMInstance{Ref: ref, Epoch: instance.Epoch + "-unknown"}
		if err := f.backend.Stop(f.ctx, unknown, stopOptions()); !errors.Is(err, vm.ErrStaleInstance) {
			t.Fatalf("Stop unknown epoch: %v", err)
		}
		if _, err := f.backend.Wait(f.ctx, unknown); !errors.Is(err, vm.ErrNotFound) {
			t.Fatalf("Wait unknown epoch: %v", err)
		}
		requireState(t, f, ref, vm.StateRunning)
	})
	// 已取消的请求不能产生资源变更。
	t.Run("PreCancelledMutations", func(t *testing.T) {
		f := newFixture(t, factory)
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		if _, err := f.backend.Create(ctx, f.config); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Create: %v", err)
		}
		ref := vm.VMRef{ID: f.config.ID}
		if _, err := f.backend.Inspect(f.ctx, ref); !errors.Is(err, vm.ErrNotFound) {
			t.Fatalf("cancelled Create left resource: %v", err)
		}
		create(t, f)
		if _, err := f.backend.Start(ctx, ref); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Start: %v", err)
		}
		requireState(t, f, ref, vm.StateCreated)
		if err := f.backend.Destroy(ctx, ref); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Destroy: %v", err)
		}
	})
	// 未知资源的查询和控制报错，重复销毁仍保持幂等。
	t.Run("UnknownResource", func(t *testing.T) {
		f := newFixture(t, factory)
		ref := vm.VMRef{ID: f.config.ID}
		instance := vm.VMInstance{Ref: ref, Epoch: "nonexistent"}
		if _, err := f.backend.Start(f.ctx, ref); !errors.Is(err, vm.ErrNotFound) {
			t.Errorf("Start unknown: %v", err)
		}
		if _, err := f.backend.Wait(f.ctx, instance); !errors.Is(err, vm.ErrNotFound) {
			t.Errorf("Wait unknown: %v", err)
		}
		if err := f.backend.Stop(f.ctx, instance, stopOptions()); !errors.Is(err, vm.ErrNotFound) {
			t.Errorf("Stop unknown: %v", err)
		}
		if err := f.backend.Destroy(f.ctx, ref); err != nil {
			t.Errorf("Destroy unknown: %v", err)
		}
	})
}
