package proxywire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// TestGoldenFrames 验证Host和Guest唯一共用的黄金字节与编解码。
func TestGoldenFrames(t *testing.T) {
	data, err := os.ReadFile("testdata/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		// Kind 是帧类型。
		Kind byte
		// ID 是请求号。
		ID uint32
		// Payload 是十六进制负载。
		Payload string
		// Wire 是完整黄金帧。
		Wire string
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		payload, _ := hex.DecodeString(fixture.Payload)
		wire, _ := hex.DecodeString(fixture.Wire)
		left, right := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- Write(left, Frame{fixture.Kind, fixture.ID, payload}, time.Now().Add(time.Second))
			left.Close()
		}()
		actual, err := io.ReadAll(right)
		right.Close()
		if err != nil || !bytes.Equal(actual, wire) {
			t.Fatal(actual, err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		left, right = net.Pipe()
		go func() { left.Write(wire); left.Close() }()
		frame, err := Read(right, time.Now().Add(time.Second))
		right.Close()
		if err != nil || frame.Kind != fixture.Kind || frame.ID != fixture.ID || !bytes.Equal(frame.Payload, payload) {
			t.Fatal(frame, err)
		}
	}
}

// TestRejectMalformedFrames 验证半帧、错误namespace、超长帧和空业务ID。
func TestRejectMalformedFrames(t *testing.T) {
	for _, wire := range [][]byte{[]byte("S"), []byte("SAGP\x00\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00"), {'S', 'A', 'P', 'X', 0, 1, 4, 0, 0, 1, 0, 0, 0, 0, 0, 1}, {'S', 'A', 'P', 'X', 0, 1, 4, 0, 0, 0, 0, 1, 0, 0, 0, 0}} {
		left, right := net.Pipe()
		go func() { left.Write(wire); left.Close() }()
		_, err := Read(right, time.Now().Add(time.Second))
		right.Close()
		if err == nil {
			t.Fatal("畸形帧未拒绝")
		}
	}
	for _, data := range []string{`{"unknown":1}`, `{} {}`, `null {}`} {
		if Decode([]byte(data), new(RequestMeta)) == nil {
			t.Fatal(data)
		}
	}
}

// shortConn 在真实连接上制造短写，验证codec不会丢弃剩余字节。
type shortConn struct {
	// Conn 保存实际传输，deadline与关闭行为保持真实。
	net.Conn
}

// Write 每次最多写三个字节。
func (c shortConn) Write(p []byte) (int, error) { return c.Conn.Write(p[:min(len(p), 3)]) }

// TestShortWriteAndMetadata 验证短写、请求响应元数据和二进制内容。
func TestShortWriteAndMetadata(t *testing.T) {
	for _, meta := range []any{RequestMeta{Service: "model", Method: "POST", Path: "/v1/responses"}, ResponseMeta{Status: 200}} {
		data, err := Encode(meta)
		if err != nil {
			t.Fatal(err)
		}
		left, right := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- Write(shortConn{left}, Frame{Kind: Data, ID: 7, Payload: data}, time.Now().Add(time.Second))
			left.Close()
		}()
		f, err := Read(right, time.Now().Add(time.Second))
		right.Close()
		if err != nil || !bytes.Equal(f.Payload, data) {
			t.Fatal(f, err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
}
