package vm_test

import (
	"errors"
	"path/filepath"
	"testing"

	"safeagent/internal/host/vm"
)

// TestConfigRejectsUnsafeOrImplicitConfiguration 验证配置边界拒绝路径式标识和隐式运行参数。
func TestConfigRejectsUnsafeOrImplicitConfiguration(t *testing.T) {
	dir := t.TempDir()
	valid := vm.VMConfig{
		ID: "sandbox-1", Architecture: vm.AMD64, Machine: vm.MicroVM,
		Accelerator: vm.TCG, VCPUs: 2, MemoryMiB: 512,
		Image: vm.Image{KernelPath: filepath.Join(dir, "kernel"), BaseDiskPath: filepath.Join(dir, "base")},
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*vm.VMConfig){
		"empty ID":              func(c *vm.VMConfig) { c.ID = "" },
		"path traversal ID":     func(c *vm.VMConfig) { c.ID = "../outside" },
		"implicit accelerator":  func(c *vm.VMConfig) { c.Accelerator = "" },
		"unknown accelerator":   func(c *vm.VMConfig) { c.Accelerator = "auto" },
		"architecture mismatch": func(c *vm.VMConfig) { c.Architecture = vm.ARM64 },
		"zero CPUs":             func(c *vm.VMConfig) { c.VCPUs = 0 },
		"zero memory":           func(c *vm.VMConfig) { c.MemoryMiB = 0 },
		"relative kernel":       func(c *vm.VMConfig) { c.Image.KernelPath = "kernel" },
		"relative disk":         func(c *vm.VMConfig) { c.Image.BaseDiskPath = "base" },
		"relative initrd":       func(c *vm.VMConfig) { c.Image.InitrdPath = "initrd" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if err := cfg.Validate(); !errors.Is(err, vm.ErrInvalidConfig) {
				t.Fatalf("expected invalid config, got %v", err)
			}
		})
	}
}
