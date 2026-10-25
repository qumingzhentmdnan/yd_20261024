package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/host/gateway"
	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
	"safeagent/internal/proxywire"
	"safeagent/internal/testkit/fakecredential"
)

// lifecycleRequest 读取真实控制请求，以关闭对端限定故障测试的最大等待时间。
func lifecycleRequest(t *testing.T, peer net.Conn) protocol.Request {
	t.Helper()
	timer := time.AfterFunc(5*time.Second, func() { peer.Close() })
	defer timer.Stop()
	frame, err := guestwire.Read(peer, protocol.Control, protocol.HostToGuest)
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocol.DecodeRequest(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// lifecycleResponse 使用真实协议关联编码器发送成功或业务错误响应。
func lifecycleResponse(t *testing.T, peer net.Conn, request protocol.Request, result any, failure *protocol.Error) {
	t.Helper()
	response := protocol.Response{RequestID: request.RequestID, Epoch: request.Epoch, ConnectionID: request.ConnectionID, Error: failure}
	if failure == nil {
		var err error
		response.Result, err = protocol.Encode(result)
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := protocol.EncodeResponse(response, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = guestwire.Write(peer, protocol.Control, protocol.GuestToHost, protocol.Frame{Kind: protocol.ResponseKind, Payload: data}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

// lifecycleClient 让真实rpc.Client完成握手，保留可注入非法响应的控制对端。
func lifecycleClient(t *testing.T) (*rpc.Client, net.Conn, vm.VMInstance, protocol.UUID) {
	return lifecycleClientWithWrite(t, nil)
}

// probeWriteConn 观察实际控制Write入口，不读取对端数据以保持同步写阻塞。
type probeWriteConn struct {
	// Conn 保留真实内存连接的deadline及关闭行为。
	net.Conn
	// started 有界通知Write已进入，测试在握手后清空历史通知。
	started chan struct{}
}

// Write 通知测试后调用真实同步写入，对端不读时一直阻塞到取消或deadline。
func (c probeWriteConn) Write(data []byte) (int, error) {
	select {
	case c.started <- struct{}{}:
	default:
	}
	return c.Conn.Write(data)
}

// lifecycleClientWithWrite 完成真实握手，并可额外观察Probe实际进入同步写入的时点。
func lifecycleClientWithWrite(t *testing.T, started chan struct{}) (*rpc.Client, net.Conn, vm.VMInstance, protocol.UUID) {
	t.Helper()
	instance := vm.VMInstance{Ref: vm.VMRef{ID: "lifecycle"}, Epoch: "epoch"}
	host, peer := net.Pipe()
	if started != nil {
		host = probeWriteConn{Conn: host, started: started}
	}
	client, err := rpc.New(instance, host, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close(); client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.Hello(ctx); done <- err }()
	request := lifecycleRequest(t, peer)
	var params protocol.HelloParams
	if err = protocol.Decode(request.Params, &params); err != nil {
		t.Fatal(err)
	}
	lifecycleResponse(t, peer, request, protocol.HelloResult{Version: "0.1", Challenge: params.Challenge, GuestBuild: "test", ImageDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}, nil)
	request = lifecycleRequest(t, peer)
	lifecycleResponse(t, peer, request, protocol.GuestState{Sessions: []protocol.SessionInfo{}}, nil)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	return client, peer, instance, request.ConnectionID
}

// lifecycleGateway 创建不接触上游的正式绑定状态，验证统一Run生命周期而非QEMU设备。
func lifecycleGateway(t *testing.T) *gateway.Gateway {
	t.Helper()
	service, err := gateway.CLIService("codex", "https://unused.example", "test-key", []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	if err != nil {
		t.Fatal(err)
	}
	g, err := gateway.New(gateway.Config{Services: []gateway.Service{service}, MaxBindings: 1, MaxConcurrent: 1, MaxRequestBytes: 1024, MaxResponseBytes: 1024, MaxHeaderBytes: 4096, TotalTimeout: 5 * time.Second, IdleTimeout: time.Second}, fakecredential.New(nil), net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

// waitLifecycle 在有界窗口等待明确事件，不使用sleep猜测故障与外部取消的顺序。
func waitLifecycle(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("生命周期事件未按预算完成")
	}
}

// TestRunPreservesControlFailureBeforeExternalCancel 验证真实控制/Probe故障先发生且紧邻主动取消时仍保留原始原因。
func TestRunPreservesControlFailureBeforeExternalCancel(t *testing.T) {
	for _, scenario := range []string{"probe-capacity", "probe-protocol", "probe-deadline", "probe-write-deadline", "control-protocol", "normal-idle", "normal-active", "normal-blocked-write", "cleanup-error", "cleanup-canceled", "cleanup-closed", "parent-deadline"} {
		t.Run(scenario, func(t *testing.T) {
			writes := make(chan struct{}, 16)
			client, control, instance, id := lifecycleClientWithWrite(t, writes)
			for len(writes) > 0 {
				<-writes
			}
			g := lifecycleGateway(t)
			port, peer := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			if scenario == "parent-deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			}
			ready, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseRun := func() { releaseOnce.Do(func() { close(release) }) }
			var cleanupErr error
			switch scenario {
			case "cleanup-error":
				cleanupErr = errors.New("destroy failed")
			case "cleanup-canceled":
				cleanupErr = context.Canceled
			case "cleanup-closed":
				cleanupErr = net.ErrClosed
			}
			cleanupCalls := 0
			result := make(chan error, 1)
			go func() {
				defer close(exited)
				result <- run(ctx, client, func(context.Context, vm.VMInstance) (net.Conn, error) { return port, nil }, g, gateway.BindingSpec{ServiceRefs: []string{"model"}}, func() {
					// 在生命周期主任务暂停期间注入故障并主动取消，确保归一化发生在两者之后。
					close(ready)
					<-release
				}, func(clean context.Context) error {
					cleanupCalls++
					if clean.Err() != nil {
						t.Error("清理继承了已取消context")
					}
					return cleanupErr
				})
			}()
			t.Cleanup(func() {
				cancel()
				releaseRun()
				port.Close()
				peer.Close()
				waitLifecycle(t, exited)
			})
			frame, err := proxywire.Read(peer, time.Now().Add(5*time.Second))
			if err != nil || frame.Kind != proxywire.Ready {
				t.Fatal(frame, err)
			}
			if err = proxywire.Write(peer, proxywire.Frame{Kind: proxywire.Ready}, time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			waitLifecycle(t, ready)
			var canceledAt time.Time
			switch scenario {
			case "normal-blocked-write", "probe-write-deadline":
				// 仅观察Write入口，绝不读取Probe帧；真实net.Pipe写因此被对端背压阻塞。
				waitLifecycle(t, writes)
				if scenario == "normal-blocked-write" {
					canceledAt = time.Now()
					cancel()
				}
				waitLifecycle(t, client.Done())
			case "probe-capacity", "probe-protocol", "probe-deadline", "normal-active":
				request := lifecycleRequest(t, control)
				if request.Method != protocol.MethodGetState {
					t.Fatal(request.Method)
				}
				if scenario == "probe-capacity" {
					lifecycleResponse(t, control, request, nil, protocol.Failure(protocol.CodeResourceExhausted))
				}
				if scenario == "probe-protocol" {
					request.RequestID++
					lifecycleResponse(t, control, request, protocol.GuestState{Sessions: []protocol.SessionInfo{}}, nil)
				}
				if scenario == "normal-active" {
					cancel()
				}
				waitLifecycle(t, client.Done())
			case "control-protocol":
				request := protocol.Request{RequestID: 1024, Epoch: protocol.Epoch(instance.Epoch), ConnectionID: id, Method: protocol.MethodGetState, TimeoutMs: 1000, Params: []byte(`{}`)}
				lifecycleResponse(t, control, request, protocol.GuestState{Sessions: []protocol.SessionInfo{}}, nil)
				waitLifecycle(t, client.Done())
			case "parent-deadline":
				waitLifecycle(t, ctx.Done())
			}
			// 故障路径以Done确认首次原因已固定，再主动取消，不能靠调度碰运气。
			cancel()
			releaseRun()
			waitLifecycle(t, exited)
			if !canceledAt.IsZero() && time.Since(canceledAt) > time.Second {
				t.Fatal("主动取消仍等待原两秒写deadline", time.Since(canceledAt))
			}
			err = <-result
			switch scenario {
			case "probe-capacity":
				var business *protocol.Error
				if !errors.As(err, &business) || business.Code != protocol.CodeResourceExhausted {
					t.Fatal("Probe业务错误被吞", err)
				}
			case "probe-protocol", "control-protocol":
				if !errors.Is(err, protocol.ErrFrame) {
					t.Fatal("控制协议错误被吞", err)
				}
			case "probe-deadline", "parent-deadline":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("deadline原因被吞", err)
				}
			case "probe-write-deadline":
				var timeout net.Error
				if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &timeout) || !timeout.Timeout()) {
					t.Fatal("取消前真实写timeout被吞", err)
				}
			case "cleanup-error", "cleanup-canceled", "cleanup-closed":
				if !errors.Is(err, cleanupErr) {
					t.Fatal("真实cleanup错误被吞", err)
				}
			default:
				if err != nil {
					t.Fatal("正常主动取消没有成功收尾", err)
				}
			}
			if cleanupCalls != 1 {
				t.Fatal("清理次数错误", cleanupCalls)
			}
			binding, err := g.Bind(context.Background(), gateway.ChannelIdentity{Instance: instance, ConnectionID: string(id)}, gateway.BindingSpec{ServiceRefs: []string{"model"}})
			if err != nil {
				t.Fatal("生命周期结束仍残留绑定", err)
			}
			g.Revoke(context.Background(), binding)
		})
	}
}
