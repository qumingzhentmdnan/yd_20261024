//go:build linux

package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"safeagent/internal/host/gateway"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/proxy"
	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/protocol"
	"safeagent/internal/proxywire"
)

// proxyTransport 额外交付可信私有句柄供真实设备故障探针使用。
func proxyTransport(t *testing.T) (*rpc.Client, protocol.PrepareAgentParams, protocol.StartSessionParams, *qemu.Channels, *vm.Manager, string, *qemu.Driver) {
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
	t.Cleanup(func() {

		os.RemoveAll(root)
	})
	// 诊断包装器仅为本测试保存串口与stderr，实际设备和生命周期仍由生产Driver创建。
	evidence := filepath.Join(image, "runs", strings.ReplaceAll(t.Name(), "/", "-"), fmt.Sprint(time.Now().UnixNano()))
	if err = os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "qemu-diagnostic")
	script := "#!/usr/bin/python3\nimport os,sys\na=sys.argv[1:]\ni=a.index('-serial')\na[i+1]=" + strconv.Quote("file:"+filepath.Join(evidence, "console.log")) + "\nf=os.open(" + strconv.Quote(filepath.Join(evidence, "stderr.log")) + ",os.O_WRONLY|os.O_CREAT,0o600)\nos.dup2(f,2)\nos.execv(" + strconv.Quote(filepath.Join(toolchain, "qemu-system-x86_64")) + ",['qemu-system-x86_64']+a)\n"
	if err = os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Log("本轮串口证据", evidence)
	driver, err := qemu.NewDriver(wrapper, filepath.Join(toolchain, "qemu-img"), root)
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
		if err := manager.Stop(cleanup, instance, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil && !errors.Is(err, vm.ErrNotFound) && !errors.Is(err, vm.ErrStaleInstance) {
			t.Error(err)
		}
		if _, err := manager.Wait(cleanup, instance); err != nil && !errors.Is(err, vm.ErrNotFound) {
			t.Error(err)
		}
		if err := manager.Destroy(cleanup, ref); err != nil && !errors.Is(err, vm.ErrNotFound) {
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

	client, err := rpc.New(instance, channels.Control, channels.OpenTerminal)
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
	return client, protocol.PrepareAgentParams{OperationID: op, ManifestRef: content.ManifestRef, ManifestDigest: content.ManifestDigest, ConfigDigest: content.ConfigDigest, WorkspaceRef: content.WorkspaceRef, WorkspaceDigest: content.WorkspaceDigest}, protocol.StartSessionParams{OperationID: startOp, SessionID: "session", WorkspaceRef: content.WorkspaceRef, WorkspaceDigest: content.WorkspaceDigest, Rows: 24, Cols: 80}, channels, manager, filepath.Join(evidence, "console.log"), driver
}

// TestQEMUProxyIntegration 在独立VM中分别验证正常流、客户端取消、控制失联和上游停滞。
func TestQEMUProxyIntegration(t *testing.T) {
	for _, scenario := range []string{"normal", "client-cancel", "control-loss", "upstream-stall", "slow-upload", "slow-read", "abort-response", "slow-read-stop", "total-timeout"} {
		t.Run(scenario, func(t *testing.T) { runQEMUProxy(t, scenario) })
	}
}

// runQEMUProxy 验证当前race Guest真实设备、非rootHTTP、TLS认证、首块和并行控制终端。
func runQEMUProxy(t *testing.T, scenario string) {
	client, prepare, start, channels, manager, console, _ := proxyTransport(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := client.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	requestStarted := make(chan time.Time, 1)
	var progress atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamCanceled)
		requestStarted <- time.Now()
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != "guest-actual-body" || r.Header.Get("Authorization") != "Bearer explicit-host-test-only" || r.Header.Get("X-Api-Key") != "" {
			t.Error("上游实际数据/认证隔离失败", string(data), err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Authorization", "must-not-leak")
		w.Write([]byte{0, 255, 10, 13, 128})
		w.(http.Flusher).Flush()
		if scenario == "total-timeout" {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					if _, err := w.Write([]byte("progress")); err != nil {
						return
					}
					w.(http.Flusher).Flush()
					progress.Add(1)
				}
			}
		}
		if scenario == "slow-read" || scenario == "slow-read-stop" {
			block := make([]byte, 16384)
			for i := 0; i < 512; i++ {
				if _, err := w.Write(block); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
			return
		}
		select {
		case <-release:
			if scenario == "abort-response" {
				panic(http.ErrAbortHandler)
			}
			w.Write([]byte("tail"))
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	idle := 8 * time.Second
	if scenario == "upstream-stall" {
		idle = 2 * time.Second
	}
	g, _, _ := gatewayWithBudgets(t, upstream, idle, 80*time.Second)
	proxyDone := make(chan error, 1)
	cleaned := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		proxyDone <- proxy.Run(ctx, client, channels, g, gateway.BindingSpec{ServiceRefs: []string{"model"}}, func() { close(ready) }, func(clean context.Context) (cleanupErr error) {
			defer func() { cleaned <- cleanupErr }()
			if scenario == "abort-response" || scenario == "total-timeout" || scenario == "slow-read-stop" {
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					data, _ := os.ReadFile(console)
					marker := "HTTP_ABORT_ERROR"
					if scenario == "slow-read-stop" {
						marker = "HTTP_STOP_TERM"
					}
					found := bytes.Contains(data, []byte(marker))
					if found {
						break
					}
					select {
					case <-clean.Done():
						return fmt.Errorf("Guest异常断言缺失: %w", clean.Err())
					case <-ticker.C:
					}
				}
			}
			if err := manager.Stop(clean, channels.Instance, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
				return err
			}
			if _, err := manager.Wait(clean, channels.Instance); err != nil {
				return err
			}
			return manager.Destroy(clean, channels.Instance.Ref)
		})
	}()
	select {
	case <-ready:
	case err := <-proxyDone:
		t.Fatal("proxy启动失败", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	prepared, err := client.PrepareAgent(ctx, prepare)
	if err != nil {
		t.Fatal(err)
	}
	start.PreparedID = prepared.PreparedID
	if _, err = client.StartSession(ctx, start); err != nil {
		t.Fatal(err)
	}
	terminal, err := client.AttachTerminal(ctx, start.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	reader := bufio.NewReader(terminal)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "READY uid=1000") {
		t.Fatal(line, err)
	}
	command := "http\n"
	switch scenario {
	case "client-cancel":
		command = "http-cancel\n"
	case "slow-upload":
		command = "http-upload\n"
	case "slow-read", "slow-read-stop":
		command = "http-slow\n"
	case "abort-response":
		command = "http-abort\n"
	case "total-timeout":
		command = "http-total\n"
	}
	uploadObserved := false
	commandStarted := time.Now()
	if _, err = terminal.Write([]byte(command)); err != nil {
		t.Fatal(err)
	}
	first, second, uid := false, scenario == "client-cancel" || scenario == "slow-read" || scenario == "slow-read-stop", false
	for !first || !second || !uid {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(line, err)
		}
		t.Log(strings.TrimSpace(line))
		if strings.Contains(line, "HTTP_UPLOAD_TIMED_OUT") {
			uploadObserved = true
			elapsed := time.Since(commandStarted)
			if elapsed < 1500*time.Millisecond || elapsed > 6*time.Second {
				t.Fatal("Guest两秒上传预算与Host实时时间偏差过大", elapsed)
			}
			t.Log("Guest两秒上传预算的Host实测", elapsed)
		}
		if strings.Contains(line, "HTTP_ERROR") {
			t.Fatal(line)
		}
		first = first || strings.Contains(line, "HTTP_FIRST_BINARY")
		second = second || strings.Contains(line, "HTTP_SECOND_503")
		uid = uid || strings.Contains(line, "HTTP_UID:1000")
	}
	if scenario == "slow-upload" && !uploadObserved {
		t.Fatal("缺少本轮Guest上传超时及实时时钟观测")
	}
	// 上游尚未结束时首块已在Guest可见，控制状态查询仍可用。
	if scenario != "client-cancel" {
		if _, err = client.GetState(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if scenario == "slow-read-stop" {
		if _, err = terminal.Write([]byte("queued-during-proxy\n")); err != nil {
			t.Fatal(err)
		}
		if err = terminal.Resize(ctx, 32, 100); err != nil {
			t.Fatal(err)
		}
		// Stop调用期间允许通道故障触发回收；串口证明TERM交付，不证明响应或整组停止完成。
		err = client.StopSession(ctx, protocol.StopSessionParams{SessionID: start.SessionID, GraceMs: 100, ForceAfterTimeout: true})
		if err != nil && !errors.Is(err, guest.ErrDisconnected) {
			t.Fatal(err)
		}
		t.Log("proxy慢读期间Resize成功、Guest收到Stop的TERM；响应断线后VM有界回收", err)
	}
	if scenario == "abort-response" {
		close(release)
	}
	if scenario != "normal" && scenario != "slow-upload" {

		if scenario == "control-loss" {
			if _, err = channels.Control.Write([]byte("BAD")); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case <-upstreamCanceled:
			if scenario == "total-timeout" {
				elapsed := time.Since(<-requestStarted)
				if elapsed < 58*time.Second || elapsed > 70*time.Second || progress.Load() < 200 {
					t.Fatal("总预算或持续进展不符", elapsed, progress.Load())
				}
				t.Log("60秒总预算与持续进展", elapsed, progress.Load())
			}
		case <-time.After(faultWait(scenario)):
			t.Fatal("上游未观察到故障取消")
		}
		select {
		case err = <-proxyDone:
			t.Log("故障回收:", err)
		case <-time.After(15 * time.Second):
			t.Fatal("故障后proxy未结束")
		}
		if cleanupErr := <-cleaned; cleanupErr != nil {
			t.Fatal("Run清理失败", cleanupErr)
		}
		if _, err = client.GetState(ctx); err == nil {
			t.Fatal("故障后控制连接仍然有效")
		}
		t.Log("真实故障撤销、上游取消、Stop/Wait/Destroy通过")
		return
	}
	close(release)
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "HTTP_ERROR") {
			t.Fatal(line)
		}
		if strings.Contains(line, "HTTP_DONE") {
			break
		}
	}
	if _, err = terminal.Write([]byte("after-proxy\n")); err != nil {
		t.Fatal(err)
	}
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "ECHO:after-proxy") {
			break
		}
	}
	if err = client.StopSession(ctx, protocol.StopSessionParams{SessionID: start.SessionID, GraceMs: 1000, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.WaitSession(ctx, start.SessionID); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err = <-proxyDone:
		t.Log("proxy lifecycle ended:", err)
	case <-time.After(12 * time.Second):
		t.Fatal("proxy未完成清理")
	}
	if cleanupErr := <-cleaned; cleanupErr != nil {
		t.Fatal("Run清理失败", cleanupErr)
	}
	t.Log("真实TCG非rootHTTP/TLS认证/SSE首块/二进制/503/并行GetState/终端/Stop通过")
}

// TestQEMUBootStability 在新VM上重复观察启动和Hello，保留逐轮串口以定位偶发超时。
func TestQEMUBootStability(t *testing.T) {
	if os.Getenv("SAFEAGENT_BOOT_STABILITY") != "1" {
		t.Skip("需要显式启用启动稳定性诊断")
	}
	for i := 0; i < 10; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			c, _, _, _, _, console, _ := proxyTransport(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := c.Hello(ctx); err != nil {
				t.Fatal("Hello", err)
			}
			data, err := os.ReadFile(console)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, []byte("smp: Brought up 1 node, 2 CPUs")) {
				t.Fatal("缺少本轮Guest两个CPU实际激活证据")
			}
		})
	}
}

