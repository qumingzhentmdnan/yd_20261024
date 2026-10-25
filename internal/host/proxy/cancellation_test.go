package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"safeagent/internal/host/gateway"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
	"testing"
	"time"
)

// TestRunCancellationResult 验证正常取消可成功返回，真实故障与cleanup错误始终保留。
func TestRunCancellationResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failure := errors.New("cleanup failed")
	for _, transport := range []error{context.Canceled, net.ErrClosed, io.EOF, io.ErrClosedPipe, fmt.Errorf("read: %w", net.ErrClosed), errors.Join(context.Canceled, net.ErrClosed)} {
		if err := runError(ctx, transport); err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(runError(ctx, transport), failure); !errors.Is(err, failure) {
			t.Fatal("清理失败被丢弃", err)
		}
	}
	for _, input := range []error{failure, errors.Join(io.ErrClosedPipe, failure), fmt.Errorf("forward: %w", errors.Join(context.Canceled, failure))} {
		if err := runError(ctx, input); !errors.Is(err, failure) {
			t.Fatal("真实错误被丢弃", err)
		}
	}
	if err := runError(ctx, context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := runError(context.Background(), net.ErrClosed); !errors.Is(err, net.ErrClosed) {
		t.Fatal("非主动取消被吞", err)
	}
	deadline, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	if err := runError(deadline, context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

// TestRunCombinedControlErrors 验证带取消包装的协议、业务及deadline组合不会被整包忽略。
func TestRunCombinedControlErrors(t *testing.T) {
	failure := errors.Join(guest.ErrDisconnected, context.Canceled, protocol.ErrFrame, protocol.Failure(protocol.CodeResourceExhausted), context.DeadlineExceeded)
	err := withoutRunErrors(failure, context.Canceled, guest.ErrDisconnected)
	var business *protocol.Error
	if !errors.Is(err, protocol.ErrFrame) || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &business) || business.Code != protocol.CodeResourceExhausted {
		t.Fatal("取消组合中的真实原因丢失", err)
	}
}

// TestRunRetainsCleanupFailure 验证Run真实退出路径在主动取消后仍报告清理失败并使用独立预算。
func TestRunRetainsCleanupFailure(t *testing.T) {
	for _, failure := range []error{errors.New("destroy failed"), context.Canceled, net.ErrClosed} {
		left, right := net.Pipe()
		c, err := rpc.New(vm.VMInstance{Ref: vm.VMRef{ID: "cleanup"}, Epoch: "epoch"}, left, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cleaned := false
		err = Run(ctx, c, nil, nil, gateway.BindingSpec{}, func() { t.Error("未握手不能Ready") }, func(clean context.Context) error {
			cleaned = true
			if clean.Err() != nil {
				t.Error("清理继承了已取消context")
			}
			deadline, ok := clean.Deadline()
			if !ok || time.Until(deadline) > 10*time.Second {
				t.Error("清理缺少有界独立预算")
			}
			return failure
		})
		right.Close()
		if !cleaned || !errors.Is(err, failure) || !errors.Is(err, guest.ErrDisconnected) {
			t.Fatal("清理或握手失败被吞", err)
		}
	}
}
