package main

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"strings"
	"unicode"

	"safeagent/internal/host/workspace"
)

// reviewWorkspace 展示冻结差异，仅精确确认本次审核ID后回写宿主项目。
func reviewWorkspace(ctx context.Context, review *workspace.Review) error {
	fmt.Fprintln(os.Stderr, "\n工作区审核ID:", review.ID(), "（未批准时保留Host副本，可用 --resume-review 恢复）")
	diff := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, review.Diff())
	fmt.Fprintln(os.Stderr, diff)
	fmt.Fprintln(os.Stderr, "回写时保留同目录 .safeagent-backup-* 原文件备份；请自行确认后清理。")
	fmt.Fprintf(os.Stderr, "确认全部导出请输入 EXPORT %s；其他输入保留副本并结束: ", review.ID())
	input, err := readConfirmation(ctx, os.Stdin)
	if err != nil {
		return err
	}
	if strings.TrimSpace(input) != "EXPORT "+review.ID() {
		return nil
	}
	return review.Apply(ctx, review.ID())
}

// readConfirmation 独占审核输入FD，以有界poll等待响应取消，不遗留阻塞读取协程。
func readConfirmation(ctx context.Context, input *os.File) (string, error) {
	var out strings.Builder
	fd := int(input.Fd())
	buffer := make([]byte, 1)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(poll, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		n, err = unix.Read(fd, buffer)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", io.EOF
		}
		if buffer[0] == '\n' {
			return out.String(), nil
		}
		if out.Len() >= 256 {
			return "", fmt.Errorf("确认输入过长")
		}
		out.WriteByte(buffer[0])
	}
}
