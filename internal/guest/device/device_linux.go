// Package device 将 Linux virtio 字符设备适配为可取消且带截止时间的通道。
package device

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Conn 拥有一个非阻塞字符设备，不向工作负载继承。
type Conn struct {
	// mu 保护 fd 生命周期及独立截止时间。
	mu sync.Mutex
	// fd 在关闭后为 -1，所有系统调用都在 mu 内执行。
	fd int
	// path 是固定镜像设备名。
	path string
	// readDeadline 零值允许无限空闲等待。
	readDeadline time.Time
	// writeDeadline 零值允许终端独立背压。
	writeDeadline time.Time
	// closed 立即唤醒重试等待。
	closed chan struct{}
}

// Open 打开固定设备，不执行通用路径恢复或自动重连。
func Open(path string) (*Conn, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &Conn{fd: fd, path: path, closed: make(chan struct{})}, nil
}

// transfer 以短非阻塞系统调用实现截止时间，关闭不与复用后的 fd 竞态。
func (c *Conn) transfer(data []byte, write bool) (int, error) {
	for {
		c.mu.Lock()
		if c.fd < 0 {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		deadline := c.readDeadline
		if write {
			deadline = c.writeDeadline
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			c.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		var n int
		var err error
		if write {
			n, err = unix.Write(c.fd, data)
		} else {
			n, err = unix.Read(c.fd, data)
		}
		c.mu.Unlock()
		if err == nil {
			if !write && n == 0 {
				return 0, io.EOF
			}
			return n, nil
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return n, err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-timer.C:
		case <-c.closed:
			timer.Stop()
			return 0, net.ErrClosed
		}
	}
}

// Read 读取字符设备，物理 EOF 不自动打开下一代。
func (c *Conn) Read(p []byte) (int, error) { return c.transfer(p, false) }

// Write 写入字符设备，背压受独立写截止时间约束。
func (c *Conn) Write(p []byte) (int, error) { return c.transfer(p, true) }

// Close 原子失效 fd 并唤醒所有重试等待。
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return nil
	}
	err := unix.Close(c.fd)
	c.fd = -1
	close(c.closed)
	return err
}

// SetDeadline 设置读写截止时间。
func (c *Conn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.writeDeadline = t
	return nil
}

// SetReadDeadline 设置读截止时间。
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	return nil
}

// SetWriteDeadline 设置写截止时间。
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return nil
}

// address 标识本地 virtio 端点，不暴露 Host 私有路径。
type address string

// Network 返回设备传输类型。
func (a address) Network() string { return "virtio-serial" }

// String 返回固定设备名。
func (a address) String() string { return string(a) }

// LocalAddr 返回 Guest 设备标识。
func (c *Conn) LocalAddr() net.Addr { return address(c.path) }

// RemoteAddr 返回 Host 端标识。
func (c *Conn) RemoteAddr() net.Addr { return address("host") }

// Listener 每次只打开一个终端设备，旧 Conn 关闭后才允许下一次 Accept。
type Listener struct {
	// path 是固定终端设备路径。
	path string
	// ctx 结束 listener 生命周期。
	ctx context.Context
	// cancel 中止后续接受。
	cancel context.CancelFunc
}

// Listen 为固定字符设备创建顺序接收器。
func Listen(path string) *Listener {
	ctx, cancel := context.WithCancel(context.Background())
	return &Listener{path: path, ctx: ctx, cancel: cancel}
}

// Accept 等待 Host 在线再交付设备；无 Host 时不把立即 EOF 当作新连接。
func (l *Listener) Accept() (net.Conn, error) {
	c, err := Open(l.path)
	if err != nil {
		return nil, err
	}
	for {
		if err = l.ctx.Err(); err != nil {
			c.Close()
			return nil, net.ErrClosed
		}
		// 交付前 Conn 仅由当前 Accept 持有，无须与外部读写同步。
		fds := []unix.PollFd{{Fd: int32(c.fd), Events: unix.POLLOUT}}
		_, err = unix.Poll(fds, 0)
		if err != nil && !errors.Is(err, unix.EINTR) {
			c.Close()
			return nil, err
		}
		if fds[0].Revents&unix.POLLOUT != 0 {
			return c, nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-timer.C:
		case <-l.ctx.Done():
			timer.Stop()
			c.Close()
			return nil, net.ErrClosed
		}
	}
}

// Close 取消设备接受，不代替当前 Conn 的资源所有者关闭。
func (l *Listener) Close() error { l.cancel(); return nil }

// Addr 返回固定设备标识。
func (l *Listener) Addr() net.Addr { return address(l.path) }
