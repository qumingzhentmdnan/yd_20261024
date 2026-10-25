//go:build linux

package rpc_test

import (
	"context"
	"testing"
	"time"

	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/protocol"
)

// TestLongSessionProbeAndOperationReplay 验证真实Host/Guest超过旧容量仍探测，且变更operation结果保持幂等。
func TestLongSessionProbeAndOperationReplay(t *testing.T) {
	c, p, s := newClient(t)
	s = readyClient(t, c, p, s)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started, err := c.StartSession(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	client := c.(*rpc.Client)
	for i := 0; i < protocol.MaxRequests+32; i++ {
		if err = client.Probe(ctx); err != nil {
			t.Fatalf("第%d次Probe: %v", i, err)
		}
	}
	again, err := c.StartSession(ctx, s)
	if err != nil || again != started {
		t.Fatal("长会话后Start重放不一致", again, started, err)
	}
	prepared, err := c.PrepareAgent(ctx, p)
	if err != nil || prepared.PreparedID != s.PreparedID {
		t.Fatal("长会话后Prepare重放不一致", prepared, err)
	}
	if err = c.StopSession(ctx, protocol.StopSessionParams{SessionID: s.SessionID, GraceMs: 100, ForceAfterTimeout: true}); err != nil {
		t.Fatal(err)
	}
}
