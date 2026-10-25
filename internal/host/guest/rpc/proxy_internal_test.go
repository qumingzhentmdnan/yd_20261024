package rpc

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"safeagent/internal/guestwire"
	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
)

// TestProxyIdentityRequiresHello 验证未Hello不交付身份，失效后旧身份不可取得。
func TestProxyIdentityRequiresHello(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	c, err := New(vm.VMInstance{Ref: vm.VMRef{ID: "proxy"}, Epoch: "epoch"}, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err = c.Identity(); err == nil {
		t.Fatal("未Hello交付身份")
	}
	c.mu.Lock()
	c.hello = true
	c.mu.Unlock()
	if _, id, err := c.Identity(); err != nil || id == "" {
		t.Fatal(id, err)
	}
	c.Close()
	if _, _, err = c.Identity(); err == nil {
		t.Fatal("失效后交付身份")
	}
}

// TestProbeFailureStages 验证资格、写等待、响应等待和账本耗尽均失效控制连接。
func TestProbeFailureStages(t *testing.T) {
	for _, stage := range []string{"slots", "write", "response", "capacity"} {
		t.Run(stage, func(t *testing.T) {
			left, right := net.Pipe()
			defer right.Close()
			c, err := New(vm.VMInstance{Ref: vm.VMRef{ID: "proxy"}, Epoch: "epoch"}, left, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			switch stage {
			case "slots":
				for i := 0; i < protocol.MaxPending; i++ {
					c.slots <- struct{}{}
				}
			case "write":
				c.write <- struct{}{}
			case "response":
				go func() { guestwire.Read(right, protocol.Control, protocol.HostToGuest) }()
			case "capacity":
				c.reading = protocol.MaxPending
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if err = c.Probe(ctx); err == nil {
				t.Fatal("Probe应失败")
			}
			select {
			case <-c.Done():
			default:
				t.Fatal("Probe失败未失效连接")
			}
			failure := c.Failure()
			if stage == "capacity" {
				var business *protocol.Error
				if !errors.As(failure, &business) || business.Code != protocol.CodeResourceExhausted {
					t.Fatal("本地Probe容量失败未保存", failure)
				}
			} else {
				var timeout net.Error
				if !errors.Is(failure, context.DeadlineExceeded) && (!errors.As(failure, &timeout) || !timeout.Timeout()) {
					t.Fatal("Probe阶段deadline未保存", stage, failure)
				}
			}
		})
	}
}
