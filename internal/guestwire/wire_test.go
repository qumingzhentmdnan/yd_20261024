package guestwire

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"safeagent/internal/protocol"
)

// TestIdleAndPartialFrameBudget 验证空闲不计帧预算，首字节后半帧超时。
func TestIdleAndPartialFrameBudget(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	done := make(chan error, 1)
	go func() { _, err := Read(left, protocol.Control, protocol.HostToGuest); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("空闲提前结束: %v", err)
	case <-time.After(Budget + 50*time.Millisecond):
	}
	if _, err := right.Write([]byte("S")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(Budget + time.Second):
		t.Fatal("半帧未超时")
	}
}
