// safeagent-web 将可信镜像配置装配为仅本机可访问的真实 VM 工作台。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"safeagent/frontend"
	"safeagent/internal/host/api"
	"safeagent/internal/host/credential"
	"safeagent/internal/host/gateway"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/mcp"
	"safeagent/internal/host/session"
	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/host/workspace"
	"safeagent/internal/protocol"
	"safeagent/internal/workspacebundle"
)

// main 报告不包含浏览器凭据的启动错误。
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run 在随机 loopback 端口发布带进程随机认证的本地页面。
func run() error {
	codex := flag.String("codex-config", "", "可信 Codex 启动配置，不含密钥")
	claude := flag.String("claude-config", "", "可信 Claude 启动配置，不含密钥")
	flag.Parse()
	configs := map[string]launchConfig{}
	contents := map[string]protocol.Preparation{}
	for agent, path := range map[string]string{"codex": *codex, "claude": *claude} {
		if path == "" {
			continue
		}
		c, p, err := loadConfig(path)
		if err != nil {
			return err
		}
		if c.Agent != agent {
			return fmt.Errorf("配置Agent不匹配")
		}
		configs[agent] = c
		contents[agent] = p
	}
	if len(configs) == 0 {
		return fmt.Errorf("至少提供 --codex-config 或 --claude-config")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	app, err := api.New("http://"+listener.Addr().String(), func(ctx context.Context, req api.Request, interact func(api.Terminal) error, review func(api.Review, func(context.Context) error) error) error {
		c, ok := configs[req.Agent]
		if !ok {
			return fmt.Errorf("Agent镜像未登记")
		}
		return launch(ctx, c, contents[req.Agent], req, interact, review)
	})
	if err != nil {
		return err
	}
	defer app.Close()
	githubID, githubSecret, githubRedirect := os.Getenv("SAFEAGENT_GITHUB_CLIENT_ID"), os.Getenv("SAFEAGENT_GITHUB_CLIENT_SECRET"), os.Getenv("SAFEAGENT_GITHUB_REDIRECT_URL")
	if githubID != "" || githubSecret != "" || githubRedirect != "" {
		if err := app.ConfigureGitHub(api.GitHubConfig{ClientID: githubID, ClientSecret: githubSecret, RedirectURL: githubRedirect}); err != nil {
			return err
		}
	}
	page, err := frontend.Files.ReadFile("web/index.html")
	if err != nil {
		return err
	}
	assets := map[string][]byte{}
	for _, name := range []string{"app.js", "style.css", "xterm.js", "xterm.css", "addon-fit.js"} {
		data, err := frontend.Files.ReadFile("web/" + name)
		if err != nil {
			return err
		}
		assets["/assets/"+name] = data
	}
	server := &http.Server{Handler: app.Handler(page, assets), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		app.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	fmt.Println("在本机浏览器打开（链接包含本次访问令牌，请勿转发）：\n" + app.URL())
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// launch 为浏览器确认的项目建立独占资源，真实密钥只进入 Host 凭据存储。
func launch(ctx context.Context, c launchConfig, content protocol.Preparation, req api.Request, interact func(api.Terminal) error, review func(api.Review, func(context.Context) error) error) (result error) {
	credentials := credential.New()
	defer credentials.Close()
	service, err := gateway.CLIService(req.Agent, req.Origin, "session-key", c.AllowedNetworks)
	if err != nil {
		return err
	}
	services := []gateway.Service{service}
	refs := []string{"model"}
	mcpRefs := []string{}
	for i, connection := range req.MCP {
		ref := "mcp:" + connection.Ref
		service, err := mcp.NewService(mcp.Spec{Ref: connection.Ref, UpstreamURL: connection.URL, AllowedNetworks: c.AllowedNetworks, CredentialRef: ref, AllowedTools: connection.AllowedTools})
		if err != nil {
			return err
		}
		if err = credentials.Put(ctx, ref, connection.Key); err != nil {
			return err
		}
		req.MCP[i].Key = ""
		services = append(services, service)
		refs = append(refs, ref)
		mcpRefs = append(mcpRefs, connection.Ref)
	}
	generated, err := mcp.GuestFiles(req.Agent, req.Model, mcpRefs)
	if err != nil {
		return err
	}
	modelGateway, err := gateway.New(gateway.Config{Services: services, MaxBindings: 1, MaxConcurrent: 1, MaxRequestBytes: 1 << 20, MaxResponseBytes: 16 << 20, MaxHeaderBytes: 16 << 10, TotalTimeout: 0, IdleTimeout: 5 * time.Minute}, credentials, net.DefaultResolver)
	if err != nil {
		return err
	}
	defer modelGateway.Close()
	if err = credentials.Put(ctx, "session-key", req.Key); err != nil {
		return err
	}
	req.Key = ""
	if err = os.MkdirAll(c.DataRoot, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(c.DataRoot)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("资源目录必须为当前用户的0700目录")
	}
	driver, err := qemu.NewDriver(c.QEMU, c.QEMUImg, filepath.Join(c.DataRoot, "vms"))
	if err != nil {
		return err
	}
	manager, err := vm.NewManager(ctx, filepath.Join(c.DataRoot, "vms.sqlite"), "qemu", driver)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, manager.Close()) }()
	snapshots, err := workspace.NewStore(filepath.Join(c.DataRoot, "workspace"), workspacebundle.GuestLimits())
	if err != nil {
		return err
	}
	id, err := protocol.NewUUID()
	if err != nil {
		return err
	}
	config := session.Config{Backend: manager, Driver: driver, Workspace: snapshots, Gateway: modelGateway, VM: vm.VMConfig{ID: string(id), Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: c.Accelerator, VCPUs: c.VCPUs, MemoryMiB: c.MemoryMiB, Image: c.Image}, Selection: workspace.ImportSelection{Root: req.Project, Paths: req.Files, Generated: generated}, Content: content, ServiceRefs: refs, Rows: 32, Cols: 100, StartupTimeout: 2 * time.Minute, RetainVM: true}
	config.Review = func(ctx context.Context, r *workspace.Review) error {
		return review(api.Review{ID: r.ID(), Changes: len(r.Changes()), Diff: r.Diff()}, func(ctx context.Context) error { return r.Apply(ctx, r.ID()) })
	}
	return session.Run(ctx, config, func(ctx context.Context, client guest.Client, info protocol.SessionInfo) error {
		term, err := client.AttachTerminal(ctx, info.SessionID)
		if err != nil {
			return err
		}
		defer term.Close()
		return interact(term)
	})
}
