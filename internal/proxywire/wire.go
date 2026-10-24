// Package proxywire 定义与控制协议隔离的proxy v0.1编解码。
package proxywire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	Limit             = 16384
	RequestLimit      = 1 << 20
	Budget            = 2 * time.Second
	Total             = 60 * time.Second
	Ready        byte = 1
	Request      byte = 2
	Response     byte = 3
	Data         byte = 4
	End          byte = 5
	Abort        byte = 6
)

var ErrFrame = errors.New("proxy帧无效")

// Frame 保存有界物理帧，不包含可信身份。
type Frame struct {
	// Kind 决定负载格式。
	Kind byte
	// ID 是当前物理通道内递增请求号，Ready为零。
	ID uint32
	// Payload 最大为Limit字节。
	Payload []byte
}

// RequestMeta 只携带逻辑服务和HTTP业务输入。
type RequestMeta struct {
	// Service 是Host配置的逻辑引用。
	Service string `json:"service"`
	// Method 是HTTP方法，由Gateway验证。
	Method string `json:"method"`
	// Path 是不含源站的业务路径。
	Path string `json:"path"`
	// Headers 由Gateway重新筛选。
	Headers http.Header `json:"headers"`
}

// ResponseMeta 保存Gateway过滤后的HTTP响应头。
type ResponseMeta struct {
	// Status 是HTTP响应状态。
	Status int `json:"status"`
	// Headers 不包含Host认证信息。
	Headers http.Header `json:"headers"`
}

// valid 校验外部帧的类型、ID与容量边界。
func valid(f Frame) bool {
	if len(f.Payload) > Limit || f.Kind < Ready || f.Kind > Abort {
		return false
	}
	if f.Kind == Ready {
		return f.ID == 0 && len(f.Payload) == 0
	}
	if f.ID == 0 {
		return false
	}
	if f.Kind == End || f.Kind == Abort {
		return len(f.Payload) == 0
	}
	return len(f.Payload) > 0
}

// Read 使用调用方给定首字节期限，并将剩余帧限定在两秒内。
func Read(c net.Conn, deadline time.Time) (Frame, error) {
	if err := c.SetReadDeadline(deadline); err != nil {
		return Frame{}, err
	}
	var h [16]byte
	if _, err := io.ReadFull(c, h[:1]); err != nil {
		return Frame{}, err
	}
	d := time.Now().Add(Budget)
	if !deadline.IsZero() && deadline.Before(d) {
		d = deadline
	}
	if err := c.SetReadDeadline(d); err != nil {
		return Frame{}, err
	}
	if _, err := io.ReadFull(c, h[1:]); err != nil {
		return Frame{}, err
	}
	size := binary.BigEndian.Uint32(h[8:12])
	if string(h[:4]) != "SAPX" || h[4] != 0 || h[5] != 1 || h[7] != 0 || size > Limit {
		return Frame{}, ErrFrame
	}
	f := Frame{Kind: h[6], ID: binary.BigEndian.Uint32(h[12:]), Payload: make([]byte, size)}
	if !valid(f) {
		return Frame{}, ErrFrame
	}
	_, err := io.ReadFull(c, f.Payload)
	return f, err
}

// Write 校验完整帧并处理真实字符设备的短写。
func Write(c net.Conn, f Frame, deadline time.Time) error {
	if !valid(f) {
		return ErrFrame
	}
	if err := c.SetWriteDeadline(deadline); err != nil {
		return err
	}
	var h [16]byte
	copy(h[:], "SAPX")
	h[5] = 1
	h[6] = f.Kind
	binary.BigEndian.PutUint32(h[8:12], uint32(len(f.Payload)))
	binary.BigEndian.PutUint32(h[12:], f.ID)
	for _, data := range [][]byte{h[:], f.Payload} {
		for len(data) > 0 {
			n, err := c.Write(data)
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

// Encode 编码单个有界元数据对象。
func Encode(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(data) > Limit {
		return nil, ErrFrame
	}
	return data, nil
}

// Decode 拒绝未知字段和尾部数据，不接受宽松JSON边界。
func Decode(data []byte, v any) error {
	if len(data) > Limit {
		return ErrFrame
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrFrame
	}
	return nil
}

// Deadline 返回总预算与两秒空闲预算的较早时刻。
func Deadline(total time.Time) time.Time {
	d := time.Now().Add(Budget)
	if !total.IsZero() && total.Before(d) {
		return total
	}
	return d
}
