//go:build linux

package main

import "testing"

// TestFinalTextRequiresStructuredSuccess 拒绝提示词回显、命令输出、失败回合与Claude错误结果。
func TestFinalTextRequiresStructuredSuccess(t *testing.T) {
	for _, item := range []struct {
		// name 指定被测结果边界。
		name string
		// agent 选择CLI输出协议。
		agent string
		// output 是不含真实凭据的测试输出。
		output string
		// expected 是成功时必须解析出的最终正文，空值表示应拒绝。
		expected string
	}{
		{"codex-success", "codex", "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"upstream-random-value\"}}\n{\"type\":\"turn.completed\"}", "upstream-random-value"},
		{"codex-diagnostic", "codex", "diagnostic upstream-random-value", ""},
		{"codex-command", "codex", "{\"type\":\"item.completed\",\"item\":{\"type\":\"command_execution\",\"text\":\"upstream-random-value\"}}\n{\"type\":\"turn.completed\"}", ""},
		{"codex-incomplete", "codex", `{"type":"item.completed","item":{"type":"agent_message","text":"upstream-random-value"}}`, ""},
		{"codex-failed", "codex", "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"upstream-random-value\"}}\n{\"type\":\"turn.failed\"}", ""},
		{"claude-success", "claude", `{"type":"result","subtype":"success","is_error":false,"result":"upstream-random-value"}`, "upstream-random-value"},
		{"claude-error", "claude", `{"type":"result","subtype":"success","is_error":true,"result":"upstream-random-value"}`, ""},
		{"claude-diagnostic", "claude", `{"type":"system","subtype":"success","result":"upstream-random-value"}`, ""},
		{"claude-trailing", "claude", `{"type":"result","subtype":"success","result":"upstream-random-value"}{}`, ""},
	} {
		t.Run(item.name, func(t *testing.T) {
			got, err := finalText(item.agent, []byte(item.output))
			if item.expected == "" {
				if err == nil {
					t.Fatal("错误输出被当作最终结果")
				}
				return
			}
			if err != nil || got != item.expected {
				t.Fatalf("result=%q err=%v", got, err)
			}
		})
	}
}
