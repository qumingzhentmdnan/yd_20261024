// Package console 提供宿主密钥输入和Guest终端交互，不把凭证交给Guest。
package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// SecretLimit 限制单次宿主API Key输入，单位为字节。
const SecretLimit = 4096

// ReadSecret 从专用FD或宿主终端读取单行密钥；终端输入关闭回显且不打印输入内容。
func ReadSecret(ctx context.Context, input *os.File, prompt io.Writer) (secret string, result error) {
	fd := int(input.Fd())
	original, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err == nil {
		hidden := *original
		hidden.Lflag &^= unix.ECHO | unix.ECHONL
		if err = unix.IoctlSetTermios(fd, unix.TCSETS, &hidden); err != nil {
			return "", fmt.Errorf("关闭密钥输入回显: %w", err)
		}
		defer func() {
			result = errors.Join(result, unix.IoctlSetTermios(fd, unix.TCSETS, original))
			if result != nil {
				secret = ""
			}
		}()
		if _, err = io.WriteString(prompt, "API Key（仅在宿主本次会话内保存）: "); err != nil {
			return "", err
		}
		defer func() {
			_, err := io.WriteString(prompt, "\n")
			result = errors.Join(result, err)
			if result != nil {
				secret = ""
			}
		}()
	} else if !errors.Is(err, unix.ENOTTY) {
		return "", fmt.Errorf("检查密钥输入终端: %w", err)
	}
	value := make([]byte, 0, 128)
	defer func() { clear(value) }()
	var one [1]byte
	for {
		n, err := readFD(ctx, fd, one[:])
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return "", fmt.Errorf("读取API Key失败: %w", err)
			}
			break
		}
		if n == 0 {
			break
		}
		if one[0] == '\n' {
			break
		}
		if one[0] < 0x21 || one[0] > 0x7e {
			return "", fmt.Errorf("API Key必须是非空可打印ASCII字符")
		}
		if len(value) == SecretLimit {
			return "", fmt.Errorf("API Key超过长度限制")
		}
		value = append(value, one[0])
	}
	if len(value) == 0 {
		return "", fmt.Errorf("API Key不能为空")
	}
	return string(value), nil
}

// readFD 通过有界poll等待独占FD输入，使取消无需遗留阻塞读取goroutine。
func readFD(ctx context.Context, fd int, buffer []byte) (int, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(events, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		if events[0].Revents&unix.POLLNVAL != 0 {
			return 0, unix.EBADF
		}
		n, err = unix.Read(fd, buffer)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if n == 0 && err == nil {
			return 0, io.EOF
		}
		return n, err
	}
}
