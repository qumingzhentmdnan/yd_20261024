package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	requestKind   = 1
	responseKind  = 2
	eventKind     = 3
	cancelKind    = 4
	bindKind      = 16
	boundKind     = 17
	dataKind      = 18
	finKind       = 19
	abortKind     = 20
	controlLimit  = 65536
	terminalLimit = 16384
)

// frame 表示控制或终端端口上的一帧。
type frame struct {
	// kind 区分请求、响应、事件和终端消息。
	kind byte
	// stream 是控制端口的零值或终端 attachment 的标识。
	stream uint32
	// body 是已检查长度的原始负载。
	body []byte
}

// readFrame 按候选协议读取一帧，先检查头再分配负载。
func readFrame(r io.Reader, terminal bool) (frame, error) {
	var header [16]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return frame{}, err
	}
	if string(header[:4]) != "SAGP" || header[4] != 0 || header[5] != 1 || header[7] != 0 {
		return frame{}, errors.New("invalid header")
	}
	f := frame{kind: header[6], stream: binary.BigEndian.Uint32(header[12:])}
	limit := uint32(controlLimit)
	if terminal {
		limit = terminalLimit
		if f.stream == 0 || f.kind < bindKind || f.kind > abortKind {
			return frame{}, errors.New("invalid terminal kind/stream")
		}
	} else if f.stream != 0 || f.kind < requestKind || f.kind > cancelKind {
		return frame{}, errors.New("invalid control kind/stream")
	}
	size := binary.BigEndian.Uint32(header[8:12])
	if size > limit || (f.kind == finKind && size != 0) {
		return frame{}, errors.New("invalid length")
	}
	f.body = make([]byte, size)
	_, err := io.ReadFull(r, f.body)
	return f, err
}

// writeFrame 写入完整帧，处理真实设备的短写。
func writeFrame(w io.Writer, f frame) error {
	data := make([]byte, 16+len(f.body))
	copy(data, "SAGP")
	data[5], data[6] = 1, f.kind
	binary.BigEndian.PutUint32(data[8:12], uint32(len(f.body)))
	binary.BigEndian.PutUint32(data[12:16], f.stream)
	copy(data[16:], f.body)
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
	return nil
}

// checkValue 递归拒绝 JSON 对象的重复键。
func checkValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	if delim != '{' && delim != '[' {
		return errors.New("invalid json delimiter")
	}
	keys := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return errors.New("duplicate json key")
			}
			keys[name] = true
		}
		if err := checkValue(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// decodeJSON 检查单个 UTF-8 JSON 对象，并拒绝未知结构字段。
func decodeJSON(data []byte, target any) error {
	trimmed := bytes.TrimSpace(data)
	if !utf8.Valid(data) || len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("invalid json object")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := checkValue(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing json")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

// jsonBody 编码原型内部构造的固定消息。
func jsonBody(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("原型内部消息无法编码: %v", err))
	}
	return data
}