// TestQEMUProxyDeviceFaults 在真实Guest Bridge上注入独立断线、半帧、超长帧和旧请求响应。
func TestQEMUProxyDeviceFaults(t *testing.T) {
	for _, fault := range []string{"disconnect", "partial", "oversized", "stale-id"} {
		t.Run(fault, func(t *testing.T) {
			client, prepare, start, channels, _, console, _ := proxyTransport(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := client.Hello(ctx); err != nil {
				t.Fatal(err)
			}
			port, err := channels.OpenProxy(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer port.Close()
			if err = proxywire.Write(port, proxywire.Frame{Kind: proxywire.Ready}, time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if f, err := proxywire.Read(port, time.Now().Add(time.Second)); err != nil || f.Kind != proxywire.Ready {
				t.Fatal(f, err)
			}
			prepared, err := client.PrepareAgent(ctx, prepare)
			if err != nil {
				t.Fatal(err)
			}
			start.PreparedID = prepared.PreparedID
			if _, err = client.StartSession(ctx, start); err != nil {
				t.Fatal(err)
			}
			terminal, err := client.AttachTerminal(ctx, start.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()
			reader := bufio.NewReader(terminal)
			if _, err = reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			if _, err = terminal.Write([]byte("http\n")); err != nil {
				t.Fatal(err)
			}
			var id uint32
			for {
				f, err := proxywire.Read(port, time.Now().Add(3*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				id = f.ID
				if f.Kind == proxywire.End {
					break
				}
			}
			// 原始畸形帧写入须重新设置预算，不能沿用启动Ready的已过期写截止时间。
			if err = port.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			faultStarted := time.Now()
			switch fault {
			case "disconnect":
				port.Close()
			case "partial":
				_, err = port.Write([]byte("S"))
			case "oversized":
				_, err = port.Write([]byte{'S', 'A', 'P', 'X', 0, 1, 3, 0, 0, 1, 0, 0, 0, 0, 0, 1})
			case "stale-id":
				data, _ := proxywire.Encode(proxywire.ResponseMeta{Status: 200})
				err = proxywire.Write(port, proxywire.Frame{Kind: proxywire.Response, ID: id + 1, Payload: data}, time.Now().Add(time.Second))
			}
			if err != nil {
				t.Fatal(err)
			}
			// Guest设备故障须使control服务失效；持续有界Probe观察，不假定Host EOF。
			deadline := time.NewTimer(6 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-deadline.C:
					t.Fatal("Guest proxy故障未失效control")
				case <-tick.C:
					probe, stop := context.WithTimeout(ctx, 500*time.Millisecond)
					err = client.Probe(probe)
					stop()
					if err != nil {
						data, readErr := os.ReadFile(console)
						if readErr != nil {
							t.Fatal(readErr)
						}
						if !bytes.Contains(data, []byte("control connection ended; VM restart required")) {
							t.Fatal("缺少本轮Guest控制服务退出证据")
						}
						if fault == "partial" {
							elapsed := time.Since(faultStarted)
							if elapsed < 1500*time.Millisecond || elapsed > 6*time.Second {
								t.Fatal("Guest两秒半帧预算与Host实时时间偏差过大", elapsed)
							}
							t.Log("Guest两秒半帧预算的Host实测", elapsed)
						}
						t.Log("真实proxy故障使Guest控制服务退出且Host探测失败", fault, err)
						return
					}
				}
			}
		})
	}
}

// faultWait 为真实60秒总预算场景提供明确观察窗口，其它故障仍限定6秒。
func faultWait(scenario string) time.Duration {
	if scenario == "total-timeout" {
		return 65 * time.Second
	}
	return 6 * time.Second
}

// TestQEMUProxyStartupCancellation 验证启动前取消和Ready提交时取消均不会留下绑定或端点。
func TestQEMUProxyStartupCancellation(t *testing.T) {
	for _, stage := range []string{"before-open", "ready"} {
		t.Run(stage, func(t *testing.T) {
			client, _, _, channels, manager, _, _ := proxyTransport(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := client.Hello(ctx); err != nil {
				t.Fatal(err)
			}
			instance, id, err := client.Identity()
			if err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("取消的启动不应调用上游") }))
			defer upstream.Close()
			g, _, _ := newGateway(t, upstream)
			if stage == "before-open" {
				cancel()
			}
			readyCalled := false
			cleanupCalled := false
			var cleanupResult error
			err = proxy.Run(ctx, client, channels, g, gateway.BindingSpec{ServiceRefs: []string{"model"}}, func() { readyCalled = true; cancel() }, func(clean context.Context) (cleanupErr error) {
				defer func() { cleanupResult = cleanupErr }()
				cleanupCalled = true
				if clean.Err() != nil {
					t.Fatal("清理context继承了请求取消")
				}
				if err := manager.Stop(clean, channels.Instance, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
					return err
				}
				if _, err := manager.Wait(clean, channels.Instance); err != nil {
					return err
				}
				return manager.Destroy(clean, channels.Instance.Ref)
			})
			if cleanupResult != nil {
				t.Fatal("Run启动取消清理失败", cleanupResult)
			}
			if err == nil || !cleanupCalled || readyCalled != (stage == "ready") {
				t.Fatal("启动取消语义错误", err, cleanupCalled, readyCalled)
			}
			// 同一身份重新绑定必须成功：若旧绑定残留，Gateway会返回Conflict。
			binding, err := g.Bind(context.Background(), gateway.ChannelIdentity{Instance: instance, ConnectionID: id}, gateway.BindingSpec{ServiceRefs: []string{"model"}})
			if err != nil {
				t.Fatal("启动结束残留绑定", err)
			}
			g.Revoke(context.Background(), binding)
			if _, _, err = client.Identity(); err == nil {
				t.Fatal("启动取消后控制身份仍有效")
			}
		})
	}
}

// TestQEMUProxyEpochReplacement 在同一Manager停止并重启同一资源，验证新握手身份与旧授权隔离。
func TestQEMUProxyEpochReplacement(t *testing.T) {
	old, _, _, channels, manager, _, driver := proxyTransport(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := old.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	first, oldID, err := old.Identity()
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(w, r.Body) }))
	defer upstream.Close()
	g, _, _ := newGateway(t, upstream)
	oldIdentity := gateway.ChannelIdentity{Instance: first, ConnectionID: oldID}
	binding, err := g.Bind(ctx, oldIdentity, gateway.BindingSpec{ServiceRefs: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	if err = manager.Stop(ctx, first, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Wait(ctx, first); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Start(ctx, channels.Instance.Ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := manager.Stop(clean, second, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true}); err != nil {
			t.Error(err)
		}
		if _, err := manager.Wait(clean, second); err != nil {
			t.Error(err)
		}
	})
	fresh, err := driver.OpenGuestChannels(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	current, err := rpc.New(second, fresh.Control, fresh.OpenTerminal)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if _, err = current.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	instance, newID, err := current.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if instance.Ref != first.Ref || instance.Epoch == first.Epoch || newID == oldID {
		t.Fatal("重启身份未隔离")
	}
	replacement, err := g.Bind(ctx, gateway.ChannelIdentity{Instance: instance, ConnectionID: newID}, gateway.BindingSpec{ServiceRefs: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []gateway.BindingID{binding, replacement} {
		_, err = g.Serve(ctx, oldIdentity, stale, gateway.ProxyRequest{ServiceRef: "model", Method: "POST", Path: "/v1/responses", Body: io.NopCloser(strings.NewReader("old"))})
		if !errors.Is(err, gateway.ErrForbidden) {
			t.Fatal("旧epoch或绑定未拒绝", err)
		}
	}
	_, err = g.Serve(ctx, gateway.ChannelIdentity{Instance: instance, ConnectionID: newID}, binding, gateway.ProxyRequest{ServiceRef: "model", Method: "POST", Path: "/v1/responses", Body: io.NopCloser(strings.NewReader("old-binding"))})
	if !errors.Is(err, gateway.ErrForbidden) {
		t.Fatal("新身份仍可使用旧绑定", err)
	}
	response, err := g.Serve(ctx, gateway.ChannelIdentity{Instance: instance, ConnectionID: newID}, replacement, gateway.ProxyRequest{ServiceRef: "model", Method: "POST", Path: "/v1/responses", Body: io.NopCloser(strings.NewReader("new-epoch"))})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(data) != "new-epoch" {
		t.Fatal(string(data), err)
	}
	t.Log("同一资源真实重启Hello与Gateway新epoch绑定、旧身份拒绝通过；本场景业务HTTP在Host执行")
}
