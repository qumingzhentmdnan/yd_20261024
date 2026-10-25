package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
)

// payloadCase 是正文 Schema 与 Go 编解码共用的负载正反例。
type payloadCase struct {
	// Name 标识确定的测试场景。
	Name string `json:"name"`
	// Definition 指向正文 Schema 的具名定义。
	Definition string `json:"definition"`
	// Valid 表示是否应被该定义接受。
	Valid bool `json:"valid"`
	// Payload 保留未经 Go DTO 改写的原始负载。
	Payload json.RawMessage `json:"payload"`
}

// TestPayloadFixtures 确认 Go 实现消费与 JSON Schema 完全相同的正反例。
func TestPayloadFixtures(t *testing.T) {
	data, err := os.ReadFile("../../design/fixtures/guest-protocol-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []payloadCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var err error
			if strings.HasSuffix(c.Definition, "Request") || c.Definition == "RequestEnvelope" {
				var req Request
				err = Decode(c.Payload, &req)
				if err == nil && c.Definition != "RequestEnvelope" {
					_, err = DecodeRequest(c.Payload)
				}
			} else if strings.HasSuffix(c.Definition, "Response") {
				var r Response
				err = Decode(c.Payload, &r)
				if err == nil && r.Error == nil {
					method := Method(strings.TrimSuffix(c.Definition, "Response"))
					if method != "" {
						_, err = DecodeResponse(c.Payload, Request{RequestID: r.RequestID, Epoch: r.Epoch, ConnectionID: r.ConnectionID, Method: method})
					}
				}
			} else {
				var target any
				switch c.Definition {
				case "Bind":
					target = &Bind{}
				case "Bound":
					target = &Bound{}
				case "ListChangesParams":
					target = &ListChangesParams{}
				case "ReadChangeChunkParams":
					target = &ReadChangeChunkParams{}
				case "StopSessionParams":
					target = &StopSessionParams{}
				case "ResizeTerminalParams":
					target = &ResizeTerminalParams{}
				case "SessionExitedEvent":
					target = &SessionExitedEvent{}
				case "SessionInfo":
					target = &SessionInfo{}
				case "Exit":
					target = &SessionExit{}
				case "OpaqueID":
					target = new(OpaqueID)
				case "Cancel":
					target = &Cancel{}
				case "Abort":
					target = &Error{}
				default:
					t.Fatalf("fixture定义尚未接入: %s", c.Definition)
				}
				err = Decode(c.Payload, target)
			}
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v err=%v", c.Valid, err)
			}
		})
	}
}

// frameCase 是独立 Python、旧 Go spike 与当前编解码共用的黄金字节。
type frameCase struct {
	// Name 标识帧边界或错误头场景。
	Name string `json:"name"`
	// Terminal 选择终端端口。
	Terminal bool `json:"terminal"`
	// Valid 只表示帧结构有效，不代表 payload 业务字段有效。
	Valid bool `json:"valid"`
	// Kind 是合法帧种类。
	Kind Kind `json:"kind"`
	// Stream 是合法帧的流标识。
	Stream StreamID `json:"stream"`
	// BodyHex 是完整负载。
	BodyHex string `json:"bodyHex"`
	// Hex 是完整或故意截断的帧字节。
	Hex string `json:"hex"`
}

// TestFramesOnMemoryTransport 覆盖逐字节分片、半帧、长度、方向和完整出站字节。
func TestFramesOnMemoryTransport(t *testing.T) {
	data, err := os.ReadFile("testdata/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []frameCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(c.Hex)
			if err != nil {
				t.Fatal(err)
			}
			port := Control
			if c.Terminal {
				port = Terminal
			}
			direction := HostToGuest
			if c.Kind == ResponseKind || c.Kind == EventKind || c.Kind == BoundKind {
				direction = GuestToHost
			}
			left, right := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer right.Close()
				for _, b := range raw {
					if _, err := right.Write([]byte{b}); err != nil {
						return
					}
				}
			}()
			f, err := ReadFrame(left, port, direction)
			left.Close()
			<-done
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v err=%v", c.Valid, err)
			}
			if !c.Valid {
				return
			}
			body, _ := hex.DecodeString(c.BodyHex)
			if f.Kind != c.Kind || f.StreamID != c.Stream || !bytes.Equal(f.Payload, body) {
				t.Fatal("解码偏离黄金字节")
			}
			var out bytes.Buffer
			if err := WriteFrame(&out, port, direction, f); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), raw) {
				t.Fatal("编码偏离黄金字节")
			}
			if c.Kind == RequestKind || c.Kind == ResponseKind || c.Kind == BoundKind {
				if _, err := ReadFrame(bytes.NewReader(raw), port, 1-direction); err == nil {
					t.Fatal("错误方向未拒绝")
				}
			}
		})
	}
}

