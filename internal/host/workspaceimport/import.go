// Package workspaceimport 将Host基线发送到同一可信VM实例，不拥有VM回收。
package workspaceimport

import (
	"context"
	"errors"
	"io"

	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/host/workspace"
	"safeagent/internal/protocol"
	"safeagent/internal/workspacebundle"
	"safeagent/internal/workspacewire"
)

// Import 校验Guest限额后传输一次基线，失败由装配者回收VM，不能同boot重试。
func Import(ctx context.Context, client *rpc.Client, channels *qemu.Channels, store *workspace.Store, id workspace.BaselineID) (record workspacewire.Imported, result error) {
	life, cancel := context.WithTimeout(ctx, workspacewire.Total)
	defer cancel()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-client.Done():
			cancel()
		case <-life.Done():
		}
	}()
	defer func() { cancel(); <-watched }()
	instance, connection, err := client.Identity()
	if err != nil {
		return record, err
	}
	if instance != channels.Instance {
		return record, protocol.Failure(protocol.CodeStaleInstance)
	}
	state, err := client.GetState(life)
	if err != nil {
		return record, err
	}
	if state.Prepared != nil || len(state.Sessions) != 0 {
		return record, protocol.Failure(protocol.CodeFailedPrecondition)
	}
	baseline, _, source, err := store.OpenImportForGuest(life, id, workspacebundle.GuestLimits())
	if err != nil {
		return record, err
	}
	defer func() { result = errors.Join(result, source.Close()) }()
	port, err := channels.OpenWorkspace(life)
	if err != nil {
		return record, err
	}
	defer port.Close()
	closed := make(chan struct{})
	stop := context.AfterFunc(life, func() { defer close(closed); port.Close() })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	total, _ := life.Deadline()
	begin := workspacewire.BeginMeta{VMRef: protocol.VMRef(instance.Ref.ID), Epoch: protocol.Epoch(instance.Epoch), ConnectionID: protocol.UUID(connection), WorkspaceRef: protocol.OpaqueID(baseline.ID), WorkspaceDigest: protocol.Digest(baseline.Digest), ArchiveBytes: baseline.ArchiveBytes}
	payload, err := workspacewire.Encode(begin)
	if err != nil {
		return record, err
	}
	if err = workspacewire.Write(port, workspacewire.Frame{Kind: workspacewire.Begin, Payload: payload}, workspacewire.Deadline(total, workspacewire.Budget)); err != nil {
		return record, err
	}
	frame, err := workspacewire.Read(port, workspacewire.Deadline(total, workspacewire.Budget))
	if err != nil {
		return record, err
	}
	if err = expect(frame, workspacewire.Ready); err != nil {
		return record, err
	}
	buffer := make([]byte, workspacewire.Limit)
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if err = workspacewire.Write(port, workspacewire.Frame{Kind: workspacewire.Data, Payload: buffer[:n]}, workspacewire.Deadline(total, workspacewire.Budget)); err != nil {
				return record, err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return record, readErr
		}
	}
	if err = workspacewire.Write(port, workspacewire.Frame{Kind: workspacewire.End}, workspacewire.Deadline(total, workspacewire.Budget)); err != nil {
		return record, err
	}
	frame, err = workspacewire.Read(port, workspacewire.Deadline(total, workspacewire.CommitBudget))
	if err != nil {
		return record, err
	}
	if err = expect(frame, workspacewire.Committed); err != nil {
		return record, err
	}
	if err = workspacewire.Decode(frame.Payload, &record); err != nil {
		return record, err
	}
	if record.WorkspaceRef != begin.WorkspaceRef || record.WorkspaceDigest != begin.WorkspaceDigest || record.FileCount != baseline.FileCount || record.TotalBytes != baseline.TotalBytes {
		return workspacewire.Imported{}, workspacewire.ErrFrame
	}
	if err = life.Err(); err != nil {
		return workspacewire.Imported{}, err
	}
	return record, nil
}

// expect 只接受当前阶段的帧，拒绝码保持结构化而不暴露内部文本。
func expect(frame workspacewire.Frame, kind byte) error {
	if frame.Kind == kind {
		return nil
	}
	if frame.Kind == workspacewire.Rejected {
		var failure workspacewire.Failure
		if err := workspacewire.Decode(frame.Payload, &failure); err != nil {
			return err
		}
		return &RejectedError{Code: failure.Code}
	}
	return workspacewire.ErrFrame
}

// RejectedError 保存Guest返回的固定拒绝码。
type RejectedError struct {
	// Code 是INVALID_STATE、INVALID_BUNDLE、LIMIT或IO。
	Code string
}

// Error 返回不含内部路径的固定拒绝说明。
func (e *RejectedError) Error() string { return "Guest工作区导入被拒绝: " + e.Code }
