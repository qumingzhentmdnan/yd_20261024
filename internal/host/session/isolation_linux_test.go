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
	"os/exec"
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

// TestRealCLIProductionIsolation 在未改启动参数的生产 CLI 中执行显式导入的验收程序。
func TestRealCLIProductionIsolation(t *testing.T) {
	runRealCLIProductionIsolation(t, false)
}

// TestRealCLIUnauthorizedRoute 验证真实 Guest 的越权模型路径使 Host 拒绝并撤销代理，而非放宽网络。
func TestRealCLIUnauthorizedRoute(t *testing.T) {
	runRealCLIProductionIsolation(t, true)
}

// runRealCLIProductionIsolation 共享生产镜像装配，区分正常隔离检查和预期撤销会话的攻击检查。
func runRealCLIProductionIsolation(t *testing.T, denyRoute bool) {
	t.Helper()
	if os.Getenv("SAFEAGENT_GUEST_INTEGRATION") != "1" {
		t.Skip("需要显式启用真实 VM 验证")
	}
	image, assets, tools := os.Getenv("SAFEAGENT_REAL_CLI_IMAGE"), os.Getenv("SAFEAGENT_VM_ASSETS"), os.Getenv("SAFEAGENT_QEMU_TOOLS")
	agent := os.Getenv("SAFEAGENT_CLI_AGENT")
	if image == "" || assets == "" || tools == "" || (agent != "codex" && agent != "claude") {
		t.Fatal("必须指定真实镜像、素材、工具和 CLI 类型")
	}
	root, err := os.MkdirTemp("/tmp", "sai-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	source := filepath.Join(root, "project")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	// 编译的是独立验收程序，镜像与生产 guest-agent/CLI 均不增加测试命令。
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(source, "guest-isolation"), "./scripts/acceptance/guest-isolation")
	build.Dir = "../../.."
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("编译验收程序: %v: %s", err, output)
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	fakeKey := "FAKE_HOST_" + hex.EncodeToString(nonce[:])
	keySum := sha256.Sum256([]byte(fakeKey))
	hostSecret := filepath.Join(root, "not-imported-secret.txt")
	if err = os.WriteFile(hostSecret, []byte(fakeKey), 0600); err != nil {
		t.Fatal(err)
	}
	configData, err := json.Marshal(map[string]string{"hostSecret": hostSecret, "keyDigest": hex.EncodeToString(keySum[:])})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "acceptance.json"), configData, 0600); err != nil {
		t.Fatal(err)
	}
	var content protocol.Preparation
	data, err := os.ReadFile(filepath.Join(image, "content.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &content); err != nil {
		t.Fatal(err)
	}
	driver, err := qemu.NewDriver(filepath.Join(tools, "qemu-system-x86_64"), filepath.Join(tools, "qemu-img"), filepath.Join(root, "vms"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := vm.NewManager(ctx, filepath.Join(root, "vms.db"), "qemu", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	snapshots, err := workspace.NewStore(filepath.Join(root, "snapshots"), workspacebundle.GuestLimits())
	if err != nil {
		t.Fatal(err)
	}
	secrets := credential.New()
	defer secrets.Close()
	if err = secrets.Put(ctx, "acceptance", fakeKey); err != nil {
		t.Fatal(err)
	}
	var authenticated atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			t.Error(err)
			http.Error(w, "invalid body", 400)
			return
		}
		if bytes.Contains(body, []byte(fakeKey)) {
			t.Error("Guest 模型请求包含宿主假凭据")
		}
		auth := r.Header.Get("Authorization") == "Bearer "+fakeKey
		if agent == "claude" {
			auth = r.Header.Get("X-Api-Key") == fakeKey
		}
		if !auth {
			t.Error("Host 未注入假凭据")
			http.Error(w, "missing auth", 403)
			return
		}
		if r.URL.Path != "/v1/responses" && r.URL.Path != "/v1/messages" {
			t.Error("未授权路径到达上游")
			http.Error(w, "forbidden", 403)
			return
		}
		authenticated.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		writeCLIText(w, agent, "ACCEPTANCE_MODEL_READY")
	}))
	defer upstream.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	service, err := gateway.CLIService(agent, upstream.URL, "acceptance", []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	if err != nil {
		t.Fatal(err)
	}
	g, err := gateway.New(gateway.Config{Services: []gateway.Service{service}, MaxBindings: 1, MaxConcurrent: 1, MaxRequestBytes: 1 << 20, MaxResponseBytes: 16 << 20, MaxHeaderBytes: 16 << 10, TotalTimeout: proxywire.Total, IdleTimeout: proxywire.Total, RootCAs: roots}, secrets, net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	basePath := filepath.Join(assets, "workspace-base.raw")
	before, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Backend: manager, Driver: driver, Workspace: snapshots, Gateway: g, VM: vm.VMConfig{ID: "production-isolation", Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: vm.TCG, VCPUs: 2, MemoryMiB: 2048, Image: vm.Image{KernelPath: filepath.Join(assets, "guest-kernel/boot/vmlinuz-6.8.0-147-generic"), InitrdPath: filepath.Join(image, "initramfs.cpio.gz"), BaseDiskPath: basePath}}, Selection: workspace.ImportSelection{Root: source, Paths: []string{"acceptance.json", "guest-isolation"}}, Content: content, ServiceRefs: []string{"model"}, Rows: 50, Cols: 180, StartupTimeout: 2 * time.Minute}
	readyForDenial := false
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
		escape := regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
		steps := map[string]bool{}
		pending := ""
		conversationSent, commandSent, cursorAnswered := false, false, false
		bangSent := false
		denialSent := false
		for {
			n, readErr := terminal.Read(buffer)
			output.Write(buffer[:n])
			if output.Len() > 2<<20 {
				return fmt.Errorf("验收终端超过 2MiB")
			}
			if bytes.Contains(output.Bytes(), []byte(fakeKey)) {
				return fmt.Errorf("Guest 终端泄露宿主假凭据")
			}
			if !cursorAnswered && bytes.Contains(output.Bytes(), []byte("\x1b[6n")) {
				cursorAnswered = true
				if _, err := terminal.Write([]byte("\x1b[1;1R")); err != nil {
					return err
				}
			}
			visible := escape.ReplaceAllStringFunc(output.String(), func(code string) string {
				if strings.HasSuffix(code, "C") || strings.HasSuffix(code, "H") {
					return " "
				}
				return ""
			})
			visible = strings.Join(strings.Fields(visible), " ")
			for _, step := range []string{"Welcome to Codex", "Choose the text style", "Do you trust the files", "Do you trust this", "Press Enter to continue", "Yes, I trust this folder"} {
				if strings.Contains(visible, step) && !steps[step] {
					steps[step] = true
					if _, err := terminal.Write([]byte("\r")); err != nil {
						return err
					}
				}
			}
			if pending != "" && strings.Contains(visible, pending) {
				if pending == "./guest-isolation deny" {
					readyForDenial = true
				}
				pending = ""
				if _, err := terminal.Write([]byte("\r")); err != nil {
					return err
				}
			}
			if !conversationSent && ((agent == "codex" && strings.Contains(visible, "model: gpt-5.4")) || (agent == "claude" && strings.Contains(visible, "for shortcuts"))) {
				conversationSent = true
				pending = "Reply with the upstream text."
				if _, err := terminal.Write([]byte(pending)); err != nil {
					return err
				}
			}
			if conversationSent && !commandSent && strings.Contains(visible, "ACCEPTANCE_MODEL_READY") {
				if agent == "claude" && !bangSent {
					bangSent = true
					if _, err := terminal.Write([]byte("!")); err != nil {
						return err
					}
					continue
				}
				if agent == "claude" && !strings.Contains(visible, "Bash mode") && !strings.Contains(visible, "bash mode") {
					continue
				}
				commandSent = true
				pending = "./guest-isolation"
				if denyRoute {
					pending += " deny"
				}
				command := pending
				if agent == "codex" {
					command = "!" + command
				}
				if _, err := terminal.Write([]byte(command)); err != nil {
					return err
				}
			}
			if commandSent && strings.Contains(visible, "ISOLATION_PASSED:") {
				if denyRoute && !denialSent {
					denialSent = true
					commandSent = false
					pending = ""
					continue
				}
				t.Logf("生产 CLI 隔离断言: %s", visible)
				return nil
			}
			if denyRoute && denialSent && strings.Contains(visible, "ISOLATION_READY_FOR_DENIAL") {
				readyForDenial = true
			}
			if strings.Contains(visible, "ISOLATION_FAILED:") || readErr != nil {
				t.Logf("隔离验收未完成: %s", visible)
				return fmt.Errorf("隔离验收失败: %v", readErr)
			}
		}
	})
	if denyRoute {
		if !readyForDenial || !errors.Is(err, gateway.ErrForbidden) {
			t.Fatalf("未观察到真实越权路径被 Host 拒绝: ready=%t err=%v", readyForDenial, err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if authenticated.Load() == 0 {
		t.Fatal("尚未验证 Host 假凭据注入")
	}
	if _, err := manager.Inspect(ctx, vm.VMRef{ID: config.VM.ID}); !errors.Is(err, vm.ErrNotFound) {
		t.Fatal("验收 VM 未回收", err)
	}
	after, err := os.ReadFile(basePath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("共享基础盘改变", err)
	}
	t.Log("真实生产镜像与真实 CLI 子进程完成固定负向验收；模型为 Host 假 TLS 服务，未验收真实供应商或其他平台")
}
