package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"sync"
	"testing"
)

// fixture 是 Go 与 Python 共用的字节级预期。
type fixture struct {
	// Name 标识测试场景。
	Name string `json:"name"`
	// Terminal 指定所验证的物理端口。
	Terminal bool `json:"terminal"`
	// Valid 表示帧结构是否应被接受，不代表业务 envelope 有效。
	Valid bool `json:"valid"`
	// Kind 是合法帧的消息类型。
	Kind byte `json:"kind"`
	// Stream 是合法帧的流标识。
	Stream uint32 `json:"stream"`
	// BodyHex 是合法帧原始负载。
	BodyHex string `json:"bodyHex"`
	// Hex 是依据候选规范固定的完整或故意截断帧。
	Hex string `json:"hex"`
}

// TestGoldenFrames 验证固定字节、任意分片和内存通道使用相同编解码规则。
func TestGoldenFrames(t *testing.T) {
	data, err := os.ReadFile("../../../internal/protocol/testdata/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []fixture
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, item := range cases {
		t.Run(item.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(item.Hex)
			if err != nil {
				t.Fatal(err)
			}
			left, right := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer right.Close()
				for _, value := range raw {
					if _, err := right.Write([]byte{value}); err != nil {
						return
					}
				}
			}()
			f, err := readFrame(left, item.Terminal)
			left.Close()
			<-done
			if (err == nil) != item.Valid {
				t.Fatalf("valid=%v err=%v", item.Valid, err)
			}
			if !item.Valid {
				return
			}
			body, err := hex.DecodeString(item.BodyHex)
			if err != nil {
				t.Fatal(err)
			}
			if f.kind != item.Kind || f.stream != item.Stream || !bytes.Equal(f.body, body) {
				t.Fatalf("unexpected frame: %#v", f)
			}
			var output bytes.Buffer
			if err := writeFrame(&output, f); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(output.Bytes(), raw) {
				t.Fatal("黄金字节不一致")
			}
		})
	}
}

// TestStrictJSON 覆盖重复键、尾随值、非法 UTF-8 和未知字段。
func TestStrictJSON(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"rows":24,"rows":25}`),
		[]byte(`{"rows":24} {}`),
		[]byte(`{"unknown":true}`),
		[]byte(`[]`),
		{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'},
	} {
		var p params
		if err := decodeJSON(body, &p); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
	var p params
	if err := decodeJSON([]byte(`{"rows":24,"cols":80}`), &p); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentPrepare 验证同 operation 的并发去重与语义参数比较。
func TestConcurrentPrepare(t *testing.T) {
	g := &guest{operations: map[string]operation{}, sessions: map[string]*session{}}
	c := &connection{guest: g}
	p := params{OperationID: "same-operation", Manifest: "probe-v1", WorkspaceDigest: "sha256:probe-workspace"}
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, problem := c.execute(context.Background(), request{Method: "PrepareAgent"}, p); problem != nil {
				t.Errorf("unexpected %v", problem)
			}
		}()
	}
	workers.Wait()
	if len(g.operations) != 1 {
		t.Fatalf("操作数=%d", len(g.operations))
	}
	p.Manifest = "other"
	if _, problem := c.execute(context.Background(), request{Method: "PrepareAgent"}, p); problem == nil || problem.Code != "CONFLICT" {
		t.Fatal("参数变化未拒绝")
	}
}
