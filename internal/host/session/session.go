// Package session 将现有VM、工作区导入、模型代理与Guest终端装配为一次宿主会话。
package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"safeagent/internal/host/gateway"
	"safeagent/internal/host/guest"
	"safeagent/internal/host/guest/rpc"
	"safeagent/internal/host/proxy"
	"safeagent/internal/host/vm"
	"safeagent/internal/host/vm/qemu"
	"safeagent/internal/host/workspace"
	"safeagent/internal/host/workspaceimport"
	"safeagent/internal/protocol"
)

// Config 保存可信宿主入口提供的具体模块与显式会话选择，不传入真实密钥。
type Config struct {
	// Backend 管理本会话独占的VM资源，调用方仍拥有其长期存储。
	Backend vm.VMBackend
	// Driver 交付本次真实QEMU启动的独立物理通道。
	Driver *qemu.Driver
	// Workspace 保存明确选择文件的不可变快照。
	Workspace *workspace.Store
	// Gateway 在Host注入凭证，生命周期由调用方拥有。
	Gateway *gateway.Gateway
	// VM 是本次独占VM配置，ID不能与其他会话共用。
	VM vm.VMConfig
	// Selection 仅包含用户确认的文件，不自动扩展源目录。
	Selection workspace.ImportSelection
	// Content 来自所选可信镜像的构建登记。
	Content protocol.Preparation
	// ServiceRefs 限定本VM授权的宿主模型服务。
	ServiceRefs []string
	// Rows 指定PTY初始行数。
	Rows protocol.Dimension
	// Cols 指定PTY初始列数。
	Cols protocol.Dimension
	// Review 在进程组停止且变更持久冻结后执行用户审核，nil仍保存待恢复记录。
	Review func(context.Context, *workspace.Review) error
	// OnReviewSaved 通知调用方冻结变更集ID，便于退出后恢复审核。
	OnReviewSaved func(string)
	// StartupTimeout 限制快照、启动、握手和导入准备的总时长。
	StartupTimeout time.Duration
	// RetainVM 表示会话结束后停止但保留VM资源，等待用户后续明确销毁。
	RetainVM bool
}

