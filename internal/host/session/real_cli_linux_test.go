package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"safeagent/internal/host/credential"
	"safeagent/internal/host/gateway"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/host/workspace"
	"safeagent/internal/protocol"
	"safeagent/internal/proxywire"
	"safeagent/internal/workspacebundle"
)

// TestRealCLIHostStartup 验证真实镜像经Host会话完整启动与代理就绪后回收；不冒称模型对话验收。
func TestRealCLIHostStartup(t *testing.T) { runRealCLIHost(t, "startup") }

// TestRealCLIHostGateway 验证测试副本中的真实CLI经完整代理与Host假TLS上游收发响应。
func TestRealCLIHostGateway(t *testing.T) { runRealCLIHost(t, "headless") }

// TestRealCLIInteractiveDialogue 验证正式PTY启动参数下的用户输入与同一会话两轮代理响应。
func TestRealCLIInteractiveDialogue(t *testing.T) { runRealCLIHost(t, "interactive") }

// runRealCLIHost 为生产界面启动、原生两轮对话和独立测试副本代理验证提供实际VM编排。
func runRealCLIHost(t *testing.T, mode string) {
	dialogue, interactive := mode != "startup", mode == "interactive"
	t.Helper()
	if os.Getenv("SAFEAGENT_GUEST_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实VM验证")
	}
	image, assets, tools := os.Getenv("SAFEAGENT_REAL_CLI_IMAGE"), os.Getenv("SAFEAGENT_VM_ASSETS"), os.Getenv("SAFEAGENT_QEMU_TOOLS")
	if image == "" || assets == "" || tools == "" {
		t.Fatal("必须显式指定镜像、VM素材与QEMU工具目录")
	}
	data, err := os.ReadFile(filepath.Join(image, "content.json"))
	if err != nil {
		t.Fatal(err)
	}
	var content protocol.Preparation
	if err = json.Unmarshal(data, &content); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "sah-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	driver, err := qemu.NewDriver(filepath.Join(tools, "qemu-system-x86_64"), filepath.Join(tools, "qemu-img"), filepath.Join(root, "vms"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	manager, err := vm.NewManager(ctx, filepath.Join(root, "state.db"), "qemu", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	}()
	source := filepath.Join(root, "project")
	if err = os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "README.md"), []byte("isolated host session\n"), 0644); err != nil {
		t.Fatal(err)
	}
	snapshots, err := workspace.NewStore(filepath.Join(root, "snapshots"), workspacebundle.GuestLimits())
	if err != nil {
		t.Fatal(err)
	}
	secrets := credential.New()
	defer secrets.Close()
	const fakeKey = "test-host-key-not-for-guest"
	if err = secrets.Put(ctx, "session-key", fakeKey); err != nil {
		t.Fatal(err)
	}
	agent := os.Getenv("SAFEAGENT_CLI_AGENT")
	if !dialogue {
		agent = strings.SplitN(string(content.ManifestRef), "@", 2)[0]
	}
	if dialogue && agent != "codex" && agent != "claude" {
		t.Fatal("代理验证必须明确选择SAFEAGENT_CLI_AGENT")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	reply := "HOST_REPLY_" + hex.EncodeToString(nonce[:])
	var requests atomic.Int64
	var backgroundRequests atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !dialogue {
			http.Error(w, "startup test does not model provider dialogue", http.StatusServiceUnavailable)
			return
		}
		body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if readErr != nil {
			t.Error(readErr)
			http.Error(w, "read failed", 400)
			return
		}
		var input map[string]any
		if json.Unmarshal(body, &input) != nil || bytes.Contains(body, []byte(fakeKey)) || (requests.Load() == 0 && bytes.Contains(body, []byte(reply))) {
			t.Error("请求体无效或包含Host密钥")
			http.Error(w, "invalid request", 400)
			return
		}
		path, model, auth := "/v1/responses", "gpt-5.4", r.Header.Get("Authorization") == "Bearer "+fakeKey && r.Header.Get("X-Api-Key") == ""
		if agent == "claude" {
			path, model, auth = "/v1/messages", "claude-sonnet-4-6", r.Header.Get("X-Api-Key") == fakeKey && r.Header.Get("Authorization") == ""
			if r.URL.RawQuery != "beta=true" || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Anthropic-Beta") == "" {
				t.Error("Claude必需query或headers丢失")
				http.Error(w, "missing beta", 400)
				return
			}
		}
		if r.Method != "POST" || r.URL.Path != path || input["model"] != model || !auth {
			t.Error("Host路由、模型或密钥注入不符")
			http.Error(w, "invalid routing", 400)
			return
		}
		// Claude 原生交互会额外请求标题和输入建议；它们不是用户对话回合。
		if interactive && agent == "claude" && (bytes.Contains(body, []byte("Generate a concise, sentence-case title (3-7 words)")) || bytes.Contains(body, []byte("[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"))) {
			backgroundRequests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			writeCLIText(w, agent, `{"title":"Isolated test conversation"}`)
			return
		}
		turn := requests.Add(1)
		responseText := reply
		if interactive {
			responseText = fmt.Sprintf("%s_%d", reply, turn)
		}
		// 故意超过物理帧预算，验证模型首响应等待不会误用两秒传输期限。
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			t.Error("模型首响应前通道已取消")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeCLIText(w, agent, responseText)
	}))
	defer upstream.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	service, err := gateway.CLIService(agent, upstream.URL, "session-key", []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	if err != nil {
		t.Fatal(err)
	}
	modelGateway, err := gateway.New(gateway.Config{Services: []gateway.Service{service}, MaxBindings: 1, MaxConcurrent: 1, MaxRequestBytes: 1 << 20, MaxResponseBytes: 16 << 20, MaxHeaderBytes: 16 << 10, TotalTimeout: proxywire.Total, IdleTimeout: proxywire.Total, RootCAs: roots}, secrets, net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	defer modelGateway.Close()
	basePath := filepath.Join(assets, "workspace-base.raw")
	before, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Backend: manager, Driver: driver, Workspace: snapshots, Gateway: modelGateway, VM: vm.VMConfig{ID: "real-cli-host", Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: vm.TCG, VCPUs: 2, MemoryMiB: 2048, Image: vm.Image{KernelPath: filepath.Join(assets, "guest-kernel/boot/vmlinuz-6.8.0-147-generic"), InitrdPath: filepath.Join(image, "initramfs.cpio.gz"), BaseDiskPath: basePath}}, Selection: workspace.ImportSelection{Root: source, Paths: []string{"README.md"}}, Content: content, ServiceRefs: []string{"model"}, Rows: 40, Cols: 120, StartupTimeout: 2 * time.Minute}
	err = Run(ctx, config, func(ctx context.Context, client guest.Client, info protocol.SessionInfo) error {
		if interactive {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
		}
		terminal, err := client.AttachTerminal(ctx, info.SessionID)
		if err != nil {
			return err
		}
		defer terminal.Close()
		callbackDone := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { defer close(callbackDone); terminal.Close() })
		defer func() {
			if !stop() {
				<-callbackDone
			}
		}()
		var output bytes.Buffer
		buffer := make([]byte, 16384)
		cursorAnswered := false
		steps := map[string]bool{}
		firstSent, secondSent := false, false
		pendingInput := ""
		escape := regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
		for {
			n, err := terminal.Read(buffer)
			output.Write(buffer[:n])
			if output.Len() > 1<<20 {
				return fmt.Errorf("启动输出超过1MiB")
			}
			if bytes.Contains(output.Bytes(), []byte(fakeKey)) {
				return fmt.Errorf("Host测试密钥出现在Guest终端")
			}
			if !cursorAnswered && bytes.Contains(output.Bytes(), []byte("\x1b[6n")) {
				cursorAnswered = true
				if _, writeErr := terminal.Write([]byte("\x1b[1;1R")); writeErr != nil {
					return writeErr
				}
			}
			visible := escape.ReplaceAllStringFunc(output.String(), func(code string) string {
				if strings.HasSuffix(code, "C") || strings.HasSuffix(code, "H") {
					return " "
				}
				return ""
			})
			visible = strings.Join(strings.Fields(visible), " ")
			if interactive {
				// 固定CLI原生引导只确认隔离测试目录；不注入真实密钥或修改权限配置。
				for _, step := range []string{"Welcome to Codex", "Choose the text style", "Do you trust the files", "Do you trust this", "Press Enter to continue", "Yes, I trust this folder"} {
					if strings.Contains(visible, step) && !steps[step] {
						steps[step] = true
						if _, writeErr := terminal.Write([]byte("\r")); writeErr != nil {
							return writeErr
						}
					}
				}
				if pendingInput != "" && strings.Contains(visible, pendingInput) {
					pendingInput = ""
					if _, writeErr := terminal.Write([]byte("\r")); writeErr != nil {
						return writeErr
					}
				}
				if !firstSent && ((agent == "codex" && strings.Contains(visible, "model: gpt-5.4")) || (agent == "claude" && strings.Contains(visible, "for shortcuts"))) {
					firstSent = true
					pendingInput = "Reply with the upstream text."
					if _, writeErr := terminal.Write([]byte("Reply with the upstream text.")); writeErr != nil {
						return writeErr
					}
				}
				if firstSent && !secondSent && strings.Contains(visible, reply+"_1") {
					secondSent = true
					pendingInput = "Second turn: reply with the upstream text."
					if _, writeErr := terminal.Write([]byte("Second turn: reply with the upstream text.")); writeErr != nil {
						return writeErr
					}
				}
				if secondSent && strings.Contains(visible, reply+"_2") {
					t.Logf("原生两轮终端响应=%s", visible)
					return nil
				}
			}
			if !dialogue && (strings.Contains(visible, "Welcome to Codex") || strings.Contains(visible, "OpenAI Codex") || strings.Contains(visible, "Welcome to Claude Code") || strings.Contains(visible, "Choose the text style")) {
				t.Logf("Host完整启动 %s，终端输出=%s", content.ManifestRef, visible)
				return nil
			}
			if dialogue && !interactive && errors.Is(err, io.EOF) {
				exit, waitErr := client.WaitSession(ctx, info.SessionID)
				if waitErr != nil {
					return waitErr
				}
				t.Logf("CLI代理测试输出=%s", visible)
				if exit.Code != 0 || exit.Signal != "" || (!strings.Contains(visible, "CLI_HOST_PROXY_OK agent="+agent) || !strings.Contains(visible, fmt.Sprintf("CLI_HOST_PROXY_RESULT=%q", reply))) {
					return fmt.Errorf("真实CLI代理测试未完成: exit=%d", exit.Code)
				}
				return nil
			}
			if err != nil {
				if interactive {
					t.Logf("原生对话未完成，requests=%d，终端=%s", requests.Load(), visible)
				}
				return err
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Inspect(ctx, vm.VMRef{ID: config.VM.ID}); !errors.Is(err, vm.ErrNotFound) {
		t.Fatal("会话结束后VM仍存在", err)
	}
	after, err := os.ReadFile(basePath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("基础盘改变", err)
	}
	expectedRequests := int64(1)
	if interactive {
		expectedRequests = 2
	}
	if dialogue && requests.Load() != expectedRequests {
		t.Fatalf("实际推理请求数=%d，预期%d", requests.Load(), expectedRequests)
	}
	t.Logf("真实Host会话与回收通过，Host假TLS对话=%t，后台请求=%d；未验收真实供应商", dialogue, backgroundRequests.Load())
}

// writeCLIText 按实际CLI协议发送Host随机无密钥SSE文本，不冒充真实模型服务。
func writeCLIText(w io.Writer, agent, reply string) {
	if agent == "claude" {
		writeCLIEvent(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_host", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 0}}})
		writeCLIEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		writeCLIEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": reply}})
		writeCLIEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeCLIEvent(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 3}})
		writeCLIEvent(w, "message_stop", map[string]any{"type": "message_stop"})
		return
	}
	item := map[string]any{"id": "msg_host", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": reply, "annotations": []any{}}}}
	writeCLIEvent(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_host", "status": "in_progress", "output": []any{}}})
	writeCLIEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	writeCLIEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_host", "object": "response", "status": "completed", "model": "gpt-5.4", "output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 3, "total_tokens": 13}}})
}

// writeCLIEvent 编码测试SSE事件，固定测试数据编码错误直接使测试失败。
func writeCLIEvent(w io.Writer, name string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
}
