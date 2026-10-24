package protocol

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	HeaderSize    = 16
	ControlLimit  = 65536
	TerminalLimit = 16384
)

// Kind 表示帧的固定消息种类。
type Kind byte

const (
	RequestKind  Kind = 1
	ResponseKind Kind = 2
	EventKind    Kind = 3
	CancelKind   Kind = 4
	BindKind     Kind = 16
	BoundKind    Kind = 17
	DataKind     Kind = 18
	FinKind      Kind = 19
	AbortKind    Kind = 20
)

// Port 限定实际接收或发送的物理端口。
type Port byte

const (
	Control Port = iota
	Terminal
)

// Direction 限定当前帧的发送方向。
type Direction byte

const (
	HostToGuest Direction = iota
	GuestToHost
)

// Frame 是完整且有界的帧，不携带未验证的长度。
type Frame struct {
	// Kind 决定负载解释方式。
	Kind Kind
	// StreamID 在控制端口必须为零，终端必须非零。
	StreamID StreamID
	// Payload 不包含固定十六字节头。
	Payload []byte
}

var ErrFrame = errors.New("invalid guest protocol frame")

// checkFrame 检查方向、端口和分配前的长度边界。
func checkFrame(port Port, direction Direction, kind Kind, stream StreamID, size uint32) error {
	if direction != HostToGuest && direction != GuestToHost {
		return ErrFrame
	}
	if port == Control {
		if stream != 0 || size > ControlLimit {
			return ErrFrame
		}
		if direction == HostToGuest && (kind == RequestKind || kind == CancelKind) {
			return nil
		}
		if direction == GuestToHost && (kind == ResponseKind || kind == EventKind) {
			return nil
		}
	} else if port == Terminal {
		if stream == 0 || size > TerminalLimit || (kind == FinKind && size != 0) {
			return ErrFrame
		}
		if kind == DataKind || kind == FinKind || kind == AbortKind || (kind == BindKind && direction == HostToGuest) || (kind == BoundKind && direction == GuestToHost) {
			return nil
		}
	}
	return ErrFrame
}

// ReadFrame 在分配 payload 前检查头；半帧返回错误，不扫描下一个 magic。
// 调用方负责基于首字节设置帧预算，并在失败后关闭所属连接或 attachment。
func ReadFrame(r io.Reader, port Port, direction Direction) (Frame, error) {
	var header [HeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	if string(header[:4]) != "SAGP" || header[4] != 0 || header[5] != 1 || header[7] != 0 {
		return Frame{}, ErrFrame
	}
	kind, stream, size := Kind(header[6]), StreamID(binary.BigEndian.Uint32(header[12:])), binary.BigEndian.Uint32(header[8:12])
	if err := checkFrame(port, direction, kind, stream, size); err != nil {
		return Frame{}, err
	}
	f := Frame{Kind: kind, StreamID: stream, Payload: make([]byte, size)}
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return f, nil
}

// WriteFrame 先验证出站帧，再处理完整头与负载的短写。
func WriteFrame(w io.Writer, port Port, direction Direction, f Frame) error {
	if len(f.Payload) > ControlLimit {
		return ErrFrame
	}
	if err := checkFrame(port, direction, f.Kind, f.StreamID, uint32(len(f.Payload))); err != nil {
		return err
	}
	var header [HeaderSize]byte
	copy(header[:], "SAGP")
	header[5] = 1
	header[6] = byte(f.Kind)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(f.Payload)))
	binary.BigEndian.PutUint32(header[12:], uint32(f.StreamID))
	for _, data := range [][]byte{header[:], f.Payload} {
		for len(data) > 0 {
			n, err := w.Write(data)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			data = data[n:]
		}
	}
	return nil
}