// Run 启动一次真实会话，只有交互回调明确结束生命周期后才停止代理并处理VM。
func Run(ctx context.Context, c Config, interact func(context.Context, guest.Client, protocol.SessionInfo) error) (result error) {
	if err := protocol.Validate(c.Content); err != nil {
		return err
	}
	if err := c.VM.Validate(); err != nil {
		return err
	}
	if c.StartupTimeout <= 0 {
		return fmt.Errorf("启动预算必须为正")
	}
	if err := protocol.Validate(c.Rows); err != nil {
		return err
	}
	if err := protocol.Validate(c.Cols); err != nil {
		return err
	}
	startup, stopStartup := context.WithTimeout(ctx, c.StartupTimeout)
	defer stopStartup()
	baseline, err := c.Workspace.Stage(startup, c.Selection)
	if err != nil {
		return fmt.Errorf("创建工作区快照: %w", err)
	}
	ref := vm.VMRef{ID: c.VM.ID}
	var instance vm.VMInstance
	transferred := false
	owned := true
	startAttempted := false
	cleanup := func(clean context.Context) error {
		var cleanupErr error
		// Start可能在启动进程后持久化失败，错误返回的零实例不能证明VM未运行。
		if startAttempted && instance.Ref.ID == "" {
			status, inspectErr := c.Backend.Inspect(clean, ref)
			if inspectErr != nil {
				return fmt.Errorf("查询启动失败后的VM: %w", inspectErr)
			}
			if status.State != vm.StateCreated && status.State != vm.StateStopped {
				instance = status.Instance
			}
		}
		if instance.Ref.ID != "" {
			cleanupErr = c.Backend.Stop(clean, instance, vm.StopOptions{GracePeriod: time.Second, ForceAfterTimeout: true})
			_, waitErr := c.Backend.Wait(clean, instance)
			cleanupErr = errors.Join(cleanupErr, waitErr)
		}
		if c.RetainVM {
			return cleanupErr
		}
		return errors.Join(cleanupErr, c.Backend.Destroy(clean, ref))
	}
	defer func() {
		if owned && !transferred {
			clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			result = errors.Join(result, cleanup(clean))
		}
	}()
	if _, err = c.Backend.Create(startup, c.VM); err != nil {
		// 配置冲突明确属于其他资源，不能把创建失败当作清理它的授权。
		owned = !errors.Is(err, vm.ErrConflict)
		return fmt.Errorf("创建VM: %w", err)
	}
	startAttempted = true
	instance, err = c.Backend.Start(startup, ref)
	if err != nil {
		return fmt.Errorf("启动VM: %w", err)
	}
	channels, err := c.Driver.OpenGuestChannels(startup, instance)
	if err != nil {
		return fmt.Errorf("打开Guest通道: %w", err)
	}
	client, err := rpc.New(instance, channels.Control, channels.OpenTerminal)
	if err != nil {
		channels.Control.Close()
		return err
	}
	defer client.Close()
	if _, err = client.Hello(startup); err != nil {
		return fmt.Errorf("Guest握手: %w", err)
	}
	imported, err := workspaceimport.Import(startup, client, channels, c.Workspace, baseline.ID)
	if err != nil {
		return fmt.Errorf("导入工作区: %w", err)
	}
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	ready := make(chan struct{})
	done := make(chan struct{})
	var proxyErr error
	// 从这里开始由proxy.Run唯一拥有撤销和VM回收，外层不再执行cleanup。
	transferred = true
	go func() {
		proxyErr = proxy.Run(life, client, channels, c.Gateway, gateway.BindingSpec{ServiceRefs: c.ServiceRefs}, func() { close(ready) }, cleanup)
		// 代理故障或VM退出必须解除终端等待，不能只等待用户主动退出。
		cancel()
		close(done)
	}()
	defer func() {
		cancel()
		<-done
		if result == nil && ctx.Err() != nil {
			result = ctx.Err()
		}
		result = errors.Join(result, proxyErr)
	}()
	select {
	case <-ready:
	case <-done:
		return fmt.Errorf("代理就绪失败: %w", proxyErr)
	case <-startup.Done():
		return startup.Err()
	}
	operation, err := protocol.NewUUID()
	if err != nil {
		return err
	}
	prepared, err := client.PrepareAgent(startup, protocol.PrepareAgentParams{OperationID: operation, ManifestRef: c.Content.ManifestRef, ManifestDigest: c.Content.ManifestDigest, ConfigDigest: c.Content.ConfigDigest, WorkspaceRef: imported.WorkspaceRef, WorkspaceDigest: imported.WorkspaceDigest})
	if err != nil {
		return fmt.Errorf("准备Agent: %w", err)
	}
	operation, err = protocol.NewUUID()
	if err != nil {
		return err
	}
	sessionID, err := protocol.NewUUID()
	if err != nil {
		return err
	}
	info, err := client.StartSession(startup, protocol.StartSessionParams{OperationID: operation, SessionID: protocol.OpaqueID(sessionID), PreparedID: prepared.PreparedID, WorkspaceRef: imported.WorkspaceRef, WorkspaceDigest: imported.WorkspaceDigest, Rows: c.Rows, Cols: c.Cols})
	if err != nil {
		return fmt.Errorf("启动Agent: %w", err)
	}
	stopStartup()
	interactionErr := interact(life, client, info)
	if life.Err() != nil {
		return interactionErr
	}
	// 先清空进程组，确保审核读取期间Agent不再执行写入。
	stopCtx, stopExport := context.WithTimeout(life, 30*time.Second)
	defer stopExport()
	if err := client.StopSession(stopCtx, protocol.StopSessionParams{SessionID: info.SessionID, GraceMs: 1000, ForceAfterTimeout: true}); err != nil {
		return errors.Join(interactionErr, err)
	}
	review, err := c.Workspace.CollectReview(life, baseline.ID, client)
	if err != nil {
		return errors.Join(interactionErr, fmt.Errorf("冻结工作区变更: %w", err))
	}
	if c.OnReviewSaved != nil {
		c.OnReviewSaved(review.ID())
	}
	if c.Review != nil {
		return errors.Join(interactionErr, c.Review(life, review))
	}
	return interactionErr
}
