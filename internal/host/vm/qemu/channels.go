package qemu

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"safeagent/internal/host/vm"
)

// channelRun 保存当前驱动实际启动的私有通道及已交付句柄，不代替 Manager 生命周期状态。
type channelRun struct {
	// mu 串行化连接交付与 VMM 退出失效。
	mu sync.Mutex
	// dir 是短私有端点目录。
	dir string
	// closed 在 VMM 退出后阻止交付。
	closed bool
	// controlOpened 禁止未验证的同 boot 控制重连。
	controlOpened bool
	// ctx 在 VMM 退出时取消所有端点连接等待。
	ctx context.Context
	// cancel 结束端点生命周期。
	cancel context.CancelFunc
	// connections 在 VMM 退出时统一关闭。
	connections map[*trackedConn]bool
}

// Channels 是绑定单次 VMInstance 的可信私有端点装配结果。
type Channels struct {
	// Instance 是 Host 已确认的 VM 启动身份。
	Instance vm.VMInstance
	// Control 是本 boot 唯一交付的控制连接。
	Control net.Conn
	// run 绑定同一驱动实际启动记录。
	run *channelRun
}

// channelDir 将不受路径长度约束的 epoch 映射为短内部目录，不使用业务路径。
func channelDir(dir, epoch string) string {
	hash := sha256.Sum256([]byte(dir + "\x00" + epoch))
	return filepath.Join("/tmp", fmt.Sprintf("safeagent-qemu-%x", hash[:16]))
}

// connectSocket 等待 QEMU 发布本轮 socket，取消后不会交付句柄。
func connectSocket(ctx context.Context, path string) (net.Conn, error) {
	for {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

// OpenGuestChannels 从实际启动记录交付通道，不接受磁盘 socket 路径冒充运行实例。
func (d *Driver) OpenGuestChannels(ctx context.Context, instance vm.VMInstance) (*Channels, error) {
	d.channelsMu.Lock()
	run := d.channels[instance]
	d.channelsMu.Unlock()
	if run == nil {
		return nil, vm.ErrStaleInstance
	}
	run.mu.Lock()
	if run.closed {
		run.mu.Unlock()
		return nil, vm.ErrStaleInstance
	}
	if run.controlOpened {
		run.mu.Unlock()
		return nil, fmt.Errorf("control reconnect requires a new VM epoch")
	}
	run.controlOpened = true
	run.mu.Unlock()
	conn, err := run.open(ctx, "control.sock")
	if err != nil {
		return nil, err
	}
	return &Channels{Instance: instance, Control: conn, run: run}, nil
}

// OpenTerminal 打开同一运行的独立终端端口，旧授权由 RPC Detach 完成释放。
func (c *Channels) OpenTerminal(ctx context.Context) (net.Conn, error) {
	return c.run.open(ctx, "terminal.sock")
}

// open 锁外等待端点，运行退出取消等待；重新核验身份后才交付连接。
func (r *channelRun) open(ctx context.Context, name string) (net.Conn, error) {
	waiting, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	conn, err := connectSocket(waiting, filepath.Join(r.dir, name))
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || waiting.Err() != nil {
		conn.Close()
		return nil, vm.ErrStaleInstance
	}
	tracked := &trackedConn{Conn: conn, run: r}
	r.connections[tracked] = true
	return tracked, nil
}

// close 失效已交付句柄并删除本轮自有端点，调用方已经观察到 QEMU 退出。
func (r *channelRun) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.cancel()
	for conn := range r.connections {
		conn.Conn.Close()
	}
	r.connections = nil
	os.RemoveAll(r.dir)
}

// trackedConn 在业务关闭时从当前运行记录移除句柄，避免多次重绑积累已关闭连接。
type trackedConn struct {
	// Conn 是实际 Unix socket。
	net.Conn
	// run 是句柄所属启动代次。
	run *channelRun
}

// Close 释放实际句柄并移除跟踪，运行整体关闭直接操作底层连接避免锁重入。
func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.run.mu.Lock()
	delete(c.run.connections, c)
	c.run.mu.Unlock()
	return err
}
