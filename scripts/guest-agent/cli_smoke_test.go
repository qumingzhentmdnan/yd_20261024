//go:build linux

package guestagent_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/host/workspace"
	"safeagent/internal/host/workspaceimport"
	"safeagent/internal/protocol"
	"safeagent/internal/workspacebundle"
)

// realGuest 使用当前生产Driver构造独立TCG实例，并分阶段注册独立清理。
func realGuest(t *testing.T) (*rpc.Client, *qemu.Channels, protocol.Preparation) {
	t.Helper()
	if os.Getenv("SAFEAGENT_GUEST_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实TCG验证")
	}
	image := os.Getenv("SAFEAGENT_REAL_CLI_IMAGE")
	if image == "" {
		t.Fatal("必须指定独立的SAFEAGENT_REAL_CLI_IMAGE")
	}
	image, err := filepath.Abs(image)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(image, "content.json"))
	if err != nil {
		t.Fatal(err)
	}
	var content protocol.Preparation
	if err = json.Unmarshal(data, &content); err != nil {
		t.Fatal(err)
	}
	assets := "/home/yedong/workspace/sandbox/artifacts/validation"
	tools := "/home/yedong/workspace/sandbox/artifacts/toolchain/bin"
	root, err := os.MkdirTemp("/tmp", "sawi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	driver, err := qemu.NewDriver(filepath.Join(tools, "qemu-system-x86_64"), filepath.Join(tools, "qemu-img"), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	manager, err := vm.NewManager(ctx, filepath.Join(root, "state.db"), "qemu", driver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	base := filepath.Join(assets, "workspace-base.raw")
	before, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	config := vm.VMConfig{ID: "workspace", Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: vm.TCG, VCPUs: 2, MemoryMiB: 2048, Image: vm.Image{KernelPath: filepath.Join(assets, "guest-kernel/boot/vmlinuz-6.8.0-147-generic"), InitrdPath: filepath.Join(image, "initramfs.cpio.gz"), BaseDiskPath: base}}
	ref, err := manager.Create(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := manager.Destroy(clean, ref); err != nil {
			t.Error(err)
		}
		after, e := os.ReadFile(base)
		if e != nil || sha256.Sum256(before) != sha256.Sum256(after) {
			t.Error("基础盘改变", e)
		} else {
			t.Logf("基础盘SHA256=%x，自有VM已回收", sha256.Sum256(after))
		}
	})
	instance, err := manager.Start(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := manager.Stop(clean, instance, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
			t.Error(err)
		}
		if _, err := manager.Wait(clean, instance); err != nil {
			t.Error(err)
		}
	})
	channels, err := driver.OpenGuestChannels(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	client, err := rpc.New(instance, channels.Control, channels.OpenTerminal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if _, err = client.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	return client, channels, content
}

// TestRealCLIProductionStart 验证生产镜像的真实导入、登记 Prepare、非 root CLI 启动和受控停止，不声称供应商任务完成。
func TestRealCLIProductionStart(t *testing.T) {
	client, channels, content := realGuest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("isolated CLI smoke\n"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := workspace.NewStore(filepath.Join(t.TempDir(), "store"), workspacebundle.GuestLimits())
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.Stage(ctx, workspace.ImportSelection{Root: source, Paths: []string{"README.md"}})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := workspaceimport.Import(ctx, client, channels, store, base.ID)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := protocol.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := client.PrepareAgent(ctx, protocol.PrepareAgentParams{OperationID: operation, ManifestRef: content.ManifestRef, ManifestDigest: content.ManifestDigest, ConfigDigest: content.ConfigDigest, WorkspaceRef: imported.WorkspaceRef, WorkspaceDigest: imported.WorkspaceDigest})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.PlanDigest == nil {
		t.Fatal("真实配置缺少计划摘要")
	}
	operation, err = protocol.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.StartSession(ctx, protocol.StartSessionParams{OperationID: operation, SessionID: "real-cli", PreparedID: prepared.PreparedID, WorkspaceRef: imported.WorkspaceRef, WorkspaceDigest: imported.WorkspaceDigest, Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := client.AttachTerminal(ctx, session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	done := make(chan error, 1)
	var output bytes.Buffer
	go func() {
		data := make([]byte, 16384)
		positionAnswered := false
		for {
			n, readErr := terminal.Read(data)
			output.Write(data[:n])
			// CLI 查询终端位置时交付固定响应，保持真实生产 PTY 启动参数。
			if !positionAnswered && bytes.Contains(output.Bytes(), []byte("\x1b[6n")) {
				positionAnswered = true
				if _, writeErr := terminal.Write([]byte("\x1b[1;1R")); writeErr != nil {
					done <- writeErr
					return
				}
			}
			if output.Len() > 1<<20 {
				done <- fmt.Errorf("CLI 启动输出超出 1MiB")
				return
			}
			text := terminalText(output.String())
			if strings.Contains(text, "Welcome to Codex") || strings.Contains(text, "OpenAI Codex") || strings.Contains(text, "Welcome to Claude Code") || strings.Contains(text, "Choose the text style") {
				done <- nil
				return
			}
			if readErr != nil {
				done <- readErr
				return
			}
		}
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		terminal.Close()
		<-done
		err = ctx.Err()
	}
	visible := terminalText(output.String())
	if len(visible) > 4096 {
		visible = visible[:4096]
	}
	t.Logf("真实 CLI 生产启动输出: %s", visible)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.StopSession(ctx, protocol.StopSessionParams{SessionID: session.SessionID, GraceMs: 1000, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.WaitSession(ctx, session.SessionID); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: Imported → Prepare(%s) → Start → 真实 CLI 界面 → Stop/Wait；未调用供应商", content.ManifestRef, *prepared.PlanDigest)
}

// terminalText 将真实 CLI 的 ANSI 样式与向右移动还原成可匹配的可见词，不改变 PTY 数据。
func terminalText(value string) string {
	escape := regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	value = escape.ReplaceAllStringFunc(value, func(code string) string {
		if strings.HasSuffix(code, "C") || strings.HasSuffix(code, "H") {
			return " "
		}
		return ""
	})
	return strings.Join(strings.Fields(value), " ")
}
