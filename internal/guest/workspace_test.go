package guest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	hostworkspace "safeagent/internal/host/workspace"
	"safeagent/internal/protocol"
	"safeagent/internal/workspacebundle"
	"safeagent/internal/workspacewire"
)

// importFixture 构造真实包和只用于工作区状态边界的可信控制身份。
func importFixture(t *testing.T) (*Boot, *connection, workspacewire.BeginMeta, []byte) {
	t.Helper()
	b := testBoot(t)
	root, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	b.config.WorkspaceRoot = root
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	id, _ := protocol.NewUUID()
	c := &connection{boot: b, ctx: ctx, cancel: cancel, hello: true, id: id}
	b.connection = c
	b.ref = "vm"
	b.epoch = "epoch"
	source := t.TempDir()
	if err = os.WriteFile(filepath.Join(source, "file"), []byte("actual"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := hostworkspace.NewStore(filepath.Join(t.TempDir(), "store"), workspacebundle.GuestLimits())
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.Stage(ctx, hostworkspace.ImportSelection{Root: source, Paths: []string{"file"}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenImport(ctx, base.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return b, c, workspacewire.BeginMeta{VMRef: b.ref, Epoch: b.epoch, ConnectionID: id, WorkspaceRef: protocol.OpaqueID(base.ID), WorkspaceDigest: protocol.Digest(base.Digest), ArchiveBytes: base.ArchiveBytes}, data
}

// TestWorkspaceImportState 验证真实导入、准备门禁、身份拒绝、失败清理及不可覆盖发布。
func TestWorkspaceImportState(t *testing.T) {
	for _, scenario := range []string{"valid", "stale", "before-hello", "corrupt", "existing", "cancel", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			b, c, begin, data := importFixture(t)
			op, _ := protocol.NewUUID()
			params := protocol.PrepareAgentParams{OperationID: op, ManifestRef: b.config.Content.ManifestRef, ManifestDigest: b.config.Content.ManifestDigest, ConfigDigest: b.config.Content.ConfigDigest, WorkspaceRef: begin.WorkspaceRef, WorkspaceDigest: begin.WorkspaceDigest}
			if _, err := c.operate(context.Background(), protocol.MethodPrepareAgent, params); err == nil {
				t.Fatal("导入前Prepare成功")
			}
			switch scenario {
			case "stale":
				begin.Epoch = "old"
			case "before-hello":
				c.hello = false
			case "corrupt":
				data = bytes.Clone(data)
				data[bytes.Index(data, []byte("actual"))] ^= 1
			case "existing":
				os.Mkdir(filepath.Join(b.config.WorkspaceRoot.Name(), "project"), 0755)
			case "oversize":
				begin.ArchiveBytes = workspacebundle.GuestLimits().MaxArchiveBytes + 1
			}
			host, remote := net.Pipe()
			defer host.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- b.ServeWorkspace(ctx, remote) }()
			payload, err := workspacewire.Encode(begin)
			if err != nil {
				t.Fatal(err)
			}
			if err = workspacewire.Write(host, workspacewire.Frame{Kind: workspacewire.Begin, Payload: payload}, time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			frame, err := workspacewire.Read(host, time.Now().Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "stale" || scenario == "before-hello" || scenario == "oversize" {
				if frame.Kind != workspacewire.Rejected {
					t.Fatal(frame)
				}
				if err = <-done; err == nil {
					t.Fatal("拒绝未返回错误")
				}
				return
			}
			if frame.Kind != workspacewire.Ready {
				t.Fatal(frame)
			}
			if scenario == "cancel" {
				cancel()
				host.Close()
				if err = <-done; err == nil {
					t.Fatal("取消成功")
				}
			} else {
				for len(data) > 0 {
					n := min(len(data), workspacewire.Limit)
					if err = workspacewire.Write(host, workspacewire.Frame{Kind: workspacewire.Data, Payload: data[:n]}, time.Now().Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					data = data[n:]
				}
				if err = workspacewire.Write(host, workspacewire.Frame{Kind: workspacewire.End}, time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				frame, err = workspacewire.Read(host, time.Now().Add(time.Second))
				if err != nil {
					t.Fatal(err)
				}
				err = <-done
			}
			if scenario != "valid" {
				if err == nil {
					t.Fatal("非法导入成功")
				}
				entries, _ := os.ReadDir(b.config.WorkspaceRoot.Name())
				for _, entry := range entries {
					if entry.Name() != "project" {
						t.Fatal("遗留staging", entry.Name())
					}
				}
				if _, ok := b.Imported(); ok {
					t.Fatal("失败产生登记")
				}
				return
			}
			if err != nil || frame.Kind != workspacewire.Committed {
				t.Fatal(frame, err)
			}
			record, ok := b.Imported()
			if !ok || record.WorkspaceRef != begin.WorkspaceRef || record.TotalBytes != 6 {
				t.Fatal(record, ok)
			}
			output, err := os.ReadFile(filepath.Join(b.config.WorkspaceRoot.Name(), "project/file"))
			if err != nil || string(output) != "actual" {
				t.Fatal(string(output), err)
			}
			value, err := c.operate(context.Background(), protocol.MethodPrepareAgent, params)
			if err != nil {
				t.Fatal(err)
			}
			prepared := value.(protocol.Preparation)
			if prepared.WorkspaceRef != begin.WorkspaceRef || prepared.WorkspaceDigest != begin.WorkspaceDigest || b.config.Content.WorkspaceRef == begin.WorkspaceRef {
				t.Fatal("准备未绑定真实记录或改写配置", prepared)
			}
			if _, err = b.reserveImport(begin); err == nil {
				t.Fatal("允许重复导入")
			}
		})
	}
}

// TestImportControlLoss 验证正在等待归档时控制取消解除物理读取并清理私有目录。
func TestImportControlLoss(t *testing.T) {
	b, c, begin, _ := importFixture(t)
	host, remote := net.Pipe()
	defer host.Close()
	done := make(chan error, 1)
	go func() { done <- b.ServeWorkspace(context.Background(), remote) }()
	data, _ := workspacewire.Encode(begin)
	workspacewire.Write(host, workspacewire.Frame{Kind: workspacewire.Begin, Payload: data}, time.Now().Add(time.Second))
	frame, err := workspacewire.Read(host, time.Now().Add(time.Second))
	if err != nil || frame.Kind != workspacewire.Ready {
		t.Fatal(frame, err)
	}
	c.cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("断联成功")
		}
	case <-time.After(time.Second):
		t.Fatal("控制取消未唤醒接收")
	}
	entries, err := os.ReadDir(b.config.WorkspaceRoot.Name())
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	if _, err = os.Stat(filepath.Join(b.config.WorkspaceRoot.Name(), "project")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// TestImportCommitCancellation 使用发布回调闸门验证取消前后不可混淆的提交结果。
func TestImportCommitCancellation(t *testing.T) {
	for _, scenario := range []string{"before", "during-publish", "control-lost", "publish-error"} {
		t.Run(scenario, func(t *testing.T) {
			b, c, begin, _ := importFixture(t)
			if _, err := b.reserveImport(begin); err != nil {
				t.Fatal(err)
			}
			record := workspacewire.Imported{WorkspaceRef: begin.WorkspaceRef, WorkspaceDigest: begin.WorkspaceDigest, FileCount: 1, TotalBytes: 6}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "before" {
				cancel()
			}
			if scenario == "control-lost" {
				c.cancel()
			}
			published := false
			failure := errors.New("发布失败")
			err := b.commitImport(ctx, c, record, func() error {
				if scenario == "publish-error" {
					return failure
				}
				published = true
				cancel()
				return nil
			})
			actual, committed := b.Imported()
			if scenario == "during-publish" {
				if err != nil || !published || !committed || actual != record {
					t.Fatal(actual, committed, err)
				}
				return
			}
			if err == nil || published || committed {
				t.Fatal("未发布却成功登记", published, committed, err)
			}
			if scenario == "publish-error" && !errors.Is(err, failure) {
				t.Fatal(err)
			}
		})
	}
}

// TestCommittedImportAcknowledgementLost 验证发布后回执半帧断开保留真实登记且禁止继续准备。
func TestCommittedImportAcknowledgementLost(t *testing.T) {
	b, _, begin, data := importFixture(t)
	host, remote := net.Pipe()
	defer host.Close()
	done := make(chan error, 1)
	go func() { done <- b.ServeWorkspace(context.Background(), remote) }()
	payload, _ := workspacewire.Encode(begin)
	if err := workspacewire.Write(host, workspacewire.Frame{Kind: workspacewire.Begin, Payload: payload}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	frame, err := workspacewire.Read(host, time.Now().Add(time.Second))
	if err != nil || frame.Kind != workspacewire.Ready {
		t.Fatal(frame, err)
	}
	for len(data) > 0 {
		n := min(len(data), workspacewire.Limit)
		if err = workspacewire.Write(host, workspacewire.Frame{Kind: workspacewire.Data, Payload: data[:n]}, time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		data = data[n:]
	}
	if err = workspacewire.Write(host, workspacewire.Frame{Kind: workspacewire.End}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// 回执首字节只能在真实rename与登记之后出现，读取它作为确定性提交闸门。
	host.SetReadDeadline(time.Now().Add(time.Second))
	var first [1]byte
	if _, err = io.ReadFull(host, first[:]); err != nil {
		t.Fatal(err)
	}
	if first[0] != 'S' {
		t.Fatal(first)
	}
	host.Close()
	if err = <-done; err == nil {
		t.Fatal("丢失回执未返回错误")
	}
	record, ok := b.Imported()
	if !ok || record.WorkspaceRef != begin.WorkspaceRef {
		t.Fatal("发布后登记被回滚", record, ok)
	}
	if b.importState != importFailed {
		t.Fatal("失败后仍允许业务", b.importState)
	}
	value, err := os.ReadFile(filepath.Join(b.config.WorkspaceRoot.Name(), "project/file"))
	if err != nil || string(value) != "actual" {
		t.Fatal(string(value), err)
	}
}
