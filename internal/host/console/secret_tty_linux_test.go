package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// promptSignal 让测试在终端已关闭回显后才交付假密钥或取消。
type promptSignal struct {
	// ready 在首次提示写入时关闭。
	ready chan struct{}
	// once 保证结束换行不会重复关闭通知。
	once sync.Once
}

// Write 记录提示阶段，不保存或输出任何密钥字节。
func (p *promptSignal) Write(data []byte) (int, error) {
	p.once.Do(func() { close(p.ready) })
	return len(data), nil
}

// openTestPTY 为真实termios验证创建独立主从终端，不启动外部进程。
func openTestPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

// TestReadSecretTTY 验证真实终端不会回显密钥，成功与取消均完整恢复原termios。
func TestReadSecretTTY(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			master, slave := openTestPTY(t)
			before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			before.Lflag |= unix.ECHO | unix.ECHONL
			if err = unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, before); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			prompt := &promptSignal{ready: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				secret, err := ReadSecret(ctx, slave, prompt)
				if !canceled && err == nil && secret != "test-only-tty-key" {
					err = errors.New("假密钥字节改变")
				}
				done <- err
			}()
			select {
			case <-prompt.ready:
			case <-ctx.Done():
				t.Fatal("没有进入隐藏输入阶段")
			}
			hidden, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if hidden.Lflag&(unix.ECHO|unix.ECHONL) != 0 {
				t.Fatal("密钥输入仍开启回显")
			}
			if canceled {
				cancel()
			} else {
				if _, err = io.WriteString(master, "test-only-tty-key\n"); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("隐藏输入未退出")
			}
			if canceled {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if *after != *before {
				t.Fatal("终端状态未恢复")
			}
			poll := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
			count, err := unix.Poll(poll, 0)
			if err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("终端出现了密钥回显字节")
			}
		})
	}
}

// TestReadSecretPreservesFollowingInput 验证密钥行后的终端输入不会被预读丢失。
func TestReadSecretPreservesFollowingInput(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = file.WriteString("test-only-key\nnext-input\n"); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadSecret(t.Context(), file, io.Discard); err != nil {
		t.Fatal(err)
	}
	remaining, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(remaining) != "next-input\n" {
		t.Fatal("密钥读取消费了后续输入")
	}
}
