package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
	"safeagent/internal/host/guest"
	"safeagent/internal/protocol"
)

// ExitError 保存真实Guest主进程的非零退出结果。
type ExitError struct {
	// Code 是退出码；信号退出时为-1。
	Code int
	// Signal 是导致Guest退出的信号名称，正常退出为空。
	Signal string
}

// Error 返回不含模型输入或凭证的退出诊断。
func (e *ExitError) Error() string {
	return fmt.Sprintf("Agent退出: code=%d signal=%s", e.Code, e.Signal)
}

// Interact 将宿主原始终端连接到Guest PTY，结束后恢复终端并等待实际退出结果。
func Interact(ctx context.Context, client guest.Client, info protocol.SessionInfo, input, output *os.File) (result error) {
	terminal, err := client.AttachTerminal(ctx, info.SessionID)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, terminal.Close()) }()
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	inputFD, outputFD := int(input.Fd()), int(output.Fd())
	original, err := unix.IoctlGetTermios(inputFD, unix.TCGETS)
	tty := err == nil
	if tty {
		raw := *original
		raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
		raw.Oflag &^= unix.OPOST
		raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
		raw.Cflag &^= unix.CSIZE | unix.PARENB
		raw.Cflag |= unix.CS8
		raw.Cc[unix.VMIN] = 1
		raw.Cc[unix.VTIME] = 0
		if err = unix.IoctlSetTermios(inputFD, unix.TCSETS, &raw); err != nil {
			return err
		}
		defer func() { result = errors.Join(result, unix.IoctlSetTermios(inputFD, unix.TCSETS, original)) }()
	} else if !errors.Is(err, unix.ENOTTY) {
		return err
	}
	// 独占本次输出FD，非阻塞写与poll共同保证取消可解除慢下游背压。
	flags, err := unix.FcntlInt(uintptr(outputFD), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if err = unix.SetNonblock(outputFD, true); err != nil {
		return err
	}
	defer func() {
		_, err := unix.FcntlInt(uintptr(outputFD), unix.F_SETFL, flags)
		result = errors.Join(result, err)
	}()
	inputDone := make(chan error, 1)
	outputDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := readFD(life, inputFD, buffer)
			if n > 0 {
				written, writeErr := terminal.Write(buffer[:n])
				if writeErr == nil && written != n {
					writeErr = io.ErrShortWrite
				}
				if writeErr != nil {
					inputDone <- writeErr
					return
				}
			}
			if errors.Is(err, io.EOF) {
				inputDone <- terminal.CloseWrite(life)
				return
			}
			if err != nil {
				inputDone <- err
				return
			}
		}
	}()
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := terminal.Read(buffer)
			if n > 0 {
				if writeErr := writeFD(life, outputFD, buffer[:n]); writeErr != nil {
					outputDone <- writeErr
					return
				}
			}
			if errors.Is(err, io.EOF) {
				outputDone <- nil
				return
			}
			if err != nil {
				outputDone <- err
				return
			}
		}
	}()
	// 无论哪个方向结束，关闭授权解除Guest I/O，再等待两个任务退出。
	inputFinished, outputFinished := false, false
	defer func() {
		cancel()
		terminal.Close()
		if !inputFinished {
			<-inputDone
		}
		if !outputFinished {
			<-outputDone
		}
	}()
	resized := make(chan os.Signal, 1)
	if tty {
		signal.Notify(resized, syscall.SIGWINCH)
		defer signal.Stop(resized)
		resized <- syscall.SIGWINCH
	}
	activeInput := inputDone
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-activeInput:
			inputFinished = true
			activeInput = nil
			if err != nil {
				return err
			}
		case err := <-outputDone:
			outputFinished = true
			if err != nil {
				return err
			}
			exit, err := client.WaitSession(ctx, info.SessionID)
			if err != nil {
				return err
			}
			if exit.Code != 0 || exit.Signal != "" {
				return &ExitError{Code: exit.Code, Signal: exit.Signal}
			}
			return nil
		case <-resized:
			size, err := unix.IoctlGetWinsize(inputFD, unix.TIOCGWINSZ)
			if err != nil {
				return err
			}
			if size.Row == 0 || size.Col == 0 {
				continue
			}
			if err := terminal.Resize(life, protocol.Dimension(size.Row), protocol.Dimension(size.Col)); err != nil {
				return err
			}
		}
	}
}

// writeFD 在独占非阻塞FD上有界等待输出空间，不吞掉取消或实际写入失败。
func writeFD(ctx context.Context, fd int, data []byte) error {
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		n, err := unix.Poll(events, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if events[0].Revents&unix.POLLNVAL != 0 {
			return unix.EBADF
		}
		n, err = unix.Write(fd, data)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		data = data[n:]
	}
	return nil
}
