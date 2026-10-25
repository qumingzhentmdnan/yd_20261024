package console

import (
	"bytes"
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
	"safeagent/internal/testkit/fakeguest"
)

// terminalFixture 创建已经准备并启动的独立测试Guest，不调用真实模型服务。
func terminalFixture(t *testing.T) (*fakeguest.Boot, *fakeguest.Client, protocol.SessionInfo) {
	t.Helper()
	digest := protocol.Digest("sha256:" + strings.Repeat("a", 64))
	content := protocol.Preparation{PreparedID: "prepared-1", ManifestRef: "fixed-agent", ManifestDigest: digest, ConfigDigest: digest, WorkspaceRef: "workspace-1", WorkspaceDigest: digest, PlanDigest: &digest}
	instance := vm.VMInstance{Ref: vm.VMRef{ID: "console-vm"}, Epoch: "boot-1"}
	boot, err := fakeguest.New(fakeguest.Config{Instance: instance, GuestBuild: "test", ImageDigest: digest, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	client, err := boot.Connect(instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if _, err = client.Hello(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = client.PrepareAgent(t.Context(), protocol.PrepareAgentParams{OperationID: "11111111-1111-4111-8111-111111111111", ManifestRef: content.ManifestRef, ManifestDigest: digest, ConfigDigest: digest, WorkspaceRef: content.WorkspaceRef, WorkspaceDigest: digest}); err != nil {
		t.Fatal(err)
	}
	info, err := client.StartSession(t.Context(), protocol.StartSessionParams{OperationID: "22222222-2222-4222-8222-222222222222", SessionID: "session-1", PreparedID: content.PreparedID, WorkspaceRef: content.WorkspaceRef, WorkspaceDigest: digest, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	return boot, client, info
}

// TestInteractOutputAndExit 验证二进制尾部输出保留且真实非零退出向调用方传播。
func TestInteractOutputAndExit(t *testing.T) {
	boot, client, info := terminalFixture(t)
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	data := []byte{'a', 0, 0xff, '\n'}
	if _, err = boot.FeedOutput(t.Context(), info.SessionID, data); err != nil {
		t.Fatal(err)
	}
	if err = boot.EndOutput(info.SessionID); err != nil {
		t.Fatal(err)
	}
	if err = boot.Exit(protocol.SessionExit{SessionID: info.SessionID, Code: 7, Reason: protocol.ExitReasonProcessExited}, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err = Interact(ctx, client, info, input, output)
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 7 {
		t.Fatalf("退出结果: %v", err)
	}
	if _, err = output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("输出改变: %x", got)
	}
}

// TestWriteFDCancellation 验证下游不读时非阻塞输出仍能取消。
func TestWriteFDCancellation(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	// 与Interact相同，输出FD必须独占并设置非阻塞。
	fd := int(writer.Fd())
	if err = unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	capacity, err := unix.FcntlInt(uintptr(fd), unix.F_GETPIPE_SZ, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- writeFD(ctx, fd, make([]byte, capacity+1)) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("下游背压未解除")
	}
}

// TestInteractCancellation 验证实际转发开始后取消会回收两个等待中的任务。
func TestInteractCancellation(t *testing.T) {
	boot, client, info := terminalFixture(t)
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer output.Close()
	if _, err = boot.FeedOutput(t.Context(), info.SessionID, []byte("x")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Interact(ctx, client, info, input, output) }()
	// 收到真实转发字节后才取消，不能用Attach之前的取消冒充任务回收。
	if err = reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err = io.ReadFull(reader, one[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("终端转发未取消")
	}
}
