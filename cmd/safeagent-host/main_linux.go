// safeagent-host 从显式宿主配置启动真实隔离CLI会话，密钥仅由Host Gateway使用。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"safeagent/internal/host/console"
	"safeagent/internal/host/credential"
	"safeagent/internal/host/gateway"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/mcp"
	"safeagent/internal/host/session"
	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/host/workspace"
	"safeagent/internal/protocol"
	"safeagent/internal/proxywire"
	"safeagent/internal/workspacebundle"
)

// main 在所有资源清理完成后映射真实Agent退出码。
func main() {
	err := run()
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, err)
	var exit *console.ExitError
	if errors.As(err, &exit) && exit.Code > 0 && exit.Code < 256 {
		os.Exit(exit.Code)
	}
	os.Exit(1)
}

// run 装配本次独占VM与内存凭证，不读取既有登录文件或环境密钥。
func run() (result error) {
	configPath := flag.String("config", "", "宿主启动JSON配置路径（不含密钥）")
	keyFD := flag.Int("key-fd", 0, "专用API Key输入FD，默认stdin；终端输入关闭回显")
	reviewExport := flag.Bool("review-export", true, "退出Agent后审核并显式确认导出")
	resumeReview := flag.String("resume-review", "", "恢复Host保留的审核ID，不启动VM或读取密钥")
	flag.Parse()
	if *configPath == "" || flag.NArg() != 0 || *keyFD < 0 {
		return fmt.Errorf("用法: safeagent-host --config /绝对路径/session.json [--key-fd 3]")
	}
	config, content, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *resumeReview != "" {
		snapshots, err := workspace.NewStore(filepath.Join(config.DataRoot, "workspace"), workspacebundle.GuestLimits())
		if err != nil {
			return err
		}
		review, err := snapshots.LoadReview(ctx, *resumeReview)
		if err != nil {
			return err
		}
		return reviewWorkspace(ctx, review)
	}
	credentials := credential.New()
	defer func() { result = errors.Join(result, credentials.Close()) }()
	// Gateway先验证源站与网络授权，失败时不提示或读取密钥。
	service, err := gateway.CLIService(config.Agent, config.Origin, "session-key", config.AllowedNetworks)
	if err != nil {
		return err
	}
	// 模型静默思考按请求总预算等待；物理帧仍由proxywire限制完整传输时间。
	services := []gateway.Service{service}
	refs := []string{"model"}
	mcpRefs := []string{}
	for _, connection := range config.MCP {
		ref := "mcp:" + connection.Ref
		service, err := mcp.NewService(mcp.Spec{Ref: connection.Ref, UpstreamURL: connection.URL, AllowedNetworks: connection.AllowedNetworks, CredentialRef: ref, AllowedTools: connection.AllowedTools})
		if err != nil {
			return err
		}
		services = append(services, service)
		refs = append(refs, ref)
		mcpRefs = append(mcpRefs, connection.Ref)
	}
	generated, err := mcp.GuestFiles(config.Agent, config.Model, mcpRefs)
	if err != nil {
		return err
	}
	modelGateway, err := gateway.New(gateway.Config{Services: services, MaxBindings: 1, MaxConcurrent: 1, MaxRequestBytes: 1 << 20, MaxResponseBytes: 16 << 20, MaxHeaderBytes: 16 << 10, TotalTimeout: proxywire.Total, IdleTimeout: proxywire.Total}, credentials, net.DefaultResolver)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, modelGateway.Close()) }()
	if err = readCredential(ctx, credentials, *keyFD); err != nil {
		return err
	}
	for _, connection := range config.MCP {
		if connection.KeyFD < 0 {
			return fmt.Errorf("MCP Key FD无效")
		}
		if err = readCredentialRef(ctx, credentials, connection.KeyFD, "mcp:"+connection.Ref); err != nil {
			return err
		}
	}
	if err = os.MkdirAll(config.DataRoot, 0700); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(config.DataRoot)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0700 || rootInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("宿主资源目录必须为当前用户拥有的0700目录")
	}
	driver, err := qemu.NewDriver(config.QEMU, config.QEMUImg, filepath.Join(config.DataRoot, "vms"))
	if err != nil {
		return err
	}
	manager, err := vm.NewManager(ctx, filepath.Join(config.DataRoot, "vms.sqlite"), "qemu", driver)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, manager.Close()) }()
	snapshots, err := workspace.NewStore(filepath.Join(config.DataRoot, "workspace"), workspacebundle.GuestLimits())
	if err != nil {
		return err
	}
	id, err := protocol.NewUUID()
	if err != nil {
		return err
	}
	vmConfig := vm.VMConfig{ID: string(id), Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: config.Accelerator, VCPUs: config.VCPUs, MemoryMiB: config.MemoryMiB, Image: config.Image}
	sessionConfig := session.Config{Backend: manager, Driver: driver, Workspace: snapshots, Gateway: modelGateway, VM: vmConfig, Selection: workspace.ImportSelection{Root: config.Project, Paths: config.Files, Generated: generated}, Content: content, ServiceRefs: refs, Rows: 24, Cols: 80, StartupTimeout: 2 * time.Minute}
	sessionConfig.OnReviewSaved = func(id string) {
		fmt.Fprintln(os.Stderr, "Host已保留工作区审核:", id, "；恢复命令: safeagent-host --config", *configPath, "--resume-review", id)
	}
	if *reviewExport {
		sessionConfig.Review = reviewWorkspace
	}
	return session.Run(ctx, sessionConfig, func(ctx context.Context, client guest.Client, info protocol.SessionInfo) error {
		return console.Interact(ctx, client, info, os.Stdin, os.Stdout)
	})
}

// readCredential 将专用FD或隐藏终端输入存入Host内存，密钥不进入启动参数。
func readCredential(ctx context.Context, store *credential.Store, fd int) error {
	return readCredentialRef(ctx, store, fd, "session-key")
}

// readCredentialRef 将指定服务的专用输入保存至Host内存。
func readCredentialRef(ctx context.Context, store *credential.Store, fd int, ref string) error {
	input := os.Stdin
	if fd != 0 {
		input = os.NewFile(uintptr(fd), "host-api-key")
		if input == nil {
			return fmt.Errorf("API Key输入FD无效")
		}
		defer input.Close()
	}
	secret, err := console.ReadSecret(ctx, input, os.Stderr)
	if err != nil {
		return err
	}
	return store.Put(ctx, ref, secret)
}
