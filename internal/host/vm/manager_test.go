package vm_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/store"
	"safeagent/internal/testkit/vmcontract"
)

// testDriver 模拟宿主资源边界，验证 Manager 而非真实虚拟化行为。
type testDriver struct {
	// mu 保护故障开关和进程表。
	mu sync.Mutex
	// runs 保存测试运行句柄。
	runs map[vm.VMInstance]*testProcess
	// removeErr 模拟资源清理失败。
	removeErr error
	// launchErr 模拟无遗留进程的启动失败。
	launchErr error
}

// testProcess 为 Manager 提供固定退出通知。
type testProcess struct {
	// instance 标识当前测试运行。
	instance vm.VMInstance
	// once 保证结果只发布一次。
	once sync.Once
	// done 在退出结果固定后关闭。
	done chan struct{}
	// exit 保存固定结果。
	exit vm.VMExit
}

// Prepare 模拟幂等资源准备。
func (d *testDriver) Prepare(ctx context.Context, c vm.VMConfig) error { return ctx.Err() }

// Launch 创建测试运行，模拟启动错误时不创建进程。
func (d *testDriver) Launch(ctx context.Context, c vm.VMConfig, i vm.VMInstance) (vm.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.launchErr != nil {
		return nil, d.launchErr
	}
	p := &testProcess{instance: i, done: make(chan struct{})}
	d.runs[i] = p
	return p, nil
}

// Remove 返回注入的清理结果。
func (d *testDriver) Remove(ctx context.Context, ref vm.VMRef) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.removeErr
}

// Stop 模拟 microvm 的显式强停。
func (p *testProcess) Stop(o vm.StopOptions) error {
	if !o.ForceAfterTimeout {
		return vm.ErrShutdownUnsupported
	}
	p.finish(vm.VMExit{Instance: p.instance, Code: -1, Signal: "SIGKILL", Reason: vm.ExitForced})
	return nil
}

// Wait 等待固定的真实边界结果。
func (p *testProcess) Wait() (vm.VMExit, error) { <-p.done; return p.exit, nil }

// finish 模拟宿主进程退出。
func (p *testProcess) finish(exit vm.VMExit) { p.once.Do(func() { p.exit = exit; close(p.done) }) }

// managerConfig 返回只用于存储与协调测试的结构有效配置。
func managerConfig() vm.VMConfig {
	return vm.VMConfig{ID: "manager-test", Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: vm.TCG, VCPUs: 1, MemoryMiB: 128, Image: vm.Image{KernelPath: "/kernel", BaseDiskPath: "/base.raw"}}
}

