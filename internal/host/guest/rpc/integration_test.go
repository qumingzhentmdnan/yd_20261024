//go:build linux

package rpc_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"errors"
	"safeagent/internal/guestwire"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/protocol"
	"safeagent/internal/testkit/guestcontract"
)

// TestQEMUIntegration 在显式环境下用生产 QEMU 装配和生产 Client/Guest 完成真实设备闭环。
func TestQEMUIntegration(t *testing.T) {
	c, p, s := qemuClient(t)
	runQEMUFlow(t, c, p, s)
}

// TestQEMUContractIntegration 将真实生产通道接入全部共享行为场景，每场景独立 VM。
func TestQEMUContractIntegration(t *testing.T) {
	if os.Getenv("SAFEAGENT_GUEST_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实 Guest TCG 验证")
	}
	guestcontract.Run(t, func(t *testing.T) (guest.Client, protocol.PrepareAgentParams, protocol.StartSessionParams) {
		return qemuClient(t)
	})
}

// qemuClient 在每个场景创建独立真实 VM，清理不依赖仍然可用的 Guest Client。
func qemuClient(t *testing.T) (*rpc.Client, protocol.PrepareAgentParams, protocol.StartSessionParams) {
	c, p, s, _, _ := qemuTransport(t)
	return c, p, s
}

// qemuTransport 额外交付可信私有句柄供真实设备故障探针使用。
func qemuTransport(t *testing.T) (*rpc.Client, protocol.PrepareAgentParams, protocol.StartSessionParams, *qemu.Channels, *terminalTap) {
	if os.Getenv("SAFEAGENT_GUEST_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实 Guest TCG 验证")
	}
	assets := "/home/yedong/workspace/sandbox/artifacts/validation"
	toolchain := "/home/yedong/workspace/sandbox/artifacts/toolchain/bin"
	image := os.Getenv("SAFEAGENT_GUEST_IMAGE")
	if image == "" {
		t.Fatal("需要指定独立构建的 SAFEAGENT_GUEST_IMAGE 目录")
	}
	image, err := filepath.Abs(image)
	if err != nil {
		t.Fatal(err)
	}
	contentBytes, err := os.ReadFile(filepath.Join(image, "content.json"))
	if err != nil {
		t.Fatal(err)
	}
	var content protocol.Preparation
	if err = json.Unmarshal(contentBytes, &content); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "sag-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	driver, err := qemu.NewDriver(filepath.Join(toolchain, "qemu-system-x86_64"), filepath.Join(toolchain, "qemu-img"), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	manager, err := vm.NewManager(ctx, filepath.Join(root, "state.db"), "qemu", driver)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(assets, "workspace-base.raw")
	before, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	config := vm.VMConfig{ID: "guest", Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: vm.TCG, VCPUs: 2, MemoryMiB: 512, Image: vm.Image{KernelPath: filepath.Join(assets, "guest-kernel/boot/vmlinuz-6.8.0-147-generic"), InitrdPath: filepath.Join(image, "initramfs.cpio.gz"), BaseDiskPath: base}}
	ref, err := manager.Create(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := manager.Start(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := manager.Stop(cleanup, instance, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
			t.Error(err)
		}
		if _, err := manager.Wait(cleanup, instance); err != nil {
			t.Error(err)
		}
		if err := manager.Destroy(cleanup, ref); err != nil {
			t.Error(err)
		}
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
		after, err := os.ReadFile(base)
		if err != nil {
			t.Error(err)
		} else if sha256.Sum256(before) != sha256.Sum256(after) {
			t.Error("基础盘摘要发生变化")
		} else {
			t.Logf("基础盘SHA256=%x；自有QEMU已停止等待并销毁", sha256.Sum256(after))
		}
	})
	channels, err := driver.OpenGuestChannels(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	tap := &terminalTap{}
	client, err := rpc.New(instance, channels.Control, func(ctx context.Context) (net.Conn, error) {
		port, err := channels.OpenTerminal(ctx)
		if err == nil {
			tap.mu.Lock()
			tap.port = port
			tap.mu.Unlock()
		}
		return port, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	op, err := protocol.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	startOp, err := protocol.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	// 资源清理依靠独立context；当前构造context只用于VM启动和端点交付。
	return client, protocol.PrepareAgentParams{OperationID: op, ManifestRef: content.ManifestRef, ManifestDigest: content.ManifestDigest, ConfigDigest: content.ConfigDigest, WorkspaceRef: content.WorkspaceRef, WorkspaceDigest: content.WorkspaceDigest}, protocol.StartSessionParams{OperationID: startOp, SessionID: "session", WorkspaceRef: content.WorkspaceRef, WorkspaceDigest: content.WorkspaceDigest, Rows: 24, Cols: 80}, channels, tap
}

// runQEMUFlow 验证真实窗口、原始终端字节与退出尾部，补充共享 suite 之外的设备断言。
func runQEMUFlow(t *testing.T, client *rpc.Client, prepare protocol.PrepareAgentParams, start protocol.StartSessionParams) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hello, err := client.Hello(ctx)
	if err != nil {
		t.Fatal("Hello:", err)
	}
	t.Log("Hello", hello.Version)
	state, err := client.GetState(ctx)
	if err != nil || len(state.Sessions) != 0 {
		t.Fatal("GetState", state, err)
	}
	prepared, err := client.PrepareAgent(ctx, prepare)
	if err != nil {
		t.Fatal("Prepare", err)
	}
	start.PreparedID = prepared.PreparedID
	info, err := client.StartSession(ctx, start)
	if err != nil {
		t.Fatal("Start", err)
	}
	terminal, err := client.AttachTerminal(ctx, info.SessionID)
	if err != nil {
		t.Fatal("Attach", err)
	}
	defer terminal.Close()
	reader := bufio.NewReader(terminal)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "READY uid=1000") {
		t.Fatal("nonroot READY", line, err)
	}
	if _, err = terminal.Write([]byte("size\n")); err != nil {
		t.Fatal(err)
	}
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "24 80") {
			break
		}
	}
	if err = terminal.Resize(ctx, 40, 120); err != nil {
		t.Fatal("Resize", err)
	}
	if _, err = terminal.Write([]byte("size\nhello\n")); err != nil {
		t.Fatal(err)
	}
	gotSize, gotEcho := false, false
	for !gotSize || !gotEcho {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		gotSize = gotSize || strings.Contains(line, "40 120")
		gotEcho = gotEcho || strings.Contains(line, "ECHO:hello")
	}
	if err = terminal.Close(); err != nil {
		t.Fatal("Detach", err)
	}
	terminal, err = client.AttachTerminal(ctx, info.SessionID)
	if err != nil {
		t.Fatal("Reattach", err)
	}
	defer terminal.Close()
	if err = terminal.CloseWrite(ctx); err != nil {
		t.Fatal("Fin", err)
	}
	if err = client.StopSession(ctx, protocol.StopSessionParams{SessionID: info.SessionID, GraceMs: 1000, ForceAfterTimeout: true}); err != nil {
		t.Fatal("Stop", err)
	}
	exit, err := client.WaitSession(ctx, info.SessionID)
	if err != nil {
		t.Fatal("Wait", err)
	}
	tail, err := io.ReadAll(terminal)
	if err != nil {
		t.Fatal("output Fin", err)
	}
	if !strings.Contains(string(tail), "EXIT_TAIL_READY") {
		t.Fatalf("missing tail: %q", tail)
	}
	state, err = client.GetState(ctx)
	if err != nil || len(state.Sessions) != 1 || state.Sessions[0].Exit == nil || state.Sessions[0].GroupState != protocol.GroupStateEmpty {
		t.Fatal("final state", state, err)
	}
	t.Logf("Hello/GetState/Prepare/Start/Bind/UID1000/I-O/Resize/Detach/Rebind/HostFin/Stop/Exit/GuestFin passed; exit=%+v", exit)
}

