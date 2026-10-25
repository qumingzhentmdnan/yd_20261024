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

// TestIncompleteFrameKeepsBudget 验证较长首字节截止不会放宽半个物理帧的两秒完整读取预算。
func TestIncompleteFrameKeepsBudget(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	written := make(chan error, 1)
	go func() { _, err := left.Write([]byte{'S'}); written <- err }()
	start := time.Now()
	_, err := Read(right, start.Add(Total))
	if err == nil {
		t.Fatal("不完整帧没有超时")
	}
	timeout, ok := err.(net.Error)
	if !ok || !timeout.Timeout() {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < Budget || elapsed > Budget+time.Second {
		t.Fatal("完整帧预算改变", elapsed)
	}
	if err = <-written; err != nil {
		t.Fatal(err)
	}
}

// TestFirstByteHonorsTotalDeadline 验证尚未到达首字节也必须服从调用方的请求硬截止。
func TestFirstByteHonorsTotalDeadline(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	start := time.Now()
	_, err := Read(right, start.Add(50*time.Millisecond))
	timeout, ok := err.(net.Error)
	if !ok || !timeout.Timeout() {
		t.Fatal("首字节等待没有总截止", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("总截止没有及时生效")
	}
}
