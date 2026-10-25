package workspacewire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestGoldenFrame 固定SAWI头字节并验证真实短写循环。
func TestGoldenFrame(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	done := make(chan error, 1)
	go func() {
		done <- Write(shortConn{Conn: left}, Frame{Kind: Data, Payload: []byte{0, 255}}, time.Now().Add(time.Second))
	}()
	raw := make([]byte, 18)
	if _, err := io.ReadFull(right, raw); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(raw) != "5341574900010300000000020000000100ff" {
		t.Fatal(hex.EncodeToString(raw))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestFramesRejectInvalidHeaders 验证半帧、版本、保留位、ID和超长负载在读取前被拒绝。
func TestFramesRejectInvalidHeaders(t *testing.T) {
	valid, _ := hex.DecodeString("5341574900010300000000020000000100ff")
	for _, scenario := range []string{"valid", "partial", "magic", "version", "reserved", "id", "oversize", "empty-data"} {
		t.Run(scenario, func(t *testing.T) {
			data := bytes.Clone(valid)
			switch scenario {
			case "partial":
				data = data[:17]
			case "magic":
				data[0] = 0
			case "version":
				data[5] = 2
			case "reserved":
				data[7] = 1
			case "id":
				data[15] = 2
			case "oversize":
				data[8] = 1
			case "empty-data":
				data[11] = 0
				data = data[:16]
			}
			left, right := net.Pipe()
			defer right.Close()
			done := make(chan struct{})
			go func() { defer close(done); left.Write(data); left.Close() }()
			frame, err := Read(right, time.Now().Add(time.Second))
			right.Close()
			<-done
			if scenario == "valid" {
				if err != nil || !bytes.Equal(frame.Payload, []byte{0, 255}) {
					t.Fatal(frame, err)
				}
			} else if err == nil {
				t.Fatal("非法帧成功")
			}
		})
	}
}

// TestStrictMetadata 拒绝重复、缺失、未知、空值、尾部和非法错误码。
func TestStrictMetadata(t *testing.T) {
	for _, value := range []string{`{"code":"IO","code":"LIMIT"}`, `{"Code":"IO"}`, `{}`, `{"code":null}`, `{"code":"oops"}`, `{"code":"IO"} {}`, `{"code":"IO","extra":1}`} {
		var failure Failure
		if err := Decode([]byte(value), &failure); err == nil {
			t.Fatal(value)
		}
	}
	var failure Failure
	if err := Decode([]byte(" { \"code\" : \"IO\" } "), &failure); err != nil {
		t.Fatal(err)
	}
	if err := Decode([]byte{255}, &failure); !errors.Is(err, ErrFrame) {
		t.Fatal(err)
	}
}

// shortConn 通过真实流把每次写入限制为三个字节，验证短写消费。
type shortConn struct {
	// Conn 是被包装的内存全双工连接。
	net.Conn
}

// Write 强制每次只写出最多三个字节。
func (c shortConn) Write(data []byte) (int, error) { return c.Conn.Write(data[:min(len(data), 3)]) }

// TestGoldenFixtures 使用同一外部黄金字节验证六种帧和业务元数据。
func TestGoldenFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for key, value := range fixtures {
		t.Run(key, func(t *testing.T) {
			raw, err := hex.DecodeString(value)
			if err != nil {
				t.Fatal(err)
			}
			left, right := net.Pipe()
			done := make(chan struct{})
			go func() { defer close(done); left.Write(raw); left.Close() }()
			frame, err := Read(right, time.Now().Add(time.Second))
			right.Close()
			<-done
			if err != nil {
				t.Fatal(err)
			}
			kind, _ := strconv.Atoi(key)
			if frame.Kind != byte(kind) {
				t.Fatal(frame.Kind)
			}
			switch frame.Kind {
			case Begin:
				var v BeginMeta
				err = Decode(frame.Payload, &v)
			case Committed:
				var v Imported
				err = Decode(frame.Payload, &v)
			case Rejected:
				var v Failure
				err = Decode(frame.Payload, &v)
			}
			if err != nil {
				t.Fatal(err)
			}
			left, right = net.Pipe()
			defer right.Close()
			written := make(chan error, 1)
			go func() { defer left.Close(); written <- Write(left, frame, time.Now().Add(time.Second)) }()
			actual, err := io.ReadAll(right)
			if err != nil || !bytes.Equal(actual, raw) {
				t.Fatal(actual, err)
			}
			if err = <-written; err != nil {
				t.Fatal(err)
			}
		})
	}
}