// TestQEMUWireFailures 验证旧身份、半帧及超长帧后Guest停止服务，Host显式失效并回收VM。
func TestQEMUWireFailures(t *testing.T) {
	if os.Getenv("SAFEAGENT_GUEST_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实 Guest TCG 验证")
	}
	for _, scenario := range []string{"stale-identity", "partial-control", "oversized-control"} {
		t.Run(scenario, func(t *testing.T) {
			c, _, _, channels, _ := qemuTransport(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := c.Hello(ctx); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "stale-identity":
				id, _ := protocol.NewUUID()
				raw, _ := protocol.EncodeRequest(protocol.Request{RequestID: 1000, Epoch: "old", ConnectionID: id, Method: protocol.MethodGetState, TimeoutMs: 1000, Params: []byte("{}")})
				if err := guestwire.Write(channels.Control, protocol.Control, protocol.HostToGuest, protocol.Frame{Kind: protocol.RequestKind, Payload: raw}, time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				probe, stop := context.WithTimeout(ctx, 500*time.Millisecond)
				_, err := c.GetState(probe)
				stop()
				if err == nil {
					t.Fatal("协议故障后仍然服务请求")
				}
				if !errors.Is(err, guest.ErrDisconnected) && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
				// virtio设备关闭不保证Host socket EOF，观察超时后显式结束逻辑连接。
				c.Close()
				if _, err = c.GetState(ctx); !errors.Is(err, guest.ErrDisconnected) {
					t.Fatal(err)
				}
			case "partial-control":
				if _, err := channels.Control.Write([]byte("S")); err != nil {
					t.Fatal(err)
				}
				timer := time.NewTimer(2200 * time.Millisecond)
				defer timer.Stop()
				<-timer.C
				probe, stop := context.WithTimeout(ctx, 500*time.Millisecond)
				_, err := c.GetState(probe)
				stop()
				if err == nil {
					t.Fatal("协议故障后仍然服务请求")
				}
				if !errors.Is(err, guest.ErrDisconnected) && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
				// virtio设备关闭不保证Host socket EOF，观察超时后显式结束逻辑连接。
				c.Close()
				if _, err = c.GetState(ctx); !errors.Is(err, guest.ErrDisconnected) {
					t.Fatal(err)
				}
			case "oversized-control":
				header := []byte{'S', 'A', 'G', 'P', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0, 0, 0}
				if _, err := channels.Control.Write(header); err != nil {
					t.Fatal(err)
				}
				probe, stop := context.WithTimeout(ctx, 500*time.Millisecond)
				_, err := c.GetState(probe)
				stop()
				if err == nil {
					t.Fatal("协议故障后仍然服务请求")
				}
				if !errors.Is(err, guest.ErrDisconnected) && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
				// virtio设备关闭不保证Host socket EOF，观察超时后显式结束逻辑连接。
				c.Close()
				if _, err = c.GetState(ctx); !errors.Is(err, guest.ErrDisconnected) {
					t.Fatal(err)
				}

			}
		})
	}
}

