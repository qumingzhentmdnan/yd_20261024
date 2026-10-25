//go:build linux

// Command protocol-smoke 在隔离 Guest 中验证真实 CLI 的工具执行与假上游，不访问真实供应商。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"safeagent/internal/guest/prepare"
	"safeagent/internal/manifest"
)

const toolScript = "printf 'TOOL_CWD='; pwd; printf 'TOOL_UID='; id -u; cat tool-input.txt; printf 'VM_TOOL_MARKER\\n' > tool-marker.txt"

const toolCommand = "/bin/bash /workspace/project/tool-check.sh"

// mockUpstream 保存单轮工具调用状态；互斥锁串行化 HTTP 并发和最终断言。
type mockUpstream struct {
	// mu 保护工具阶段、请求计数与首个错误，生命周期为单次 Guest 验证。
	mu sync.Mutex
	// agent 是明确选中的 codex 或 claude。
	agent string
	// nonce 是仅写入 Guest 测试文件的随机校验文本，不是认证信息。
	nonce string
	// phase 为零时尚未下发工具，为一时等待结果，为二时已验证结果并返回文本。
	phase int
	// requests 记录实际推理请求数量。
	requests int
	// failure 保存首个实际协议错误，空值表示尚未发生错误。
	failure error
}

// event 写入一个协议兼容的 SSE 事件，数据仅包含固定测试内容。
func event(w io.Writer, name string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
}

// contentText 提取 CLI 工具结果支持的文本形式，不把任意消息当作工具执行证据。
func contentText(value any) string {
	switch current := value.(type) {
	case string:
		return current
	case []any:
		var text strings.Builder
		for _, item := range current {
			text.WriteString(contentText(item))
		}
		return text.String()
	case map[string]any:
		return contentText(current["text"])
	}
	return ""
}

// toolResult 只接纳与本次调用 ID 对应的正式工具结果消息。
func toolResult(agent string, request map[string]any) (string, bool) {
	if agent == "codex" {
		items, _ := request["input"].([]any)
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if ok && entry["type"] == "function_call_output" && entry["call_id"] == "call_vm_tool" {
				return contentText(entry["output"]), true
			}
		}
	} else {
		messages, _ := request["messages"].([]any)
		for _, message := range messages {
			entry, ok := message.(map[string]any)
			if !ok || entry["role"] != "user" {
				continue
			}
			blocks, _ := entry["content"].([]any)
			for _, block := range blocks {
				result, ok := block.(map[string]any)
				if ok && result["type"] == "tool_result" && result["tool_use_id"] == "toolu_vm_tool" && result["is_error"] != true {
					return contentText(result["content"]), true
				}
			}
		}
	}
	return "", false
}

// codexTool 仅选择 CLI 实际公布的 shell 工具，并构造该工具的正式参数。
func codexTool(request map[string]any) (string, string, error) {
	tools, _ := request["tools"].([]any)
	for _, tool := range tools {
		entry, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		var arguments any
		switch name {
		case "exec_command":
			arguments = map[string]any{"cmd": toolCommand, "workdir": "/workspace/project", "max_output_tokens": 2000}
		case "shell_command":
			arguments = map[string]any{"command": toolCommand, "workdir": "/workspace/project", "timeout_ms": 10000}
		case "shell":
			arguments = map[string]any{"command": []string{"/bin/bash", "-c", toolCommand}, "workdir": "/workspace/project", "timeout_ms": 10000}
		default:
			continue
		}
		data, err := json.Marshal(arguments)
		return name, string(data), err
	}
	return "", "", fmt.Errorf("CLI 未公布可调用的 shell 工具")
}

