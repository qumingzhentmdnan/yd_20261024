//go:build linux

package rpc_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	server "safeagent/internal/guest"
	"safeagent/internal/guest/runner"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
	"safeagent/internal/testkit/guestcontract"
)

// newClient 为每个场景启动独立真实 Handler、Unix 终端监听器及持续运行的 PTY 工作负载。
func newClient(t *testing.T) (guest.Client, protocol.PrepareAgentParams, protocol.StartSessionParams) {
	return workloadClient(t, []byte("#!/bin/sh\ntrap 'exit 0' TERM\nprintf 'READY\\n'\nwhile :; do sleep 1; done\n"))
}

// workloadClient 为故障场景登记显式固定工作负载，业务参数不能更换命令。
func workloadClient(t *testing.T, workload []byte) (guest.Client, protocol.PrepareAgentParams, protocol.StartSessionParams) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workload")
	if err := os.WriteFile(path, workload, 0755); err != nil {
		t.Fatal(err)
	}
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if uid == 0 {
		uid = 1000
		gid = 1000
	}
	content := protocol.Preparation{PreparedID: "registered", ManifestRef: "fixed", ManifestDigest: digest(workload), ConfigDigest: digest([]byte("fixed-config")), WorkspaceRef: "fixed-workspace", WorkspaceDigest: digest([]byte("fixed-workspace"))}
	boot, err := server.New(server.Config{Content: content, Build: "test", ImageDigest: digest(workload), Runner: runner.Config{Path: path, Dir: dir, UID: uid, GID: gid}})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "terminal.sock"))
	if err != nil {
		t.Fatal(err)
	}
	host, remote := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- boot.Serve(ctx, remote, listener) }()
	client, err := rpc.New(vm.VMInstance{Ref: vm.VMRef{ID: "test-vm"}, Epoch: "test-epoch"}, host, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		cancel()
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := boot.Close(cleanup); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-cleanup.Done():
			t.Error("Handler 未退出")
		}
	})
	op, err := protocol.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	startID, err := protocol.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	return client, protocol.PrepareAgentParams{OperationID: op, ManifestRef: content.ManifestRef, ManifestDigest: content.ManifestDigest, ConfigDigest: content.ConfigDigest, WorkspaceRef: content.WorkspaceRef, WorkspaceDigest: content.WorkspaceDigest}, protocol.StartSessionParams{OperationID: startID, SessionID: "session", WorkspaceRef: content.WorkspaceRef, WorkspaceDigest: content.WorkspaceDigest, Rows: 24, Cols: 80}
}

// TestContract 让真实 RPC/Handler/PTY 执行与 Fake 相同的外部行为契约。
func TestContract(t *testing.T) { guestcontract.Run(t, newClient) }
