//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestToolRoundtrip 要求正式调用 ID、UID、工作目录和文件随机内容均匹配才允许最终文本。
func TestToolRoundtrip(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			for _, valid := range []bool{false, true} {
				m := &mockUpstream{agent: agent, nonce: "VM_FILE_TEST"}
				model, path := "gpt-5.4", "/v1/responses"
				if agent == "claude" {
					model, path = "claude-sonnet-4-6", "/v1/messages?beta=true"
				}
				first := map[string]any{"model": model, "tools": []any{map[string]any{"name": "exec_command"}}}
				data, _ := json.Marshal(first)
				req := httptest.NewRequest("POST", path, bytes.NewReader(data))
				req.Header.Set("Authorization", "Bearer proxy-managed")
				response := httptest.NewRecorder()
				m.ServeHTTP(response, req)
				if response.Code != 200 || m.phase != 1 || strings.Contains(response.Body.String(), "MOCK_TOOL_OK") {
					t.Fatalf("首轮未只返回工具调用: %s", response.Body.String())
				}
				output := "TOOL_CWD=/workspace/project\nTOOL_UID=1000\nVM_FILE_TEST\n"
				if !valid {
					output = "MOCK_TOOL_OK"
				}
				second := map[string]any{"model": model}
				if agent == "codex" {
					second["input"] = []any{map[string]any{"type": "function_call_output", "call_id": "call_vm_tool", "output": output}}
				} else {
					second["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_vm_tool", "content": output}}}}
				}
				data, _ = json.Marshal(second)
				req = httptest.NewRequest("POST", path, bytes.NewReader(data))
				req.Header.Set("Authorization", "Bearer proxy-managed")
				response = httptest.NewRecorder()
				m.ServeHTTP(response, req)
				if valid {
					if response.Code != 200 || m.phase != 2 || !strings.Contains(response.Body.String(), "MOCK_TOOL_OK") {
						t.Fatalf("正式结果未成功: %s", response.Body.String())
					}
				} else if response.Code != 400 || m.failure == nil || m.phase != 1 {
					t.Fatalf("纯文本被错误视为执行证据: %s", response.Body.String())
				}
			}
		})
	}
}

// TestToolResultIdentity 拒绝其他调用 ID、普通文本和 Claude 工具错误。
func TestToolResultIdentity(t *testing.T) {
	cases := []struct {
		// agent 指定被测协议。
		agent string
		// body 包含不应接纳的上游输入。
		body string
	}{
		{"codex", `{"input":[{"type":"function_call_output","call_id":"wrong","output":"MOCK_TOOL_OK"}]}`},
		{"codex", `{"input":[{"type":"message","content":"MOCK_TOOL_OK"}]}`},
		{"claude", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_vm_tool","is_error":true,"content":"denied"}]}]}`},
	}
	for _, current := range cases {
		var request map[string]any
		if err := json.Unmarshal([]byte(current.body), &request); err != nil {
			t.Fatal(err)
		}
		if _, ok := toolResult(current.agent, request); ok {
			t.Fatalf("错误接纳 %s", current.body)
		}
	}
}
