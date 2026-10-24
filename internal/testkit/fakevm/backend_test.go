package fakevm_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"safeagent/internal/host/vm"
	"safeagent/internal/testkit/fakevm"
	"safeagent/internal/testkit/vmcontract"
)

// config 构造结构有效但不需要真实镜像文件的 fake 配置。
func config(t *testing.T) vm.VMConfig {
	t.Helper()
	dir := t.TempDir()
	return vm.VMConfig{
		ID: "test-vm", Architecture: vm.AMD64, Machine: vm.MicroVM,
		Accelerator: vm.TCG, VCPUs: 2, MemoryMiB: 512,
		Image: vm.Image{KernelPath: filepath.Join(dir, "kernel"), BaseDiskPath: filepath.Join(dir, "base")},
	}
}

// TestContract 让 fake 运行真实后端也需遵守的共用测试。
func TestContract(t *testing.T) {
	vmcontract.Run(t, func(t *testing.T) (vm.VMBackend, vm.VMConfig) {
		return fakevm.New(), config(t)
	})
}

// setup 创建带总时限的独立 fake 场景。
func setup(t *testing.T) (*fakevm.Backend, context.Context, vm.VMRef) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	b := fakevm.New()
	ref, err := b.Create(ctx, config(t))
	if err != nil {
		t.Fatal(err)
	}
	return b, ctx, ref
}

// must 在测试操作失败时终止当前场景。
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// running 启动实例并返回其身份。
func running(t *testing.T, b *fakevm.Backend, ctx context.Context, ref vm.VMRef) vm.VMInstance {
	t.Helper()
	instance, err := b.Start(ctx, ref)
	must(t, err)
	return instance
}

// TestConcurrentFirstCreateAndStartHaveSingleSideEffect 验证并发首次调用只产生一份资源和实例。
func TestConcurrentFirstCreateAndStartHaveSingleSideEffect(t *testing.T) {
	b := fakevm.New()
	cfg := config(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			ref, err := b.Create(ctx, cfg)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := b.Start(ctx, ref); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := b.Stats(); got.Created != 1 || got.Started != 1 {
		t.Fatalf("duplicate side effects: %+v", got)
	}
}

// TestStartGateCancellationAndRetry 验证阻塞启动可取消，且不影响其他资源和后续重试。
func TestStartGateCancellationAndRetry(t *testing.T) {
	b, ctx, ref := setup(t)
	gate := fakevm.NewGate()
	t.Cleanup(gate.Release)
	b.SetGate(fakevm.OpStart, gate)
	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := b.Start(startCtx, ref); result <- err }()
	select {
	case <-gate.Entered():
	case <-ctx.Done():
		t.Fatal("Start did not reach gate")
	}
	status, err := b.Inspect(ctx, ref)
	must(t, err)
	if status.State != vm.StateStarting {
		t.Fatalf("state: %s", status.State)
	}
	// 同一资源阻塞时，其他资源仍可继续操作。
	other := config(t)
	other.ID = "another-vm"
	_, err = b.Create(ctx, other)
	must(t, err)
	// 首个启动仍被闸门阻塞，让另一个调用在等待资源锁时超时。
	blocked, done := context.WithTimeout(ctx, 50*time.Millisecond)
	defer done()
	lockResult := make(chan error, 1)
	go func() { _, err := b.Start(blocked, ref); lockResult <- err }()
	select {
	case err := <-lockResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked Start: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("resource lock ignored cancellation")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Start stayed blocked after cancellation")
	}
	status, err = b.Inspect(ctx, ref)
	must(t, err)
	if status.State != vm.StateCreated || b.Stats().Started != 0 {
		t.Fatalf("cancelled startup mutated resource: %+v", status)
	}
	gate.Release()
	running(t, b, ctx, ref)
}

// TestFailuresLeaveRetryableResources 验证操作失败保留资源，重试后计数正确。
func TestFailuresLeaveRetryableResources(t *testing.T) {
	for _, op := range []fakevm.Operation{fakevm.OpStart, fakevm.OpStop, fakevm.OpDestroy} {
		t.Run(string(op), func(t *testing.T) {
			b, ctx, ref := setup(t)
			injected := errors.New("injected failure")
			b.FailNext(op, injected)
			if op == fakevm.OpStart {
				if _, err := b.Start(ctx, ref); !errors.Is(err, injected) {
					t.Fatal(err)
				}
			}
			instance := running(t, b, ctx, ref)
			opts := vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}
			if op == fakevm.OpStop {
				if err := b.Stop(ctx, instance, opts); !errors.Is(err, injected) {
					t.Fatal(err)
				}
			}
			must(t, b.Stop(ctx, instance, opts))
			exit, err := b.Wait(ctx, instance)
			must(t, err)
			if op == fakevm.OpDestroy {
				if err := b.Destroy(ctx, ref); !errors.Is(err, injected) {
					t.Fatal(err)
				}
				if _, err := b.Inspect(ctx, ref); err != nil {
					t.Fatalf("failed cleanup lost resource: %v", err)
				}
				if got, err := b.Wait(ctx, instance); err != nil || got != exit {
					t.Fatalf("failed cleanup lost exit result: %+v, %v", got, err)
				}
			}
			must(t, b.Destroy(ctx, ref))
			if got := b.Stats(); got.Created != 1 || got.Started != 1 || got.Stopped != 1 || got.Destroyed != 1 {
				t.Fatalf("unexpected side effects after retry: %+v", got)
			}
		})
	}
}

