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
	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/proxywire"
)

// Run 拥有本次绑定和通道，ready须立即通知返回；失效后用独立context回收VM，不关闭共享Gateway。
func Run(ctx context.Context, client *rpc.Client, channels *qemu.Channels, g *gateway.Gateway, spec gateway.BindingSpec, ready func(), cleanup func(context.Context) error) (result error) {
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	// 唯一装配任务串行启动与清理，回调只发布取消，不能重入绑定。
	defer func() {
		client.Close()
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		result = errors.Join(result, cleanup(clean))
	}()
	instance, id, err := client.Identity()
	if err != nil {
		return err
	}
	if instance != channels.Instance {
		return gateway.ErrForbidden
	}
	stopped := make(chan struct{})
	stopControl := context.AfterFunc(controlContext{client.Done()}, func() { defer close(stopped); cancel() })
	defer func() {
		if !stopControl() {
			<-stopped
		}
	}()
	port, err := channels.OpenProxy(life)
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

// serveRequest 从接纳首帧开始限制总时间、帧间空闲及累计容量。
func serveRequest(ctx context.Context, c net.Conn, g *gateway.Gateway, identity gateway.ChannelIdentity, binding gateway.BindingID, first proxywire.Frame) error {
	total := time.Now().Add(proxywire.Total)
	requestCtx, cancel := context.WithDeadline(ctx, total)
	defer cancel()
	var meta proxywire.RequestMeta
	if err := proxywire.Decode(first.Payload, &meta); err != nil {
		return err
	}
	var body bytes.Buffer
	for {
		f, err := proxywire.Read(c, proxywire.Deadline(total))
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
		return errors.Join(err, proxywire.Write(c, proxywire.Frame{Kind: proxywire.Abort, ID: first.ID}, proxywire.Deadline(total)))
	}
	defer response.Body.Close()
	data, err := proxywire.Encode(proxywire.ResponseMeta{Status: response.Status, Headers: response.Headers})
	if err != nil {
		return err
	}
	write := func(kind byte, p []byte) error {
		return proxywire.Write(c, proxywire.Frame{Kind: kind, ID: first.ID, Payload: p}, proxywire.Deadline(total))
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
			return errors.Join(readErr, write(proxywire.Abort, nil))
		}
	}
}