// terminalTap 只供真实设备故障测试保存当前连接，不改变生产装配接口。
type terminalTap struct {
	// mu 保护工厂回调交付的当前终端句柄。
	mu sync.Mutex
	// port 是已打开的真实终端连接。
	port net.Conn
}

// current 返回最近一次由可信工厂交付的终端句柄。
func (t *terminalTap) current() net.Conn { t.mu.Lock(); defer t.mu.Unlock(); return t.port }

// TestQEMUTerminalFaults 在真实设备上验证无效Bind和单独终端断线不停止工作负载。
func TestQEMUTerminalFaults(t *testing.T) {
	if os.Getenv("SAFEAGENT_GUEST_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实 Guest TCG 验证")
	}
	for _, scenario := range []string{"terminal-disconnect", "old-bind", "slow-output"} {
		t.Run(scenario, func(t *testing.T) {
			c, prepare, start, channels, tap := qemuTransport(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			// 内核启动与准备使用本场景20秒预算，不复用内存服务的5秒辅助函数。
			if _, err := c.Hello(ctx); err != nil {
				t.Fatal(err)
			}
			prepared, err := c.PrepareAgent(ctx, prepare)
			if err != nil {
				t.Fatal(err)
			}
			start.PreparedID = prepared.PreparedID
			if _, err := c.StartSession(ctx, start); err != nil {
				t.Fatal(err)
			}
			terminal, err := c.AttachTerminal(ctx, start.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "terminal-disconnect":
				tap.current().Close()
			case "old-bind":
				nonce, _ := protocol.NewNonce()
				id, _ := protocol.NewUUID()
				raw, _ := protocol.Encode(protocol.Bind{SessionID: start.SessionID, Epoch: protocol.Epoch(channels.Instance.Epoch), ConnectionID: id, BindToken: nonce})
				if err = guestwire.Write(tap.current(), protocol.Terminal, protocol.HostToGuest, protocol.Frame{Kind: protocol.BindKind, StreamID: 1, Payload: raw}, time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			case "slow-output":
				if _, err = terminal.Write([]byte("flood\n")); err != nil {
					t.Fatal(err)
				}
				// 用控制查询确认工作负载仍在运行，再停止，终端保持不消费。
				timer := time.NewTimer(100 * time.Millisecond)
				<-timer.C
				if err = c.StopSession(ctx, protocol.StopSessionParams{SessionID: start.SessionID, GraceMs: 100, ForceAfterTimeout: true}); err != nil {
					t.Fatal(err)
				}
				if _, err = c.WaitSession(ctx, start.SessionID); err != nil {
					t.Fatal(err)
				}
				timer = time.NewTimer(2200 * time.Millisecond)
				<-timer.C
				_, err = io.Copy(io.Discard, terminal)
				if !errors.Is(err, guest.ErrOutputIncomplete) {
					t.Fatal("慢读排空未标记不完整", err)
				}
			}
			if scenario != "slow-output" {
				terminal.Close()
				state, err := c.GetState(ctx)
				if err != nil || len(state.Sessions) != 1 || state.Sessions[0].Exit != nil {
					t.Fatal(state, err)
				}
				if err = c.StopSession(ctx, protocol.StopSessionParams{SessionID: start.SessionID, GraceMs: 100, ForceAfterTimeout: true}); err != nil {
					t.Fatal(err)
				}
			} else {
				terminal.Close()
				if _, err = c.GetState(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