// TestCreateFailureHasNoResource 验证创建失败不留下可见资源。
func TestCreateFailureHasNoResource(t *testing.T) {
	b := fakevm.New()
	cfg := config(t)
	err := errors.New("create failed")
	b.FailNext(fakevm.OpCreate, err)
	if _, got := b.Create(context.Background(), cfg); !errors.Is(got, err) {
		t.Fatal(got)
	}
	if _, got := b.Inspect(context.Background(), vm.VMRef{ID: cfg.ID}); !errors.Is(got, vm.ErrNotFound) {
		t.Fatal(got)
	}
	if _, got := b.Create(context.Background(), cfg); got != nil {
		t.Fatal(got)
	}
}

// TestExternalExitCompletesWaitAndPendingStop 验证外部退出与正在停止的操作共享同一结果。
func TestExternalExitCompletesWaitAndPendingStop(t *testing.T) {
	b, ctx, ref := setup(t)
	instance := running(t, b, ctx, ref)
	gate := fakevm.NewGate()
	t.Cleanup(gate.Release)
	b.SetGate(fakevm.OpStop, gate)
	stopped := make(chan error, 1)
	go func() { stopped <- b.Stop(ctx, instance, vm.StopOptions{GracePeriod: time.Second}) }()
	select {
	case <-gate.Entered():
	case <-ctx.Done():
		t.Fatal("Stop did not reach gate")
	}
	must(t, b.Exit(instance, 7, ""))
	exit, err := b.Wait(ctx, instance)
	must(t, err)
	if exit.Code != 7 || exit.Reason != vm.ExitProcess {
		t.Fatalf("external exit: %+v", exit)
	}
	gate.Release()
	select {
	case err := <-stopped:
		must(t, err)
	case <-ctx.Done():
		t.Fatal("Stop did not complete")
	}
	status, err := b.Inspect(ctx, ref)
	must(t, err)
	// 修改查询快照不能改变缓存的退出结果。
	status.Exit.Code = 99
	again, err := b.Wait(ctx, instance)
	must(t, err)
	if again != exit {
		t.Fatalf("exit was overwritten: %+v", again)
	}
}

// TestUnsupportedGracefulShutdownRequiresExplicitForce 验证不支持优雅关机时不会擅自强停。
func TestUnsupportedGracefulShutdownRequiresExplicitForce(t *testing.T) {
	b, ctx, ref := setup(t)
	b.SetGracefulShutdownSupported(false)
	instance := running(t, b, ctx, ref)
	opts := vm.StopOptions{GracePeriod: time.Second}
	if err := b.Stop(ctx, instance, opts); !errors.Is(err, vm.ErrShutdownUnsupported) {
		t.Fatal(err)
	}
	status, err := b.Inspect(ctx, ref)
	must(t, err)
	if status.State != vm.StateRunning {
		t.Fatalf("unsupported shutdown stopped VM: %+v", status)
	}
	opts.ForceAfterTimeout = true
	must(t, b.Stop(ctx, instance, opts))
	exit, err := b.Wait(ctx, instance)
	must(t, err)
	if exit.Reason != vm.ExitForced || exit.Signal != "SIGKILL" || exit.Code != -1 {
		t.Fatalf("forced exit: %+v", exit)
	}
}

// TestCancelledStopCanRetry 验证停止中的取消不终结运行，后续仍可正常停止。
func TestCancelledStopCanRetry(t *testing.T) {
	b, ctx, ref := setup(t)
	instance := running(t, b, ctx, ref)
	gate := fakevm.NewGate()
	t.Cleanup(gate.Release)
	b.SetGate(fakevm.OpStop, gate)
	stopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	options := vm.StopOptions{GracePeriod: time.Second}
	result := make(chan error, 1)
	go func() { result <- b.Stop(stopCtx, instance, options) }()
	select {
	case <-gate.Entered():
	case <-ctx.Done():
		t.Fatal("Stop did not reach gate")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Stop: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Stop ignored cancellation")
	}
	status, err := b.Inspect(ctx, ref)
	must(t, err)
	if status.State != vm.StateRunning || b.Stats().Stopped != 0 {
		t.Fatalf("cancelled Stop changed run: %+v", status)
	}
	gate.Release()
	must(t, b.Stop(ctx, instance, options))
}

// TestOldExitCannotChangeRestartedVM 验证旧实例的退出通知不能改写新运行。
func TestOldExitCannotChangeRestartedVM(t *testing.T) {
	b, ctx, ref := setup(t)
	first := running(t, b, ctx, ref)
	must(t, b.Stop(ctx, first, vm.StopOptions{GracePeriod: time.Second}))
	second := running(t, b, ctx, ref)
	if err := b.Exit(first, 7, ""); !errors.Is(err, vm.ErrStaleInstance) {
		t.Fatalf("old exit: %v", err)
	}
	status, err := b.Inspect(ctx, ref)
	must(t, err)
	if status.State != vm.StateRunning || status.Instance != second || status.Exit != nil {
		t.Fatalf("old exit changed current run: %+v", status)
	}
}