// openManager 创建测试 Manager，数据库位于独立临时目录。
func openManager(t *testing.T, path string, d *testDriver) *vm.Manager {
	t.Helper()
	m, err := vm.NewManager(context.Background(), path, "test", d)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestManagerContract 复用共用 suite 验证 Manager 与非真实 Driver 的组合。
func TestManagerContract(t *testing.T) {
	vmcontract.Run(t, func(t *testing.T) (vm.VMBackend, vm.VMConfig) {
		d := &testDriver{runs: make(map[vm.VMInstance]*testProcess)}
		m := openManager(t, filepath.Join(t.TempDir(), "state.db"), d)
		t.Cleanup(func() {
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		})
		return m, managerConfig()
	})
}

// TestManagerReopen 验证配置、历史退出及 epoch 跨重开和销毁保留正确语义。
func TestManagerReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	d := &testDriver{runs: make(map[vm.VMInstance]*testProcess)}
	m := openManager(t, path, d)
	cfg := managerConfig()
	ref, err := m.Create(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	m = openManager(t, path, d)
	if s, e := m.Inspect(ctx, ref); e != nil || s.State != vm.StateCreated {
		t.Fatalf("restored Created: %+v %v", s, e)
	}
	i, err := m.Start(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("Close running: %v", err)
	}
	if err = m.Stop(ctx, i, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	exit, err := m.Wait(ctx, i)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	m = openManager(t, path, d)
	if got, e := m.Wait(ctx, i); e != nil || got != exit {
		t.Fatalf("restored exit: %+v %v", got, e)
	}
	if err = m.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Wait(ctx, i); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("destroyed history: %v", err)
	}
	if _, err = m.Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	second, err := m.Start(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch == i.Epoch {
		t.Fatal("epoch reused after reopen and Destroy")
	}
	if err = m.Stop(ctx, second, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestManagerRecoveryUnknown 验证崩溃留下的运行不会被编造为已退出。
func TestManagerRecoveryUnknown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := managerConfig()
	data, _ := json.Marshal(cfg)
	if err = s.Insert(ctx, store.Resource{ID: cfg.ID, Backend: "test", Config: data, State: "created"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := s.BeginRun(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	m := openManager(t, path, &testDriver{})
	defer m.Close()
	ref := vm.VMRef{ID: cfg.ID}
	i := vm.VMInstance{Ref: ref, Epoch: epoch}
	if status, e := m.Inspect(ctx, ref); e != nil || status.State != vm.StateUnknown || status.Exit != nil {
		t.Fatalf("unknown recovery: %+v %v", status, e)
	}
	if _, err = m.Wait(ctx, i); !errors.Is(err, vm.ErrExitUnavailable) {
		t.Fatalf("lost result: %v", err)
	}
	if _, err = m.Start(ctx, ref); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("restart unresolved run: %v", err)
	}
	if err = m.Destroy(ctx, ref); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("destroy unresolved run: %v", err)
	}
}

// TestManagerCleanupRetry 验证清理失败保留历史与可重试记录。
func TestManagerCleanupRetry(t *testing.T) {
	ctx := context.Background()
	d := &testDriver{runs: make(map[vm.VMInstance]*testProcess)}
	m := openManager(t, filepath.Join(t.TempDir(), "state.db"), d)
	defer m.Close()
	ref, err := m.Create(ctx, managerConfig())
	if err != nil {
		t.Fatal(err)
	}
	i, err := m.Start(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Stop(ctx, i, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	before, err := m.Wait(ctx, i)
	if err != nil {
		t.Fatal(err)
	}
	d.removeErr = os.ErrPermission
	if err = m.Destroy(ctx, ref); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	if got, e := m.Wait(ctx, i); e != nil || got != before {
		t.Fatalf("history lost: %+v %v", got, e)
	}
	d.removeErr = nil
	if err = m.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
}

// TestManagerSpontaneousExit 验证后台观察者持久化非零退出且等待者取消不影响运行。
func TestManagerSpontaneousExit(t *testing.T) {
	ctx := context.Background()
	d := &testDriver{runs: make(map[vm.VMInstance]*testProcess)}
	m := openManager(t, filepath.Join(t.TempDir(), "state.db"), d)
	defer m.Close()
	ref, err := m.Create(ctx, managerConfig())
	if err != nil {
		t.Fatal(err)
	}
	i, err := m.Start(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = m.Wait(cancelled, i); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	want := vm.VMExit{Instance: i, Code: 17, Reason: vm.ExitProcess}
	d.mu.Lock()
	p := d.runs[i]
	d.mu.Unlock()
	p.finish(want)
	if got, e := m.Wait(ctx, i); e != nil || got != want {
		t.Fatalf("exit: %+v %v", got, e)
	}
}

// Identity 返回测试 Driver 的固定资源身份。
func (d *testDriver) Identity() string { return "test-root" }

// TestManagerExitCommitRetry 使用 SQLite 触发器模拟实际提交错误并验证修复后可重试。
func TestManagerExitCommitRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	d := &testDriver{runs: make(map[vm.VMInstance]*testProcess)}
	m := openManager(t, path, d)
	ref, err := m.Create(ctx, managerConfig())
	if err != nil {
		t.Fatal(err)
	}
	i, err := m.Start(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER fail_exit BEFORE UPDATE OF exit ON runs BEGIN SELECT RAISE(FAIL,'injected commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	p := d.runs[i]
	d.mu.Unlock()
	want := vm.VMExit{Instance: i, Code: 23, Reason: vm.ExitProcess}
	p.finish(want)
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err = m.Wait(wait, i); err == nil {
		t.Fatal("commit failure hidden")
	}
	if err = m.Close(); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("lost pending exit on close: %v", err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_exit`); err != nil {
		t.Fatal(err)
	}
	if status, e := m.Inspect(ctx, ref); e != nil || status.State != vm.StateStopped || status.Exit == nil || *status.Exit != want {
		t.Fatalf("retry inspect: %+v %v", status, e)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	m = openManager(t, path, d)
	defer m.Close()
	if got, e := m.Wait(ctx, i); e != nil || got != want {
		t.Fatalf("persisted retry: %+v %v", got, e)
	}
}

// Acquire 为无真实资源的测试 Driver 提供无副作用句柄。
func (d *testDriver) Acquire() (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil }

// TestManagerRunningCommitRetry 验证进程启动成功后的 Running 提交错误可以重试。
func TestManagerRunningCommitRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	d := &testDriver{runs: make(map[vm.VMInstance]*testProcess)}
	m := openManager(t, path, d)
	ref, err := m.Create(ctx, managerConfig())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER fail_running BEFORE UPDATE OF state ON vms WHEN NEW.state='running' BEGIN SELECT RAISE(FAIL,'injected running failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(ctx, ref); err == nil {
		t.Fatal("running commit failure hidden")
	}
	if _, err = db.Exec(`DROP TRIGGER fail_running`); err != nil {
		t.Fatal(err)
	}
	i, err := m.Start(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	count := len(d.runs)
	d.mu.Unlock()
	if count != 1 {
		t.Fatalf("started %d processes", count)
	}
	if err = m.Stop(ctx, i, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestManagerLaunchFailureCommitRetry 验证无遗留进程的启动失败事实可在数据库恢复后提交。
func TestManagerLaunchFailureCommitRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	d := &testDriver{runs: make(map[vm.VMInstance]*testProcess), launchErr: os.ErrNotExist}
	m := openManager(t, path, d)
	ref, err := m.Create(ctx, managerConfig())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER fail_stopped BEFORE UPDATE OF state ON vms WHEN NEW.state='stopped' BEGIN SELECT RAISE(FAIL,'injected stopped failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(ctx, ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err = m.Close(); !errors.Is(err, vm.ErrInvalidState) {
		t.Fatalf("Close discarded failed launch: %v", err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_stopped`); err != nil {
		t.Fatal(err)
	}
	status, err := m.Inspect(ctx, ref)
	if err != nil || status.State != vm.StateStopped || status.Exit != nil {
		t.Fatalf("failed launch retry: %+v %v", status, err)
	}
	if err = m.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
}
