//go:build linux

package workspaceimport_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"safeagent/internal/host/gateway"
	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/proxy"
	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/host/workspace"
	"safeagent/internal/host/workspaceimport"
	"safeagent/internal/protocol"
	"safeagent/internal/proxywire"
	"safeagent/internal/testkit/fakecredential"
	"safeagent/internal/workspacebundle"
	"safeagent/internal/workspacewire"
)

// realGuest 使用当前生产Driver构造独立TCG实例，并分阶段注册独立清理。
func realGuest(t *testing.T) (*rpc.Client, *qemu.Channels, protocol.Preparation) {
	t.Helper()
	if os.Getenv("SAFEAGENT_GUEST_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实TCG验证")
	}
	image := os.Getenv("SAFEAGENT_WORKSPACE_IMAGE")
	if image == "" {
		t.Fatal("必须指定独立的SAFEAGENT_WORKSPACE_IMAGE")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	config := vm.VMConfig{ID: "workspace", Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: vm.TCG, VCPUs: 2, MemoryMiB: 512, Image: vm.Image{KernelPath: filepath.Join(assets, "guest-kernel/boot/vmlinuz-6.8.0-147-generic"), InitrdPath: filepath.Join(image, "initramfs.cpio.gz"), BaseDiskPath: base}}
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

// baseline 创建真实不可变工作区，保留中文和二进制样本供Guest读取验证。
func baseline(t *testing.T) (*workspace.Store, workspace.Baseline, []byte) {
	t.Helper()
	source := t.TempDir()
	binary := bytes.Repeat([]byte{0, 255, 128, 10, 13}, 20000)
	entries := map[string][]byte{"imported.txt": []byte("真实工作区内容\n"), "binary.bin": binary, "run.sh": []byte("#!/bin/sh\nprintf 'WORKSPACE_EXEC_UID='; id -u\n"), "空文件": {}}
	paths := make([]string, 0, len(entries))
	for name, data := range entries {
		mode := os.FileMode(0644)
		if name == "run.sh" {
			mode = 0755
		}
		if err := os.WriteFile(filepath.Join(source, name), data, mode); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	store, err := workspace.NewStore(filepath.Join(t.TempDir(), "store"), workspacebundle.GuestLimits())
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Stage(context.Background(), workspace.ImportSelection{Root: source, Paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenImport(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return store, value, data
}

// preparation 把实际基线与镜像固定程序配置组成Prepare请求。
func preparation(content protocol.Preparation, base workspace.Baseline) protocol.PrepareAgentParams {
	id, _ := protocol.NewUUID()
	return protocol.PrepareAgentParams{OperationID: id, ManifestRef: content.ManifestRef, ManifestDigest: content.ManifestDigest, ConfigDigest: content.ConfigDigest, WorkspaceRef: protocol.OpaqueID(base.ID), WorkspaceDigest: protocol.Digest(base.Digest)}
}

// TestQEMUWorkspaceImport 验证生产Host到Guest导入、真实Prepare与非rootPTY，以及故障回收。
func TestQEMUWorkspaceImport(t *testing.T) {
	for _, scenario := range []string{"normal", "wrong-epoch", "corrupt", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			client, channels, content := realGuest(t)
			store, base, data := baseline(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := client.PrepareAgent(ctx, preparation(content, base)); err == nil {
				t.Fatal("导入前Prepare成功")
			}
			if scenario != "normal" {
				port, err := channels.OpenWorkspace(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer port.Close()
				instance, id, err := client.Identity()
				if err != nil {
					t.Fatal(err)
				}
				begin := workspacewire.BeginMeta{VMRef: protocol.VMRef(instance.Ref.ID), Epoch: protocol.Epoch(instance.Epoch), ConnectionID: protocol.UUID(id), WorkspaceRef: protocol.OpaqueID(base.ID), WorkspaceDigest: protocol.Digest(base.Digest), ArchiveBytes: base.ArchiveBytes}
				if scenario == "wrong-epoch" {
					begin.Epoch = "old"
				}
				payload, err := workspacewire.Encode(begin)
				if err != nil {
					t.Fatal(err)
				}
				if err = workspacewire.Write(port, workspacewire.Frame{Kind: workspacewire.Begin, Payload: payload}, time.Now().Add(5*time.Second)); err != nil {
					t.Fatal(err)
				}
				frame, err := workspacewire.Read(port, time.Now().Add(5*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "wrong-epoch" {
					if frame.Kind != workspacewire.Rejected {
						t.Fatal(frame)
					}
					return
				}
				if frame.Kind != workspacewire.Ready {
					t.Fatal(frame)
				}
				// Ready后尚未交付归档，控制查询与Prepare必须保持可用且拒绝过早准备。
				if _, err = client.GetState(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err = client.PrepareAgent(ctx, preparation(content, base)); err == nil {
					t.Fatal("接收中Prepare成功")
				}
				if scenario == "cancel" {
					port.Close()
					// Guest设备关闭不保证Host收到EOF，使用现有有界Probe观察控制失效。
					observation, stop := context.WithTimeout(ctx, 10*time.Second)
					defer stop()
					ticker := time.NewTicker(time.Second)
					defer ticker.Stop()
					for {
						probe, finish := context.WithTimeout(observation, 2*time.Second)
						probeErr := client.Probe(probe)
						finish()
						if probeErr != nil {
							t.Log("有界Probe观察到导入失效", probeErr)
							return
						}
						select {
						case <-ticker.C:
						case <-observation.Done():
							t.Fatal("Guest导入失效未被Probe观察到")
						}
					}
				}
				data[bytes.Index(data, []byte{0, 255, 128, 10, 13})] ^= 1
				for len(data) > 0 {
					n := min(len(data), workspacewire.Limit)
					if err = workspacewire.Write(port, workspacewire.Frame{Kind: workspacewire.Data, Payload: data[:n]}, time.Now().Add(5*time.Second)); err != nil {
						t.Fatal(err)
					}
					data = data[n:]
				}
				if err = workspacewire.Write(port, workspacewire.Frame{Kind: workspacewire.End}, time.Now().Add(5*time.Second)); err != nil {
					t.Fatal(err)
				}
				frame, err = workspacewire.Read(port, time.Now().Add(5*time.Second))
				if err != nil || frame.Kind != workspacewire.Rejected {
					t.Fatal(frame, err)
				}
				return
			}
			record, err := workspaceimport.Import(ctx, client, channels, store, base.ID)
			if err != nil {
				t.Fatal(err)
			}
			if record.WorkspaceRef != protocol.OpaqueID(base.ID) || record.TotalBytes != base.TotalBytes {
				t.Fatal(record)
			}
			prepared, err := client.PrepareAgent(ctx, preparation(content, base))
			if err != nil {
				t.Fatal(err)
			}
			id, _ := protocol.NewUUID()
			session, err := client.StartSession(ctx, protocol.StartSessionParams{OperationID: id, SessionID: "imported", PreparedID: prepared.PreparedID, WorkspaceRef: record.WorkspaceRef, WorkspaceDigest: record.WorkspaceDigest, Rows: 24, Cols: 80})
			if err != nil {
				t.Fatal(err)
			}
			terminal, err := client.AttachTerminal(ctx, session.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()
			if _, err = terminal.Write([]byte("workspace\n")); err != nil {
				t.Fatal(err)
			}
			output := make(chan []byte, 1)
			go func() {
				var all []byte
				buffer := make([]byte, 4096)
				for {
					n, e := terminal.Read(buffer)
					all = append(all, buffer[:n]...)
					if bytes.Contains(all, []byte("WORKSPACE_EXEC_UID=1000")) || e != nil {
						output <- all
						return
					}
				}
			}()
			select {
			case value := <-output:
				expected := fmt.Sprintf("%x", sha256.Sum256(bytes.Repeat([]byte{0, 255, 128, 10, 13}, 20000)))
				if !bytes.Contains(value, []byte("/workspace/project")) || !bytes.Contains(value, []byte("真实工作区内容")) || !bytes.Contains(value, []byte(expected)) || !bytes.Contains(value, []byte("WORKSPACE_EXEC_UID=1000")) {
					t.Fatal(string(value))
				}
			case <-ctx.Done():
				terminal.Close()
				<-output
				t.Fatal(ctx.Err())
			}
			if err = terminal.Resize(ctx, 40, 100); err != nil {
				t.Fatal(err)
			}
			if err = client.StopSession(ctx, protocol.StopSessionParams{SessionID: session.SessionID, GraceMs: 1000, ForceAfterTimeout: true}); err != nil {
				t.Fatal(err)
			}
			exit, err := client.WaitSession(ctx, session.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("实际Guest进程退出", exit)
			if _, err = workspaceimport.Import(ctx, client, channels, store, base.ID); err == nil {
				t.Fatal("重复导入成功")
			}
		})
	}
}

// TestGuestLimitPreflight 验证Guest较小限额在打开并发送Begin前被明确拒绝。
func TestGuestLimitPreflight(t *testing.T) {
	store, base, _ := baseline(t)
	limits := workspacebundle.GuestLimits()
	limits.MaxFileBytes = 1
	_, _, reader, err := store.OpenImportForGuest(context.Background(), base.ID, limits)
	if reader != nil {
		reader.Close()
	}
	if !errors.Is(err, workspace.ErrLimitExceeded) {
		t.Fatal(err)
	}
}

// TestQEMUWorkspaceProxy 验证导入完成屏障以及同一VM内真实工作区与Host认证代理联调。
func TestQEMUWorkspaceProxy(t *testing.T) {
	client, channels, content := realGuest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	store, base, _ := baseline(t)
	port, err := channels.OpenProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	// 故意提前交付Ready，Guest导入完成前不能确认或开放HTTP服务。
	if err = proxywire.Write(port, proxywire.Frame{Kind: proxywire.Ready}, time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = proxywire.Read(port, time.Now().Add(300*time.Millisecond)); err == nil {
		t.Fatal("导入前确认了proxy Ready")
	} else {
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal(err)
		}
	}
	imported, err := workspaceimport.Import(ctx, client, channels, store, base.ID)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, e := io.ReadAll(r.Body)
		if e != nil || string(body) != "guest-actual-body" || r.Header.Get("Authorization") != "Bearer explicit-import-test-only" {
			t.Error("真实上游输入或Host认证错误", string(body), e)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte{0, 255, 10, 13, 128})
		w.(http.Flusher).Flush()
		select {
		case <-release:
			w.Write([]byte("tail"))
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	gatewayClient, err := gateway.New(gateway.Config{Services: []gateway.Service{{Ref: "model", Origin: upstream.URL, Paths: []string{"/v1/responses"}, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, CredentialRef: "fake", Auth: gateway.Bearer}}, MaxBindings: 1, MaxConcurrent: 1, MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxHeaderBytes: 16384, TotalTimeout: 20 * time.Second, IdleTimeout: 8 * time.Second, RootCAs: roots}, fakecredential.New(map[string]string{"fake": "explicit-import-test-only"}), net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	defer gatewayClient.Close()
	instance, connection, err := client.Identity()
	if err != nil {
		t.Fatal(err)
	}
	identity := gateway.ChannelIdentity{Instance: instance, ConnectionID: connection}
	binding, err := gatewayClient.Bind(ctx, identity, gateway.BindingSpec{ServiceRefs: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	proxyContext, stopProxy := context.WithCancel(ctx)
	done := make(chan error, 1)
	frame, err := proxywire.Read(port, time.Now().Add(2*time.Second))
	if err != nil || frame.Kind != proxywire.Ready {
		t.Fatal(frame, err)
	}
	go func() { done <- proxy.Serve(proxyContext, port, gatewayClient, identity, binding) }()
	defer func() { gatewayClient.Revoke(context.Background(), binding); stopProxy(); port.Close(); <-done }()
	prepared, err := client.PrepareAgent(ctx, preparation(content, base))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := protocol.NewUUID()
	session, err := client.StartSession(ctx, protocol.StartSessionParams{OperationID: id, SessionID: "with-proxy", PreparedID: prepared.PreparedID, WorkspaceRef: imported.WorkspaceRef, WorkspaceDigest: imported.WorkspaceDigest, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := client.AttachTerminal(ctx, session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if _, err = terminal.Write([]byte("workspace\nhttp\n")); err != nil {
		t.Fatal(err)
	}
	output := make(chan []byte, 1)
	go func() {
		var all []byte
		buffer := make([]byte, 4096)
		for {
			n, e := terminal.Read(buffer)
			all = append(all, buffer[:n]...)
			if bytes.Contains(all, []byte("HTTP_SECOND_503")) {
				releaseOnce.Do(func() { close(release) })
			}
			if bytes.Contains(all, []byte("HTTP_DONE")) || e != nil {
				output <- all
				return
			}
		}
	}()
	select {
	case value := <-output:
		for _, marker := range []string{"真实工作区内容", "WORKSPACE_EXEC_UID=1000", "HTTP_UID:1000", "HTTP_FIRST_BINARY", "HTTP_SECOND_503", "HTTP_DONE"} {
			if !bytes.Contains(value, []byte(marker)) {
				t.Fatal("缺少实际Guest标记", marker, string(value))
			}
		}
	case <-ctx.Done():
		terminal.Close()
		<-output
		t.Fatal(ctx.Err())
	}
	if err = client.StopSession(ctx, protocol.StopSessionParams{SessionID: session.SessionID, GraceMs: 1000, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.WaitSession(ctx, session.SessionID); err != nil {
		t.Fatal(err)
	}
	t.Log("真实Imported→Gateway Ready→Prepare→非root工作区与HTTP→退出通过")
}
