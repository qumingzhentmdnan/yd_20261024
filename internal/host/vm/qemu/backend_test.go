package qemu

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"safeagent/internal/host/vm"
	"safeagent/internal/testkit/vmcontract"
)

// testConfig 返回本机验证使用的最小 microvm 配置。
func testConfig(t *testing.T, id string) vm.VMConfig {
	t.Helper()
	root := "/home/yedong/workspace/sandbox/artifacts/validation"
	return vm.VMConfig{ID: id, Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: vm.TCG, VCPUs: 1, MemoryMiB: 256, Image: vm.Image{KernelPath: filepath.Join(root, "guest-kernel/boot/vmlinuz-6.8.0-147-generic"), InitrdPath: filepath.Join(root, "initramfs.cpio.gz"), BaseDiskPath: filepath.Join(root, "workspace-base.raw")}}
}

// privateTempDir 创建满足后端权限契约的临时根目录。
func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestNewDriverRejectsRelativePaths 验证构造函数拒绝未解析的宿主路径。
func TestNewDriverRejectsRelativePaths(t *testing.T) {
	if _, err := NewDriver("qemu", "/qemu-img", t.TempDir()); !errors.Is(err, vm.ErrInvalidConfig) {
		t.Fatalf("NewDriver: %v", err)
	}
}

// TestArgsUsesFixedDevicePolicy 验证启动参数保持无网卡和私有 overlay 策略。
func TestArgsUsesFixedDevicePolicy(t *testing.T) {
	argv := args(testConfig(t, "args-test"), "/tmp/safeagent-vm", "epoch")
	joined := " " + strings.Join(argv, " ") + " "
	for _, want := range []string{" -nic none ", " -nodefaults ", " -no-user-config ", "overlay.qcow2", " -machine microvm,acpi=off "} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %v", want, argv)
		}
	}
}

// TestContractIntegration 在显式环境变量下运行真实 QEMU 契约测试。
func TestContractIntegration(t *testing.T) {
	if os.Getenv("SAFEAGENT_QEMU_INTEGRATION") != "1" {
		t.Skip("设置 SAFEAGENT_QEMU_INTEGRATION=1 才运行真实 TCG 测试")
	}
	qemuPath := os.Getenv("SAFEAGENT_QEMU_PATH")
	qemuImgPath := os.Getenv("SAFEAGENT_QEMU_IMG_PATH")
	if qemuPath == "" {
		qemuPath = "/home/yedong/workspace/sandbox/artifacts/toolchain/bin/qemu-system-x86_64"
	}
	if qemuImgPath == "" {
		qemuImgPath = "/home/yedong/workspace/sandbox/artifacts/toolchain/bin/qemu-img"
	}
	vmcontract.Run(t, func(t *testing.T) (vm.VMBackend, vm.VMConfig) {
		root := privateTempDir(t)
		backend, err := newTestManager(qemuPath, qemuImgPath, root, filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := backend.Close(); err != nil {
				t.Error(err)
			}
		})
		return backend, testConfig(t, "contract-test")
	})
}

