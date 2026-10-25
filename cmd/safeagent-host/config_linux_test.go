package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
)

// writeTestJSON 写入不含真实凭据的显式启动测试素材。
func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadConfigBoundaries 验证镜像选择、文件逃逸与显式加速器在读取密钥前被检查。
func TestLoadConfigBoundaries(t *testing.T) {
	dir := t.TempDir()
	digest := protocol.Digest("sha256:" + strings.Repeat("a", 64))
	content := protocol.Preparation{PreparedID: "registered", ManifestRef: "codex@0.114.0", ManifestDigest: digest, ConfigDigest: digest, WorkspaceRef: "fixed-workspace", WorkspaceDigest: digest}
	contentPath := filepath.Join(dir, "content.json")
	writeTestJSON(t, contentPath, content)
	base := launchConfig{Agent: "codex", QEMU: "/usr/bin/qemu-system-x86_64", QEMUImg: "/usr/bin/qemu-img", DataRoot: filepath.Join(dir, "data"), Image: vm.Image{KernelPath: "/trusted/kernel", InitrdPath: "/trusted/initramfs", BaseDiskPath: "/trusted/base.raw"}, ContentFile: contentPath, Accelerator: vm.TCG, VCPUs: 2, MemoryMiB: 2048, Project: dir, Files: []string{"src/main.go"}}
	cases := []struct {
		// name 指定边界用例。
		name string
		// modify 调整用户可编辑输入。
		modify func(*launchConfig)
		// valid 标记是否应通过配置加载。
		valid bool
	}{{"valid", func(*launchConfig) {}, true}, {"different-agent", func(c *launchConfig) { c.Agent = "claude" }, false}, {"escape", func(c *launchConfig) { c.Files = []string{"../key"} }, false}, {"implicit-accelerator", func(c *launchConfig) { c.Accelerator = "" }, false}, {"relative-root", func(c *launchConfig) { c.Project = "project" }, false}, {"zero-memory", func(c *launchConfig) { c.MemoryMiB = 0 }, false}}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			config := base
			item.modify(&config)
			path := filepath.Join(dir, item.name+".json")
			writeTestJSON(t, path, config)
			_, _, err := loadConfig(path)
			if (err == nil) != item.valid {
				t.Fatal(err)
			}
		})
	}
}

// TestReadJSONRejectsUnexpectedInput 验证不接受密钥字段、尾随JSON与超限配置。
func TestReadJSONRejectsUnexpectedInput(t *testing.T) {
	for _, data := range []string{`{"apiKey":"test-only"}`, `{} {}`, strings.Repeat(" ", 1<<20) + "{}"} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		var config launchConfig
		if err := readJSON(path, &config); err == nil {
			t.Fatal("接受了不符合配置契约的输入")
		}
	}
}