// TestStrictJSONAndCounters 拒绝零值掩盖、大小写别名、重复键、非法文本与计数溢出。
func TestStrictJSONAndCounters(t *testing.T) {
	for _, data := range []string{`{"sessionID":"s","graceMs":1}`, `{"sessionID":"s","graceMs":1,"forceAfterTimeout":false,"GraceMs":2}`, `{"sessionID":"s","graceMs":1,"graceMs":2,"forceAfterTimeout":false}`, `{"sessionID":"s","graceMs":1,"forceAfterTimeout":false} {}`, `null`} {
		var target StopSessionParams
		if err := Decode([]byte(data), &target); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	for _, text := range []string{`"00"`, `"18446744073709551616"`, `0`, `null`, `"+1"`} {
		var c Counter
		if err := json.Unmarshal([]byte(text), &c); err == nil {
			t.Fatalf("accepted counter %s", text)
		}
	}
	var seq Sequence
	if err := json.Unmarshal([]byte(`"0"`), &seq); err == nil {
		t.Fatal("零序号被接受")
	}
	var counter Counter
	if err := json.Unmarshal([]byte(`"18446744073709551615"`), &counter); err != nil {
		t.Fatal(err)
	}
	var target Empty
	if err := Decode([]byte{'{', '"', 0xff, '"', ':', '0', '}'}, &target); err == nil {
		t.Fatal("非法UTF-8被接受")
	}
}

// TestOperationVectors 确认跨语言固定数组顺序和指纹不受 JSON 参数顺序影响。
func TestOperationVectors(t *testing.T) {
	data, err := os.ReadFile("../../design/fixtures/guest-operation-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		// Method 选择参数契约。
		Method Method `json:"method"`
		// Params 保留原始参数。
		Params json.RawMessage `json:"params"`
		// Canonical 是固定顺序的规范数组文本。
		Canonical string `json:"canonical"`
		// SHA256 是规范数组的黄金摘要。
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		r := Request{Method: v.Method, Params: v.Params}
		p, err := r.DecodeParams()
		if err != nil {
			t.Fatal(err)
		}
		canonical, digest, err := Fingerprint(v.Method, reflect.ValueOf(p).Elem().Interface())
		if err != nil || canonical != v.Canonical || digest != v.SHA256 {
			t.Fatalf("%s: %s %s %v", v.Method, canonical, digest, err)
		}
	}
}

// TestCleanEOFAndHalfPayload 区分完整连接结束与已读头后缺失负载。
func TestCleanEOFAndHalfPayload(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader(nil), Control, HostToGuest); err != io.EOF {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString("53414750000101000000000100000000")
	if _, err := ReadFrame(bytes.NewReader(raw), Control, HostToGuest); err != io.ErrUnexpectedEOF {
		t.Fatal(err)
	}
}

// TestEncodeMethodBoundaries 确认出站消息不能绕开方法参数、结果和必填数组约束。
func TestEncodeMethodBoundaries(t *testing.T) {
	request := Request{RequestID: 1, Epoch: "boot-1", ConnectionID: "11111111-1111-4111-8111-111111111111", Method: MethodGetState, TimeoutMs: 100, Params: json.RawMessage(`{}`)}
	if _, err := EncodeRequest(request); err != nil {
		t.Fatal(err)
	}
	request.Params = json.RawMessage(`{"sessionID":"unexpected"}`)
	if _, err := EncodeRequest(request); err == nil {
		t.Fatal("接受了其他方法的参数")
	}
	request.Params = json.RawMessage(`{}`)
	response := Response{RequestID: request.RequestID, Epoch: request.Epoch, ConnectionID: request.ConnectionID, Result: json.RawMessage(`{"prepared":null,"sessions":[],"currentSessionID":null,"attachment":null}`)}
	if _, err := EncodeResponse(response, request); err != nil {
		t.Fatal(err)
	}
	response.Result = json.RawMessage(`{}`)
	if _, err := EncodeResponse(response, request); err == nil {
		t.Fatal("接受了错误方法结果")
	}
	response.Result = json.RawMessage(`{"prepared":null,"sessions":null,"currentSessionID":null,"attachment":null}`)
	if _, err := EncodeResponse(response, request); err == nil {
		t.Fatal("接受了 null 数组")
	}
	if _, err := Encode(GuestState{}); err == nil {
		t.Fatal("编码了缺失的必填数组")
	}
	request.Method = Method(strings.Repeat("未", 64))
	data, err := EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRequest(data)
	if err != nil || decoded.Method != request.Method {
		t.Fatal(decoded, err)
	}
	if _, err := decoded.DecodeParams(); err == nil {
		t.Fatal("未知方法未返回 UNIMPLEMENTED")
	}
}
