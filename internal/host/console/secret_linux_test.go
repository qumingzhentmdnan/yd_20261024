package console

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestReadSecretFD 验证专用FD读取、不泄露输入及外部输入边界。
func TestReadSecretFD(t *testing.T) {
	for _, item := range []struct {
		// name 是输入边界名称。
		name string
		// value 是显式测试假值，不使用本机真实凭证。
		value string
		// valid 标记该输入是否允许。
		valid bool
	}{{"line", "test-only-key\n", true}, {"eof", "test-only-key", true}, {"empty", "\n", false}, {"control", "test\x00key\n", false}, {"limit", strings.Repeat("k", SecretLimit+1), false}} {
		t.Run(item.name, func(t *testing.T) {
			input, err := os.CreateTemp(t.TempDir(), "secret")
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			if _, err := input.WriteString(item.value); err != nil {
				t.Fatal(err)
			}
			if _, err := input.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			secret, err := ReadSecret(context.Background(), input, &output)
			if (err == nil) != item.valid {
				t.Fatal(err)
			}
			if item.valid && secret != "test-only-key" {
				t.Fatal("密钥字节改变")
			}
			if !item.valid && secret != "" {
				t.Fatal("失败返回密钥")
			}
			if output.Len() != 0 {
				t.Fatal("FD密钥被输出")
			}
		})
	}
}

// TestReadSecretCancellation 验证等待输入时取消会有界退出。
func TestReadSecretCancellation(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := ReadSecret(ctx, input, &bytes.Buffer{}); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("密钥输入未取消")
	}
}
