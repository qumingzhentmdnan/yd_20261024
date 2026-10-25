package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestReadConfirmationCancellation 验证不完整输入不会阻塞取消及后续VM回收。
func TestReadConfirmationCancellation(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	if _, err = write.Write([]byte("EXPORT partial")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err = readConfirmation(ctx, read); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("取消未及时返回")
	}
}

// TestReadConfirmationLine 验证仅消费本次确认行，不吞入后续输入。
func TestReadConfirmationLine(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	if _, err = write.Write([]byte("EXPORT review-id\nnext\n")); err != nil {
		t.Fatal(err)
	}
	line, err := readConfirmation(context.Background(), read)
	if err != nil || line != "EXPORT review-id" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	line, err = readConfirmation(context.Background(), read)
	if err != nil || line != "next" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}
