package qemu

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"safeagent/internal/host/vm"
)

// TestChannelWaitCancelledByExit 验证 socket 未发布时退出无需等待端点打开者释放锁。
func TestChannelWaitCancelledByExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	instance := vm.VMInstance{Ref: vm.VMRef{ID: "test"}, Epoch: "epoch"}
	run := &channelRun{dir: t.TempDir(), ctx: ctx, cancel: cancel, connections: make(map[*trackedConn]bool)}
	driver := &Driver{channels: map[vm.VMInstance]*channelRun{instance: run}}
	done := make(chan error, 1)
	go func() { _, err := driver.OpenGuestChannels(context.Background(), instance); done <- err }()
	run.close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("退出后不应交付端点")
		}
	case <-time.After(time.Second):
		t.Fatal("实际退出被端点等待阻塞")
	}
}

// TestTerminalWaitCancelledByExit 验证终端重绑等待也随运行退出取消。
func TestTerminalWaitCancelledByExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	run := &channelRun{dir: t.TempDir(), ctx: ctx, cancel: cancel, connections: make(map[*trackedConn]bool)}
	channels := &Channels{run: run}
	done := make(chan error, 1)
	go func() { _, err := channels.OpenTerminal(context.Background()); done <- err }()
	run.close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("退出后不应交付终端")
		}
	case <-time.After(time.Second):
		t.Fatal("退出未取消终端等待")
	}
}

// TestTrackedConnectionRelease 验证反复关闭不会把历史句柄保留到 VM 退出。
func TestTrackedConnectionRelease(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	run := &channelRun{connections: make(map[*trackedConn]bool)}
	conn := &trackedConn{Conn: left, run: run}
	run.connections[conn] = true
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if len(run.connections) != 0 {
		t.Fatal("关闭后仍保留句柄")
	}
}

// TestProxyDeliveryAndInvalidation 验证proxy只交付一次、实例退出关闭物理句柄且不再连接。
func TestProxyDeliveryAndInvalidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	listener, err := net.Listen("unix", filepath.Join(dir, "proxy.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	run := &channelRun{dir: dir, ctx: ctx, cancel: cancel, connections: make(map[*trackedConn]bool)}
	channels := &Channels{run: run}
	port, err := channels.OpenProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	peer := <-accepted
	defer peer.Close()
	if _, err = channels.OpenProxy(context.Background()); err == nil {
		t.Fatal("允许同boot重连")
	}
	run.close()
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatal("VM退出未交付EOF", err)
	}
	if len(run.connections) != 0 {
		t.Fatal("残留已交付句柄")
	}
}