// codexResponse 返回一次正式 function_call 或已经验证结果后的最终文本。
func codexResponse(w io.Writer, item map[string]any, phase int) {
	id := fmt.Sprintf("resp_vm_%d", phase)
	event(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "status": "in_progress", "output": []any{}}})
	event(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	event(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	event(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "object": "response", "status": "completed", "model": "gpt-5.4", "output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 3, "total_tokens": 13}}})
}

// claudeResponse 返回一次 Bash tool_use 或已经验证结果后的最终文本。
func claudeResponse(w io.Writer, tool bool) {
	event(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_vm_tool", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 0}}})
	stop := "end_turn"
	if tool {
		stop = "tool_use"
		event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_vm_tool", "name": "Bash", "input": map[string]any{}}})
		input, _ := json.Marshal(map[string]any{"command": toolCommand, "description": "执行已授权的固定 VM 工具验证命令", "timeout": 10000})
		event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
	} else {
		event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "MOCK_TOOL_OK"}})
	}
	event(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 3}})
	event(w, "message_stop", map[string]any{"type": "message_stop"})
}

// ServeHTTP 强制先执行工具并回传真实结果，再允许返回成功文本。
func (m *mockUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		m.failure = err
		http.Error(w, err.Error(), 400)
		return
	}
	var request map[string]any
	if err = json.Unmarshal(body, &request); err != nil {
		m.failure = err
		http.Error(w, err.Error(), 400)
		return
	}
	model, path := "gpt-5.4", "/v1/responses"
	if m.agent == "claude" {
		model, path = "claude-sonnet-4-6", "/v1/messages"
	}
	if r.URL.Path == "/v1/messages/count_tokens" {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":10}`)
		return
	}
	if r.Method != "POST" || r.URL.Path != path || request["model"] != model || r.Header.Get("Authorization") != "Bearer proxy-managed" {
		m.failure = fmt.Errorf("实际 CLI 请求不符合固定路由/模型/占位认证")
		http.Error(w, m.failure.Error(), 400)
		return
	}
	m.requests++
	fmt.Printf("MOCK_REQUEST method=%s uri=%s model=%s placeholder_auth=true anthropic_version=%q anthropic_beta=%q\n", r.Method, r.URL.RequestURI(), model, r.Header.Get("Anthropic-Version"), r.Header.Get("Anthropic-Beta"))
	if m.phase == 0 {
		var item map[string]any
		if m.agent == "codex" {
			name, args, err := codexTool(request)
			if err != nil {
				m.failure = err
				http.Error(w, err.Error(), 400)
				return
			}
			fmt.Printf("TOOL_CALL_SENT agent=codex name=%s sandbox=workspace-write command=%q\n", name, toolCommand)
			item = map[string]any{"id": "fc_vm_tool", "type": "function_call", "call_id": "call_vm_tool", "name": name, "arguments": args, "status": "completed"}
		} else {
			fmt.Printf("TOOL_CALL_SENT agent=claude name=Bash command=%q\n", toolCommand)
		}
		m.phase = 1
		w.Header().Set("Content-Type", "text/event-stream")
		if m.agent == "codex" {
			codexResponse(w, item, 1)
		} else {
			claudeResponse(w, true)
		}
		return
	}
	if m.phase != 1 {
		m.failure = fmt.Errorf("工具完成后出现额外推理请求")
		http.Error(w, m.failure.Error(), 400)
		return
	}
	text, ok := toolResult(m.agent, request)
	fmt.Printf("TOOL_RESULT_RECEIVED agent=%s matched_call=%t output=%q\n", m.agent, ok, text)
	if !ok || !strings.Contains(text, "TOOL_CWD=/workspace/project") || !strings.Contains(text, "TOOL_UID=1000") || !strings.Contains(text, m.nonce) {
		m.failure = fmt.Errorf("缺少真实工作目录、UID1000或文件随机内容的工具结果")
		http.Error(w, m.failure.Error(), 400)
		return
	}
	m.phase = 2
	w.Header().Set("Content-Type", "text/event-stream")
	if m.agent == "codex" {
		codexResponse(w, map[string]any{"id": "msg_vm_done", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "MOCK_TOOL_OK", "annotations": []any{}}}}, 2)
	} else {
		claudeResponse(w, false)
	}
}

