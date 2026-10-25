//go:build linux

package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
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

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"safeagent/internal/host/credential"
	"safeagent/internal/host/gateway"
	"safeagent/internal/host/guest"
	hostmcp "safeagent/internal/host/mcp"
	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/host/workspace"
	"safeagent/internal/protocol"
	"safeagent/internal/proxywire"
	"safeagent/internal/workspacebundle"
)

// TestRealCodexMCP 验证生产 Manifest 内原生 Codex 在无网卡 TCG VM 中发现并执行远程只读 MCP 工具。
func TestRealCodexMCP(t *testing.T) {
	if os.Getenv("SAFEAGENT_GUEST_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实VM MCP验收")
	}
	image, assets, tools := os.Getenv("SAFEAGENT_REAL_CLI_IMAGE"), os.Getenv("SAFEAGENT_VM_ASSETS"), os.Getenv("SAFEAGENT_QEMU_TOOLS")
	if image == "" || assets == "" || tools == "" {
		t.Fatal("必须明确指定镜像、素材和QEMU目录")
	}
	data, err := os.ReadFile(filepath.Join(image, "content.json"))
	if err != nil {
		t.Fatal(err)
	}
	var content protocol.Preparation
	if err = json.Unmarshal(data, &content); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(content.ManifestRef), "codex@") {
		t.Fatal("本验收必须使用生产Codex镜像")
	}
	root, err := os.MkdirTemp("/tmp", "samcp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	driver, err := qemu.NewDriver(filepath.Join(tools, "qemu-system-x86_64"), filepath.Join(tools, "qemu-img"), filepath.Join(root, "vms"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := vm.NewManager(ctx, filepath.Join(root, "state.db"), "qemu", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	source := filepath.Join(root, "project")
	if err = os.MkdirAll(filepath.Join(source, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	generated, err := hostmcp.GuestFiles("codex", "", []string{"test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, ".codex/config.toml"), generated[".codex/config.toml"], 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "README.md"), []byte("isolated MCP validation"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := workspace.NewStore(filepath.Join(root, "snapshots"), workspacebundle.GuestLimits())
	if err != nil {
		t.Fatal(err)
	}
	secrets := credential.New()
	defer secrets.Close()
	const fakeKey = "host-only-mcp-validation-token"
	if err = secrets.Put(ctx, "test", fakeKey); err != nil {
		t.Fatal(err)
	}
	var random [16]byte
	rand.Read(random[:])
	nonce := "MCP_" + hex.EncodeToString(random[:])
	var calls, denied, models atomic.Int64
	mcpServer := sdk.NewServer(&sdk.Implementation{Name: "reproducible-remote", Version: "1"}, nil)
	mcpServer.AddTool(&sdk.Tool{Name: "read_status", Description: "Read a fixed test status", InputSchema: map[string]any{"type": "object"}, Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls.Add(1)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: nonce}}}, nil
	})
	mcpServer.AddTool(&sdk.Tool{Name: "delete_all", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		denied.Add(1)
		return &sdk.CallToolResult{}, nil
	})
	mcpHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return mcpServer }, &sdk.StreamableHTTPOptions{JSONResponse: false})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			t.Error("认证未由Host注入")
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/remote/mcp" {
			mcpHandler.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			t.Error(err)
			return
		}
		if bytes.Contains(body, []byte(fakeKey)) {
			t.Error("实际token出现在Guest请求体")
			return
		}
		var input map[string]any
		if json.Unmarshal(body, &input) != nil {
			t.Error("无效模型输入")
			return
		}
		models.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Load() > 0 {
			if !bytes.Contains(body, []byte(nonce)) {
				t.Error("MCP结果未返回模型")
				return
			}
			writeCLIText(w, "codex", nonce)
			return
		}
		toolName := ""
		tools, _ := input["tools"].([]any)
		for _, entry := range tools {
			tool, _ := entry.(map[string]any)
			name, _ := tool["name"].(string)
			if strings.Contains(name, "delete_all") {
				t.Error("禁止工具暴露给CLI")
			}
			if strings.Contains(name, "read_status") {
				toolName = name
			}
		}
		if toolName == "" {
			t.Errorf("Codex未发现MCP工具，工具=%v", tools)
			writeCLIText(w, "codex", "MCP_TOOL_MISSING")
			return
		}
		item := map[string]any{"id": "fc_mcp", "type": "function_call", "call_id": "call_mcp", "name": toolName, "arguments": "{}", "status": "completed"}
		writeCLIEvent(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_mcp", "status": "in_progress", "output": []any{}}})
		writeCLIEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		writeCLIEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_mcp", "object": "response", "status": "completed", "model": "gpt-5.4", "output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 3, "total_tokens": 13}}})
	}))
	defer upstream.Close()
	networks := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	model, err := gateway.CLIService("codex", upstream.URL, "test", networks)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := hostmcp.NewService(hostmcp.Spec{Ref: "test", UpstreamURL: upstream.URL + "/remote/mcp", CredentialRef: "test", AllowedNetworks: networks, AllowedTools: []string{"read_status"}})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	g, err := gateway.New(gateway.Config{Services: []gateway.Service{model, remote}, MaxBindings: 1, MaxConcurrent: 1, MaxRequestBytes: 1 << 20, MaxResponseBytes: 8 << 20, MaxHeaderBytes: 16 << 10, TotalTimeout: proxywire.Total, IdleTimeout: proxywire.Total, RootCAs: roots}, secrets, net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	config := Config{Backend: manager, Driver: driver, Workspace: snapshots, Gateway: g, VM: vm.VMConfig{ID: "mcp-real-cli", Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: vm.TCG, VCPUs: 2, MemoryMiB: 2048, Image: vm.Image{KernelPath: filepath.Join(assets, "guest-kernel/boot/vmlinuz-6.8.0-147-generic"), InitrdPath: filepath.Join(image, "initramfs.cpio.gz"), BaseDiskPath: filepath.Join(assets, "workspace-base.raw")}}, Selection: workspace.ImportSelection{Root: source, Paths: []string{"README.md", ".codex/config.toml"}}, Content: content, ServiceRefs: []string{"model", "mcp:test"}, Rows: 40, Cols: 120, StartupTimeout: 2 * time.Minute}
	err = Run(ctx, config, func(ctx context.Context, client guest.Client, info protocol.SessionInfo) error {
		terminal, err := client.AttachTerminal(ctx, info.SessionID)
		if err != nil {
			return err
		}
		defer terminal.Close()
		stop := context.AfterFunc(ctx, func() { terminal.Close() })
		defer stop()
		var output bytes.Buffer
		buffer := make([]byte, 16384)
		steps := make(map[string]bool)
		cursor, sent, pending := false, false, false
		escape := regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
		for {
			n, readErr := terminal.Read(buffer)
			output.Write(buffer[:n])
			if output.Len() > 1<<20 {
				return fmt.Errorf("终端输出超过预算")
			}
			if bytes.Contains(output.Bytes(), []byte(fakeKey)) {
				return fmt.Errorf("Host token出现在Guest")
			}
			if !cursor && bytes.Contains(output.Bytes(), []byte("\x1b[6n")) {
				cursor = true
				terminal.Write([]byte("\x1b[1;1R"))
			}
			visible := strings.Join(strings.Fields(escape.ReplaceAllString(output.String(), " ")), " ")
			for _, step := range []string{"Welcome to Codex", "Do you trust the contents", "Press enter to continue"} {
				if strings.Contains(visible, step) && !steps[step] {
					steps[step] = true
					terminal.Write([]byte("\r"))
				}
			}
			if !sent && strings.Contains(visible, "model: gpt-5.4") && !strings.Contains(visible, "MCP startup failed") {
				sent = true
				pending = true
				terminal.Write([]byte("Call the read_status MCP tool and report its result."))
			}
			if pending && strings.Contains(visible, "Call the read_status MCP tool and report its result.") {
				pending = false
				terminal.Write([]byte("\r"))
			}
			if strings.Contains(visible, "Would you like to run") && !steps["approval"] {
				steps["approval"] = true
				terminal.Write([]byte("\r"))
			}
			if calls.Load() == 1 && models.Load() >= 2 && strings.Contains(visible, nonce) {
				t.Logf("真实生产Codex MCP完成: allowed=%d denied=%d model_requests=%d", calls.Load(), denied.Load(), models.Load())
				return nil
			}
			if strings.Contains(visible, "MCP_TOOL_MISSING") || strings.Contains(visible, "MCP startup failed") || readErr != nil {
				t.Logf("MCP原生终端=%s", visible)
				return fmt.Errorf("原生MCP未完成: %w", readErr)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || denied.Load() != 0 {
		t.Fatalf("执行计数不符:%d %d", calls.Load(), denied.Load())
	}
	t.Log("Linux/amd64 TCG生产VM＋原生Codex＋Host Gateway＋可复现远端TLS MCP通过；模型为Host假SSE，不代表真实供应商工具决策")
}
