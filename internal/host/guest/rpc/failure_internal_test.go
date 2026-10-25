package rpc

import (
	"context"
	"errors"
	"sync"
	"testing"

	"safeagent/internal/protocol"
)

// TestFailureSnapshotConcurrent 验证失败快照并发可读，Done后固定且不会被后续关闭覆盖。
func TestFailureSnapshotConcurrent(t *testing.T) {
	c, _ := ledgerClient(t)
	if err := c.Failure(); err != nil {
		t.Fatal("健康连接不应有失败原因", err)
	}
	first := errors.Join(context.Canceled, protocol.ErrFrame)
	start := make(chan struct{})
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for range 1000 {
				err := c.Failure()
				if err != nil && (!errors.Is(err, protocol.ErrFrame) || !errors.Is(err, context.Canceled)) {
					t.Error("读取到了部分或不一致的失败原因", err)
				}
			}
		}()
	}
	close(start)
	c.fail(first)
	<-c.Done()
	if err := c.Failure(); err != first {
		t.Fatal("Done后首次失败没有固定", err)
	}
	c.fail(context.DeadlineExceeded)
	c.Close()
	readers.Wait()
	if err := c.Failure(); err != first {
		t.Fatal("首次原因被后续错误或主动关闭覆盖", err)
	}
}