// run 在实际 Guest 安装正式配置，以 UID1000 启动真实 CLI 并核验工具落盘结果。
func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("需要明确的 codex 或 claude 选择")
	}
	agent := os.Args[1]
	ref, ok := map[string]string{"codex": "codex@0.114.0", "claude": "claude@2.1.81"}[agent]
	if !ok {
		return fmt.Errorf("未知 CLI %q", agent)
	}
	catalog, err := manifest.NewCatalogFor(ref)
	if err != nil {
		return err
	}
	descriptor := catalog.List()[0]
	definition, err := catalog.Load(ref, descriptor.ManifestDigest, manifest.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		return err
	}
	plan, err := definition.Render(manifest.Values{Home: "/home/agent/protocol-smoke", Workspace: "/workspace/project", ModelGatewayURL: "http://127.0.0.1:18080"})
	if err != nil {
		return err
	}
	if err = prepare.Install(context.Background(), plan, 1000, 1000); err != nil {
		return err
	}
	if err = os.MkdirAll(plan.Directory, 0755); err != nil {
		return err
	}
	if err = os.Chown(plan.Directory, 1000, 1000); err != nil {
		return err
	}
	nonceBytes := make([]byte, 16)
	if _, err = rand.Read(nonceBytes); err != nil {
		return err
	}
	nonce := "VM_FILE_" + hex.EncodeToString(nonceBytes)
	if err = os.WriteFile(plan.Directory+"/tool-input.txt", []byte(nonce+"\n"), 0644); err != nil {
		return err
	}
	if err = os.WriteFile(plan.Directory+"/tool-check.sh", []byte(toolScript+"\n"), 0644); err != nil {
		return err
	}
	mock := &mockUpstream{agent: agent, nonce: nonce}
	listener, err := net.Listen("tcp", "127.0.0.1:18080")
	if err != nil {
		return err
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: mock}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	defer func() { server.Close(); <-served }()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// 仅测试副本显式配置非交互授权；Codex 保留 workspace-write 沙箱且不允许提权。
	args := []string{"-a", "never", "exec", "--skip-git-repo-check", "--json", "Run the fixed VM tool check requested by the upstream, then report its result."}
	if agent == "claude" {
		args = []string{"-p", "Run the fixed VM tool check requested by the upstream, then report its result.", "--output-format", "json", "--settings", plan.Home + "/.claude/settings.json", "--allowedTools", "Bash(" + toolCommand + ")"}
	}
	command := exec.CommandContext(ctx, plan.Executable, args...)
	command.Dir = plan.Directory
	for _, item := range plan.Environment {
		command.Env = append(command.Env, item.Name+"="+item.Value)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err = command.Run()
	fmt.Printf("CLI_PROTOCOL_OUTPUT=%s\n", output.String())
	if err != nil {
		return err
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.failure != nil {
		return mock.failure
	}
	if mock.phase != 2 || mock.requests != 2 || !strings.Contains(output.String(), "MOCK_TOOL_OK") {
		return fmt.Errorf("工具调用、结果回传、最终文本未全部完成")
	}
	marker, err := os.ReadFile(plan.Directory + "/tool-marker.txt")
	if err != nil {
		return err
	}
	info, err := os.Stat(plan.Directory + "/tool-marker.txt")
	if err != nil {
		return err
	}
	owner := info.Sys().(*syscall.Stat_t)
	if string(marker) != "VM_TOOL_MARKER\n" || owner.Uid != 1000 || owner.Gid != 1000 {
		return fmt.Errorf("工具创建标记的内容或 UID/GID 不符")
	}
	fmt.Printf("TOOL_FILE_VERIFIED agent=%s uid=%d gid=%d marker=%q\n", agent, owner.Uid, owner.Gid, marker)
	fmt.Printf("PROTOCOL_SMOKE_OK agent=%s requests=%d uid=1000 tool_roundtrip=true\n", agent, mock.requests)
	return nil
}

// main 只输出无凭据验证结果，失败时返回非零状态。
func main() {
	if err := run(); err != nil {
		fmt.Println("PROTOCOL_SMOKE_ERROR", err)
		os.Exit(1)
	}
}
