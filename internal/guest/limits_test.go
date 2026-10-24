package guest

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/protocol"
)

// TestControlQueueBudget 验证三十二项 FIFO 容量及独立入队预算，饱和失效连接。
func TestControlQueueBudget(t *testing.T) {
	b := testBoot(t)
	left, right := net.Pipe()
	defer right.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c := newConnection(b, left, listener)
	for range 32 {
		if err = c.enqueue(outgoing{}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err = c.enqueue(outgoing{}); err == nil {
		t.Fatal("队列饱和未失败")
	}
	if time.Since(start) < guestwire.Budget {
		t.Fatal("入队预算提前结束")
	}
	if c.ctx.Err() == nil {
		t.Fatal("队列超时未失效连接")
	}
}

// TestSessionCapacity 验证历史结果占用六十四个槽位且不会因组空而淘汰。
func TestSessionCapacity(t *testing.T) {
	b := testBoot(t)
	prepared := b.config.Content
	b.prepared = &prepared
	b.preparations[prepared.PreparedID] = prepared
	for i := 0; i < protocol.MaxSessions; i++ {
		id := protocol.OpaqueID(string(rune('A' + i)))
		b.sessions[id] = &session{info: protocol.SessionInfo{GroupState: protocol.GroupStateEmpty}}
	}
	id, _ := protocol.NewUUID()
	input := protocol.StartSessionParams{OperationID: id, SessionID: "new", PreparedID: prepared.PreparedID, WorkspaceRef: prepared.WorkspaceRef, WorkspaceDigest: prepared.WorkspaceDigest}
	b.mu.Lock()
	err := b.checkNewLocked(input)
	b.mu.Unlock()
	business, ok := err.(*protocol.Error)
	if !ok || business.Code != protocol.CodeResourceExhausted {
		t.Fatal(err)
	}
}

// TestAcceptedFailureReplay 验证接纳后失败固定，而不同参数优先返回冲突。
func TestAcceptedFailureReplay(t *testing.T) {
	b := testBoot(t)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c := newConnection(b, left, listener)
	id, _ := protocol.NewUUID()
	p := protocol.StartSessionParams{OperationID: id, SessionID: "session"}
	failure := protocol.Failure(protocol.CodeCancelled)
	done := make(chan struct{})
	close(done)
	b.operations[id] = &operation{method: protocol.MethodStartSession, input: p, err: failure, done: done}
	_, err = c.operate(context.Background(), protocol.MethodStartSession, p)
	var replay *protocol.Error
	if !errors.As(err, &replay) || *replay != *failure {
		t.Fatal(err)
	}
	p.PreparedID = "different"
	_, err = c.operate(context.Background(), protocol.MethodStartSession, p)
	if e, ok := err.(*protocol.Error); !ok || e.Code != protocol.CodeConflict {
		t.Fatal(err)
	}
}
