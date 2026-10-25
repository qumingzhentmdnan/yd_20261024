package protocol

import (
	"bytes"
	"testing"
)

// TestChangeChunkRoundTrip 验证base64字节块编码，空串合法而缺失/null/畸形base64无效。
func TestChangeChunkRoundTrip(t *testing.T) {
	for _, data := range [][]byte{[]byte{}, []byte("hello"), bytes.Repeat([]byte{0xff}, ExportChunkBytes)} {
		raw, err := Encode(ChangeChunk{Data: data})
		if err != nil {
			t.Fatal(err)
		}
		var chunk ChangeChunk
		if err = Decode(raw, &chunk); err != nil || !bytes.Equal(data, chunk.Data) {
			t.Fatalf("roundtrip: %v", err)
		}
	}
	for _, raw := range []string{`{}`, `{"data":null}`, `{"data":"???"}`, `{"data":[]}`} {
		var chunk ChangeChunk
		if err := Decode([]byte(raw), &chunk); err == nil {
			t.Fatalf("接受%s", raw)
		}
	}
}
