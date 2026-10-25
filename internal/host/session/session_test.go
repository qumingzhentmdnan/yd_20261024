package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"safeagent/internal/host/vm"
	"safeagent/internal/host/workspace"
	"safeagent/internal/protocol"
	"safeagent/internal/testkit/fakevm"
	"safeagent/internal/workspacebundle"
)

// TestStartFailureCleanup 验证VM启动失败仍回收已经创建的资源，并返回原始失败。
func TestStartFailureCleanup(t *testing.T) {
	backend := fakevm.New()
	injected := errors.New("明确的启动故障")
	backend.FailNext(fakevm.OpStart, injected)
	root := t.TempDir()
	store, err := workspace.NewStore(filepath.Join(root, "workspace"), workspacebundle.GuestLimits())
	if err != nil {
		t.Fatal(err)
	}
	const digest = protocol.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	config := Config{Backend: backend, Workspace: store, VM: vm.VMConfig{ID: "start-failure", Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: vm.TCG, VCPUs: 1, MemoryMiB: 512, Image: vm.Image{KernelPath: filepath.Join(root, "kernel"), BaseDiskPath: filepath.Join(root, "base")}}, Selection: workspace.ImportSelection{Root: root, Paths: []string{}}, Content: protocol.Preparation{PreparedID: "registered", ManifestRef: "probe@1.0.0", ManifestDigest: digest, ConfigDigest: digest, WorkspaceRef: "workspace", WorkspaceDigest: digest}, Rows: 24, Cols: 80, StartupTimeout: time.Second}
	if err := Run(context.Background(), config, nil); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if _, err := backend.Inspect(context.Background(), vm.VMRef{ID: config.VM.ID}); !errors.Is(err, vm.ErrNotFound) {
		t.Fatal("启动失败留下VM资源", err)
	}
}

// TestInvalidInputBeforeResources 验证入口拒绝无预算或非法PTY尺寸，不创建VM。
func TestInvalidInputBeforeResources(t *testing.T) {
	backend := fakevm.New()
	for _, config := range []Config{{Backend: backend}, {Backend: backend, StartupTimeout: time.Second}} {
		if err := Run(context.Background(), config, nil); err == nil {
			t.Fatal("非法输入成功")
		}
	}
	if backend.Stats().Created != 0 {
		t.Fatal("非法输入创建了资源")
	}
}

// partialBackend 模拟资源或进程已创建之后返回错误的生命周期边界。
type partialBackend struct {
	// VMBackend 提供可检查的实际Fake资源状态。
	vm.VMBackend
	// phase 指定创建或启动完成后注入失败。
	phase string
	// failure 是应保留的原始失败。
	failure error
}

// Create 模拟创建副作用已发生但调用未成功返回。
func (b partialBackend) Create(ctx context.Context, c vm.VMConfig) (vm.VMRef, error) {
	ref, err := b.VMBackend.Create(ctx, c)
	if err == nil && b.phase == "create" {
		return vm.VMRef{}, b.failure
	}
	return ref, err
}

// Start 模拟进程已运行但状态发布失败时返回零实例。
func (b partialBackend) Start(ctx context.Context, ref vm.VMRef) (vm.VMInstance, error) {
	instance, err := b.VMBackend.Start(ctx, ref)
	if err == nil && b.phase == "start" {
		return vm.VMInstance{}, b.failure
	}
	return instance, err
}

// TestPartialLifecycleFailureCleanup 验证创建与启动的部分成功错误都不会留下自有资源。
func TestPartialLifecycleFailureCleanup(t *testing.T) {
	for _, phase := range []string{"create", "start"} {
		t.Run(phase, func(t *testing.T) {
			backend := fakevm.New()
			failure := errors.New("副作用完成后的发布失败")
			root := t.TempDir()
			store, err := workspace.NewStore(filepath.Join(root, "store"), workspacebundle.GuestLimits())
			if err != nil {
				t.Fatal(err)
			}
			const digest = protocol.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			config := Config{Backend: partialBackend{VMBackend: backend, phase: phase, failure: failure}, Workspace: store, VM: vm.VMConfig{ID: "partial-failure", Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: vm.TCG, VCPUs: 1, MemoryMiB: 512, Image: vm.Image{KernelPath: filepath.Join(root, "kernel"), BaseDiskPath: filepath.Join(root, "base")}}, Selection: workspace.ImportSelection{Root: root}, Content: protocol.Preparation{PreparedID: "registered", ManifestRef: "probe@1.0.0", ManifestDigest: digest, ConfigDigest: digest, WorkspaceRef: "workspace", WorkspaceDigest: digest}, Rows: 24, Cols: 80, StartupTimeout: time.Second}
			if err = Run(t.Context(), config, nil); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if _, err = backend.Inspect(t.Context(), vm.VMRef{ID: config.VM.ID}); !errors.Is(err, vm.ErrNotFound) {
				t.Fatal("部分失败留下自有资源", err)
			}
		})
	}
}