// TestPersistentReopenIntegration 验证真实磁盘和退出记录跨 Manager 重开后可继续使用。
func TestPersistentReopenIntegration(t *testing.T) {
	if os.Getenv("SAFEAGENT_QEMU_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实 TCG")
	}
	root := privateTempDir(t)
	qemuPath := os.Getenv("SAFEAGENT_QEMU_PATH")
	if qemuPath == "" {
		qemuPath = "/home/yedong/workspace/sandbox/artifacts/toolchain/bin/qemu-system-x86_64"
	}
	imgPath := os.Getenv("SAFEAGENT_QEMU_IMG_PATH")
	if imgPath == "" {
		imgPath = "/home/yedong/workspace/sandbox/artifacts/toolchain/bin/qemu-img"
	}
	cfg := testConfig(t, "persistent")
	baseBefore, err := os.ReadFile(cfg.Image.BaseDiskPath)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "state.db")
	m, err := newTestManager(qemuPath, imgPath, root, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := m.Create(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(root, ref.ID, "overlay.qcow2")
	before, err := os.Stat(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = newTestManager(qemuPath, imgPath, root, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("reopen replaced overlay")
	}
	i, err := m.Start(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	if _, err = m.Wait(waitCtx, i); !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		t.Fatalf("real QEMU exited early: %v", err)
	}
	cancel()
	if err = m.Stop(ctx, i, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	exit, err := m.Wait(ctx, i)
	if err != nil {
		t.Fatal(err)
	}
	if exit.Reason != vm.ExitForced || exit.Signal == "" {
		t.Fatalf("forced exit: %+v", exit)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = newTestManager(qemuPath, imgPath, root, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if restored, e := m.Wait(ctx, i); e != nil || restored != exit {
		t.Fatalf("restored actual exit: %+v %v", restored, e)
	}
	if err = m.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	baseAfter, err := os.ReadFile(cfg.Image.BaseDiskPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(baseBefore) != sha256.Sum256(baseAfter) {
		t.Fatal("base image changed")
	}
}

// TestLaunchFailureIntegration 验证真实 qemu-img 资源创建后进程启动错误可查询且可重试。
func TestLaunchFailureIntegration(t *testing.T) {
	if os.Getenv("SAFEAGENT_QEMU_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实 TCG")
	}
	m, err := newTestManager(filepath.Join(t.TempDir(), "missing-qemu"), "/home/yedong/workspace/sandbox/artifacts/toolchain/bin/qemu-img", privateTempDir(t), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()
	ref, err := m.Create(ctx, testConfig(t, "launch-failure"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(ctx, ref); err == nil {
		t.Fatal("missing executable accepted")
	}
	status, err := m.Inspect(ctx, ref)
	if err != nil || status.State != vm.StateStopped || status.Exit != nil {
		t.Fatalf("failed launch status: %+v %v", status, err)
	}
	if _, err = m.Wait(ctx, status.Instance); !errors.Is(err, vm.ErrExitUnavailable) {
		t.Fatalf("fabricated launch exit: %v", err)
	}
	if err = m.Destroy(ctx, ref); err != nil {
		t.Fatal(err)
	}
}

// TestResourceRootExclusive 验证不同数据库不能同时控制同一真实资源根目录。
func TestResourceRootExclusive(t *testing.T) {
	root := privateTempDir(t)
	d, err := NewDriver("/qemu", "/qemu-img", root)
	if err != nil {
		t.Fatal(err)
	}
	m, err := vm.NewManager(context.Background(), filepath.Join(root, "one.db"), "qemu", d)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if second, e := vm.NewManager(context.Background(), filepath.Join(root, "two.db"), "qemu", d); e == nil {
		second.Close()
		t.Fatal("same root accepted by two databases")
	}
}

// TestPrepareInterruptedConfigIntegration 验证创建目录后中断可继续发布配置和差分盘。
func TestPrepareInterruptedConfigIntegration(t *testing.T) {
	if os.Getenv("SAFEAGENT_QEMU_INTEGRATION") != "1" {
		t.Skip("需要真实 qemu-img")
	}
	root := privateTempDir(t)
	cfg := testConfig(t, "interrupted-create")
	d, err := NewDriver("/unused", "/home/yedong/workspace/sandbox/artifacts/toolchain/bin/qemu-img", root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, cfg.ID)
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "config.json.creating"), []byte(`{"partial":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = d.Prepare(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(dir, "overlay.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Prepare(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(dir, "overlay.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("idempotent prepare replaced overlay")
	}
}

// newTestManager 在测试调用侧显式组装 Driver 与 Manager，数据库路径独立于资源目录。
func newTestManager(qemuPath, qemuImgPath, resourceRoot, databasePath string) (*vm.Manager, error) {
	driver, err := NewDriver(qemuPath, qemuImgPath, resourceRoot)
	if err != nil {
		return nil, err
	}
	return vm.NewManager(context.Background(), databasePath, "qemu", driver)
}

// TestChannelPathBound 验证最长资源目录和 epoch 不会让 Unix socket 路径超过内核上限。
func TestChannelPathBound(t *testing.T) {
	dir := strings.Repeat("long-root/", 20)
	path := filepath.Join(channelDir(dir, strings.Repeat("e", 128)), "terminal.sock")
	if len(path) >= 108 {
		t.Fatalf("Unix socket 路径过长: %d", len(path))
	}
	if channelDir(dir, "epoch") == channelDir(dir+"other", "epoch") {
		t.Fatal("不同资源根不能共享端点目录")
	}
}

// TestClockPolicyMatchesAccelerator 验证TCG关闭精简ACPI，KVM参数与vCPU数量不变。
func TestClockPolicyMatchesAccelerator(t *testing.T) {
	config := testConfig(t, "clock-policy")
	config.VCPUs = 2
	for _, accel := range []vm.Accelerator{vm.TCG, vm.KVM} {
		config.Accelerator = accel
		argv := args(config, "/tmp/clock-policy", "epoch")
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "microvm,acpi=off") != (accel == vm.TCG) {
			t.Fatal("时钟策略错误", argv)
		}
		if !strings.Contains(joined, "-smp 2") || strings.Contains(joined, "-icount") {
			t.Fatal("不得改变vCPU数量或虚拟时钟速率", argv)
		}
	}
}
