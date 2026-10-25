package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// device 为实验提供可取消的非阻塞 virtio 设备读写。
type device struct {
	// file 是只由当前设备循环持有的文件。
	file *os.File
	// ctx 在逻辑连接失效时终止本轮读写。
	ctx context.Context
}

// openDevice 打开本轮连接的设备，不向测试进程继承句柄。
func openDevice(ctx context.Context, path string) (*device, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &device{file: f, ctx: ctx}, nil
}

// pause 等待明确的设备探测间隔，同时响应取消。
func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Millisecond):
		return nil
	}
}

// Read 读取设备，离线 EOF 和逻辑取消分别返回。
func (d *device) Read(data []byte) (int, error) {
	for {
		if err := d.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := syscall.Read(int(d.file.Fd()), data)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
			if err := pause(d.ctx); err != nil {
				return 0, err
			}
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		// 仅记录读取长度，供半帧实验确认字节已到 Guest，不记录消息内容。
		if d.file.Name() == "/dev/safeagent.control" {
			fmt.Fprintln(os.Stderr, "CONTROL_READ_BYTES:", n)
		}
		return n, nil
	}
}

// Write 写入设备，背压等待可由逻辑连接取消打断。
func (d *device) Write(data []byte) (int, error) {
	for {
		if err := d.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := syscall.Write(int(d.file.Fd()), data)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
			if err := pause(d.ctx); err != nil {
				return 0, err
			}
			continue
		}
		return n, err
	}
}
