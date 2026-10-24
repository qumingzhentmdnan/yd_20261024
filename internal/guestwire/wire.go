// Package guestwire 为真实双通道提供共用帧时间预算，不承载业务状态。
package guestwire

import (
	"bytes"
	"context"
	"io"
	"net"
	"time"

	"safeagent/internal/protocol"
)

const Budget = 2 * time.Second

// Read 允许空闲等待，从首字节起限制完整帧的读取预算；同一连接只能有一个读者。
func Read(c net.Conn, port protocol.Port, direction protocol.Direction) (protocol.Frame, error) {
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		return protocol.Frame{}, err
	}
	var first [1]byte
	if _, err := io.ReadFull(c, first[:]); err != nil {
		return protocol.Frame{}, err
	}
	if err := c.SetReadDeadline(time.Now().Add(Budget)); err != nil {
		return protocol.Frame{}, err
	}
	return protocol.ReadFrame(io.MultiReader(bytes.NewReader(first[:]), c), port, direction)
}

// Write 写出单帧，调用方须串行化；控制帧限制预算，数据帧由 attachment 关闭打断背压。
func Write(c net.Conn, port protocol.Port, direction protocol.Direction, f protocol.Frame, deadline time.Time) error {
	if err := c.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return protocol.WriteFrame(c, port, direction, f)
}

// Deadline 取当前控制预算与调用方截止时间的较早值。
func Deadline(ctx context.Context) time.Time {
	d := time.Now().Add(Budget)
	if earlier, ok := ctx.Deadline(); ok && earlier.Before(d) {
		return earlier
	}
	return d
}
