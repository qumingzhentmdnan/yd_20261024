// Package workspacewire 定义独立的SAWI工作区导入协议。
package workspacewire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"time"
	"unicode/utf8"

	"safeagent/internal/protocol"
)

const (
	Limit             = 16 << 10
	Budget            = 5 * time.Second
	Total             = 120 * time.Second
	CommitBudget      = 30 * time.Second
	Begin        byte = 1
	Ready        byte = 2
	Data         byte = 3
	End          byte = 4
	Committed    byte = 5
	Rejected     byte = 6
)

var ErrFrame = errors.New("工作区帧无效")

// Frame 保存固定transferID=1的有界帧。
type Frame struct {
	// Kind 指定单次传输状态。
	Kind byte
	// Payload 保存元数据或原始归档字节。
	Payload []byte
}

// BeginMeta 绑定当前可信控制身份及预期基线。
type BeginMeta struct {
	// VMRef 是Hello已建立的VM标识。
	VMRef protocol.VMRef `json:"vmRef"`
	// Epoch 是本次启动代次。
	Epoch protocol.Epoch `json:"epoch"`
	// ConnectionID 是当前控制连接标识。
	ConnectionID protocol.UUID `json:"connectionID"`
	// WorkspaceRef 是Host基线标识。
	WorkspaceRef protocol.OpaqueID `json:"workspaceRef"`
	// WorkspaceDigest 是规范清单摘要。
	WorkspaceDigest protocol.Digest `json:"workspaceDigest"`
	// ArchiveBytes 是精确归档字节数。
	ArchiveBytes int64 `json:"archiveBytes"`
}

// Imported 是当前boot真实导入成功的值快照。
type Imported struct {
	// WorkspaceRef 标识已导入Host基线。
	WorkspaceRef protocol.OpaqueID `json:"workspaceRef"`
	// WorkspaceDigest 绑定导入时规范清单。
	WorkspaceDigest protocol.Digest `json:"workspaceDigest"`
	// FileCount 是普通文件数量。
	FileCount int `json:"fileCount"`
	// TotalBytes 是普通文件字节总数。
	TotalBytes int64 `json:"totalBytes"`
}

// Failure 只对外提供固定错误码。
type Failure struct {
	// Code 不包含内部路径或错误文本。
	Code string `json:"code"`
}

// valid 校验帧头可表达的外部边界。
func valid(f Frame) bool {
	if f.Kind < Begin || f.Kind > Rejected || len(f.Payload) > Limit {
		return false
	}
	if f.Kind == Ready || f.Kind == End {
		return len(f.Payload) == 0
	}
	return len(f.Payload) > 0
}

// Deadline 取阶段预算和总体截止时间的较早值。
func Deadline(total time.Time, budget time.Duration) time.Time {
	d := time.Now().Add(budget)
	if !total.IsZero() && total.Before(d) {
		return total
	}
	return d
}

// Read 允许调用方指定首字节等待，剩余帧始终限制在五秒内。
func Read(c net.Conn, deadline time.Time) (Frame, error) {
	if err := c.SetReadDeadline(deadline); err != nil {
		return Frame{}, err
	}
	var header [16]byte
	if _, err := io.ReadFull(c, header[:1]); err != nil {
		return Frame{}, err
	}
	if err := c.SetReadDeadline(Deadline(deadline, Budget)); err != nil {
		return Frame{}, err
	}
	if _, err := io.ReadFull(c, header[1:]); err != nil {
		return Frame{}, err
	}
	size := binary.BigEndian.Uint32(header[8:12])
	if string(header[:4]) != "SAWI" || header[4] != 0 || header[5] != 1 || header[7] != 0 || binary.BigEndian.Uint32(header[12:]) != 1 || size > Limit {
		return Frame{}, ErrFrame
	}
	f := Frame{Kind: header[6], Payload: make([]byte, size)}
	if !valid(f) {
		return Frame{}, ErrFrame
	}
	_, err := io.ReadFull(c, f.Payload)
	return f, err
}

// Write 写入规范头与负载，处理字符设备短写。
func Write(c net.Conn, f Frame, deadline time.Time) error {
	if !valid(f) {
		return ErrFrame
	}
	if err := c.SetWriteDeadline(deadline); err != nil {
		return err
	}
	var header [16]byte
	copy(header[:], "SAWI")
	header[5] = 1
	header[6] = f.Kind
	binary.BigEndian.PutUint32(header[8:12], uint32(len(f.Payload)))
	binary.BigEndian.PutUint32(header[12:], 1)
	for _, part := range [][]byte{header[:], f.Payload} {
		for len(part) > 0 {
			n, err := c.Write(part)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return nil
}

// Decode 对扁平元数据严格检查重复键、必填字段、编码与值约束。
func Decode(data []byte, target any) error {
	if len(data) > Limit || !utf8.Valid(data) {
		return ErrFrame
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrFrame
	}
	fields := make(map[string]bool)
	shape := reflect.TypeOf(target).Elem()
	allowed := make(map[string]bool, shape.NumField())
	for i := 0; i < shape.NumField(); i++ {
		allowed[shape.Field(i).Tag.Get("json")] = true
	}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		key := token.(string)
		if fields[key] || !allowed[key] {
			return ErrFrame
		}
		fields[key] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return err
		}
		if bytes.Equal(value, []byte("null")) {
			return ErrFrame
		}
	}
	if _, err = decoder.Token(); err != nil {
		return err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return ErrFrame
	}
	if len(fields) != len(allowed) {
		return ErrFrame
	}
	if err = json.Unmarshal(data, target); err != nil {
		return err
	}
	switch value := target.(type) {
	case *BeginMeta:
		for _, v := range []any{value.VMRef, value.Epoch, value.ConnectionID, value.WorkspaceRef, value.WorkspaceDigest} {
			if err = protocol.Validate(v); err != nil {
				return err
			}
		}
		if value.ArchiveBytes <= 0 {
			return ErrFrame
		}
	case *Imported:
		for _, v := range []any{value.WorkspaceRef, value.WorkspaceDigest} {
			if err = protocol.Validate(v); err != nil {
				return err
			}
		}
		if value.FileCount < 0 || value.TotalBytes < 0 {
			return ErrFrame
		}
	case *Failure:
		if value.Code != "INVALID_STATE" && value.Code != "INVALID_BUNDLE" && value.Code != "LIMIT" && value.Code != "IO" {
			return ErrFrame
		}
	}
	return nil
}

// Encode 编码并检查本协议实际消费的元数据类型。
func Encode(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	target := reflect.New(reflect.TypeOf(value)).Interface()
	if err = Decode(data, target); err != nil {
		return nil, err
	}
	return data, nil
}
