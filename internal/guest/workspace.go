package guest

import (
	"context"
	"errors"
	"net"
	"time"

	guestworkspace "safeagent/internal/guest/workspace"
	"safeagent/internal/protocol"
	"safeagent/internal/workspacebundle"
	"safeagent/internal/workspacewire"
)

// importState 表示每boot唯一导入的状态，零值尚未接纳。
type importState uint8

const (
	importEmpty importState = iota
	importReceiving
	importCommitted
	importFailed
)

// reserveImport 按控制连接既有锁顺序核验身份并预留唯一导入资格。
func (b *Boot) reserveImport(p workspacewire.BeginMeta) (*connection, error) {
	b.mu.Lock()
	c := b.connection
	b.mu.Unlock()
	if c == nil {
		return nil, protocol.Failure(protocol.CodeFailedPrecondition)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.config.WorkspaceRoot == nil || b.connection != c || !c.hello || c.ctx.Err() != nil || p.VMRef != b.ref || p.Epoch != b.epoch || p.ConnectionID != c.id || b.importState != importEmpty || b.reserved || b.prepared != nil || len(b.sessions) != 0 {
		return nil, protocol.Failure(protocol.CodeFailedPrecondition)
	}
	b.importState = importReceiving
	return c, nil
}

// Imported 返回当前boot真实成功记录的独立快照，不向调用方暴露路径。
func (b *Boot) Imported() (workspacewire.Imported, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.imported, b.imported.WorkspaceRef != ""
}

// ServeWorkspace 接收本boot唯一workspace连接，失败只返回给Guest入口处理控制失效。
func (b *Boot) ServeWorkspace(ctx context.Context, port net.Conn) (result error) {
	defer port.Close()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(closed); port.Close() })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	frame, err := workspacewire.Read(port, time.Time{})
	if err != nil {
		return err
	}
	life, cancel := context.WithTimeout(ctx, workspacewire.Total)
	defer cancel()
	var begin workspacewire.BeginMeta
	if frame.Kind != workspacewire.Begin {
		return workspacewire.ErrFrame
	}
	if err = workspacewire.Decode(frame.Payload, &begin); err != nil {
		return err
	}
	// 拒绝只暴露固定错误码；实际I/O错误仍返回入口日志。
	defer func() {
		if result == nil {
			return
		}
		b.mu.Lock()
		b.importState = importFailed
		b.mu.Unlock()
		code := "IO"
		if errors.Is(result, workspacebundle.ErrLimitExceeded) {
			code = "LIMIT"
		} else if errors.Is(result, workspacebundle.ErrCorruptBaseline) || errors.Is(result, workspacebundle.ErrInvalidSelection) || errors.Is(result, workspacewire.ErrFrame) {
			code = "INVALID_BUNDLE"
		} else {
			var e *protocol.Error
			if errors.As(result, &e) {
				code = "INVALID_STATE"
			}
		}
		data, _ := workspacewire.Encode(workspacewire.Failure{Code: code})
		total, _ := life.Deadline()
		result = errors.Join(result, workspacewire.Write(port, workspacewire.Frame{Kind: workspacewire.Rejected, Payload: data}, workspacewire.Deadline(total, workspacewire.Budget)))
	}()
	control, err := b.reserveImport(begin)
	if err != nil {
		return err
	}
	disconnected := make(chan struct{})
	stopControl := context.AfterFunc(control.ctx, func() { defer close(disconnected); cancel(); port.Close() })
	defer func() {
		if !stopControl() {
			<-disconnected
		}
	}()
	lifeClosed := make(chan struct{})
	stopLife := context.AfterFunc(life, func() { defer close(lifeClosed); port.Close() })
	defer func() {
		if !stopLife() {
			<-lifeClosed
		}
	}()
	return guestworkspace.Receive(life, port, b.config.WorkspaceRoot, begin, func(record workspacewire.Imported, publish func() error) error {
		return b.commitImport(life, control, record, publish)
	})
}

// commitImport 在同一锁内完成最后身份复核、文件发布与记录登记，发布后取消不回滚。
func (b *Boot) commitImport(ctx context.Context, control *connection, record workspacewire.Imported, publish func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.connection != control || control.ctx.Err() != nil {
		return protocol.Failure(protocol.CodeFailedPrecondition)
	}
	if err := publish(); err != nil {
		return err
	}
	b.imported = record
	b.importState = importCommitted
	return nil
}
