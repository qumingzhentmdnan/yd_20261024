// Package proxy 将可信Guest通道装配到现有Gateway，不复制出站策略。
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"safeagent/internal/host/gateway"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/proxywire"
)

// Run 拥有绑定及VM清理；调用方主动取消只剔除传输关闭错误，超时、协议及清理失败仍返回，不关闭共享Gateway。
func Run(ctx context.Context, client *rpc.Client, channels *qemu.Channels, g *gateway.Gateway, spec gateway.BindingSpec, ready func(), cleanup func(context.Context) error) (result error) {
	return run(ctx, client, func(ctx context.Context, instance vm.VMInstance) (net.Conn, error) {
		if instance != channels.Instance {
			return nil, gateway.ErrForbidden
		}
		return channels.OpenProxy(ctx)
	}, g, spec, ready, cleanup)
}

// run 统一拥有可信端口装配、探测及清理，等待子任务退出后合并真实失败原因。
func run(ctx context.Context, client *rpc.Client, open func(context.Context, vm.VMInstance) (net.Conn, error), g *gateway.Gateway, spec gateway.BindingSpec, ready func(), cleanup func(context.Context) error) (result error) {
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	var probeErr error
	// 唯一装配任务串行启动与清理，回调只发布取消，不能重入绑定。
	defer func() {
		// 探测任务已由内层defer等待退出；先取首次原因，不能把本地主动Close当作真实断线。
		controlErr := client.Failure()
		client.Close()
		if controlErr == nil {
			// 读循环可能在快照与Close之间先固定协议错误；Close等待它退出后必须再次读取。
			controlErr = client.Failure()
			if controlErr == guest.ErrDisconnected {
				controlErr = nil
			}
		}
		parentErr := ctx.Err()
		if parentErr == context.Canceled {
			parentErr = nil
			controlErr = withoutRunErrors(controlErr, context.Canceled)
			// Probe的断线包装由controlErr中的首次原因说明，不能把正常取消包装成真实故障。
			probeErr = withoutRunErrors(probeErr, context.Canceled, guest.ErrDisconnected)
		}
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		result = errors.Join(runError(ctx, result), parentErr, controlErr, probeErr, cleanup(clean))
	}()
	instance, id, err := client.Identity()
	if err != nil {
		return err
	}
	stopped := make(chan struct{})
	stopControl := context.AfterFunc(controlContext{client.Done()}, func() { defer close(stopped); cancel() })
	defer func() {
		if !stopControl() {
			<-stopped
		}
	}()
	port, err := open(life, instance)
	if err != nil {
		return err
	}
	defer port.Close()
	identity := gateway.ChannelIdentity{Instance: instance, ConnectionID: id}
	binding, err := g.Bind(life, identity, spec)
	if err != nil {
		return err
	}
	// 生命周期取消先撤销，再关闭设备以解除I/O；唯一回调与最终清理共享顺序。
	var terminateOnce sync.Once
	terminate := func() { terminateOnce.Do(func() { g.Revoke(context.Background(), binding); port.Close() }) }
	closed := make(chan struct{})
	stopPort := context.AfterFunc(life, func() { defer close(closed); terminate() })
	defer func() {
		terminate()
		if !stopPort() {
			<-closed
		}
	}()
	if err = life.Err(); err != nil {
		return err
	}
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-life.Done():
				return
			case <-ticker.C:
				probe, stop := context.WithTimeout(life, proxywire.Budget)
				err := client.Probe(probe)
				stop()
				if err != nil {
					probeErr = err
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-probeDone }()
	if err = proxywire.Write(port, proxywire.Frame{Kind: proxywire.Ready}, proxywire.Deadline(time.Time{})); err != nil {
		return err
	}
	confirmation, err := proxywire.Read(port, proxywire.Deadline(time.Time{}))
	if err != nil {
		return err
	}
	if confirmation.Kind != proxywire.Ready {
		return proxywire.ErrFrame
	}
	if err = life.Err(); err != nil {
		return err
	}
	ready()
	return serve(life, port, g, identity, binding)
}

// controlContext 仅将具体RPC失效信号接入context取消，不引入后台转发goroutine。
type controlContext struct {
	// done 由RPC Client唯一关闭。
	done <-chan struct{}
}

// Deadline 控制失效信号没有独立截止时间。
func (controlContext) Deadline() (time.Time, bool) { return time.Time{}, false }

// Done 返回RPC失效信号。
func (c controlContext) Done() <-chan struct{} { return c.done }

// Err 在RPC失效后返回取消。
func (c controlContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

// Value 不传递业务context值。
func (controlContext) Value(any) any { return nil }

// Serve 处理一个在途请求，调用者负责可信绑定、失效撤销和物理通道回收。
func Serve(ctx context.Context, c net.Conn, g *gateway.Gateway, identity gateway.ChannelIdentity, binding gateway.BindingID) error {
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(stopped); c.Close() })
	defer func() {
		if !stop() {
			<-stopped
		}
	}()
	return serve(ctx, c, g, identity, binding)
}

// serve 由唯一生命周期所有者关闭通道，避免撤销与重复关闭回调竞争。
func serve(ctx context.Context, c net.Conn, g *gateway.Gateway, identity gateway.ChannelIdentity, binding gateway.BindingID) error {
	var previous uint32
	for {
		f, err := proxywire.Read(c, time.Time{})
		if err != nil {
			return err
		}
		if f.Kind != proxywire.Request || f.ID <= previous {
			return proxywire.ErrFrame
		}
		previous = f.ID
		if err = serveRequest(ctx, c, g, identity, binding, f); err != nil {
			return err
		}
	}
}

// serveRequest 限制帧传输与累计容量，上游等待由Gateway空闲超时管理。
func serveRequest(ctx context.Context, c net.Conn, g *gateway.Gateway, identity gateway.ChannelIdentity, binding gateway.BindingID, first proxywire.Frame) error {
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var meta proxywire.RequestMeta
	if err := proxywire.Decode(first.Payload, &meta); err != nil {
		return err
	}
	var body bytes.Buffer
	for {
		f, err := proxywire.Read(c, proxywire.Deadline(time.Time{}))
		if err != nil {
			return err
		}
		if f.ID != first.ID {
			return proxywire.ErrFrame
		}
		if f.Kind == proxywire.End {
			break
		}
		if f.Kind != proxywire.Data || body.Len()+len(f.Payload) > proxywire.RequestLimit {
			return proxywire.ErrFrame
		}
		body.Write(f.Payload)
	}
	response, err := g.Serve(requestCtx, identity, binding, gateway.ProxyRequest{ServiceRef: meta.Service, Method: meta.Method, Path: meta.Path, Headers: meta.Headers, Body: io.NopCloser(bytes.NewReader(body.Bytes()))})
	if err != nil {
		// 错误只暴露固定中止，不将内部错误或凭据文本送入Guest。
		abortErr := proxywire.Write(c, proxywire.Frame{Kind: proxywire.Abort, ID: first.ID}, proxywire.Deadline(time.Time{}))
		if errors.Is(err, gateway.ErrForbidden) || errors.Is(err, gateway.ErrRevoked) || errors.Is(err, gateway.ErrClosed) {
			return errors.Join(err, abortErr)
		}
		return abortErr
	}
	defer response.Body.Close()
	data, err := proxywire.Encode(proxywire.ResponseMeta{Status: response.Status, Headers: response.Headers})
	if err != nil {
		return err
	}
	write := func(kind byte, p []byte) error {
		return proxywire.Write(c, proxywire.Frame{Kind: kind, ID: first.ID, Payload: p}, proxywire.Deadline(time.Time{}))
	}
	if err = write(proxywire.Response, data); err != nil {
		return err
	}
	buffer := make([]byte, proxywire.Limit)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if err = write(proxywire.Data, buffer[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return write(proxywire.End, nil)
		}
		if readErr != nil {
			abortErr := write(proxywire.Abort, nil)
			if errors.Is(readErr, gateway.ErrForbidden) || errors.Is(readErr, gateway.ErrRevoked) || errors.Is(readErr, gateway.ErrClosed) {
				return errors.Join(readErr, abortErr)
			}
			return abortErr
		}
	}
}

// runError 在调用方主动取消时剔除传输终止错误，保留同一组合错误中的真实故障。
func runError(ctx context.Context, err error) error {
	if ctx.Err() != context.Canceled || err == nil {
		return err
	}
	return withoutRunErrors(err, context.Canceled, net.ErrClosed, io.ErrClosedPipe, io.EOF)
}

// withoutRunErrors 逐项剔除明确由生命周期收尾产生的错误，不整包忽略组合中的其他真实原因。
func withoutRunErrors(err error, ignored ...error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface {
		// Unwrap 返回组合错误的各个独立分支。
		Unwrap() []error
	}); ok {
		var remaining []error
		for _, cause := range joined.Unwrap() {
			if failure := withoutRunErrors(cause, ignored...); failure != nil {
				remaining = append(remaining, failure)
			}
		}
		return errors.Join(remaining...)
	}
	if wrapped, ok := err.(interface {
		// Unwrap 返回带上下文错误的实际原因。
		Unwrap() error
	}); ok {
		return withoutRunErrors(wrapped.Unwrap(), ignored...)
	}
	for _, cause := range ignored {
		if err == cause {
			return nil
		}
	}
	return err
}
