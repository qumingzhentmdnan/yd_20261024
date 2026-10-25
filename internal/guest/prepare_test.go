package guest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"safeagent/agents"
	"safeagent/internal/guest/prepare"
	"safeagent/internal/manifest"
	"safeagent/internal/protocol"
	"testing"
)

// TestManifestPolicyAndWorkspace 验证清单模式拒绝旧配置摘要和错误工作区。
func TestManifestPolicyAndWorkspace(t *testing.T) {
	catalog, err := manifest.NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := catalog.List()[0]
	config := Config{Catalog: catalog, Content: protocol.Preparation{PreparedID: "registered", ManifestRef: protocol.OpaqueID(descriptor.Ref), ManifestDigest: protocol.Digest(descriptor.ManifestDigest), ConfigDigest: protocol.Digest(prepare.ConfigDigest()), WorkspaceRef: "imported", WorkspaceDigest: protocol.Digest(descriptor.ManifestDigest)}, Build: "test", ImageDigest: protocol.Digest(descriptor.ManifestDigest)}
	bad := config
	bad.Content.ConfigDigest = bad.Content.ManifestDigest
	if _, err := New(bad); err == nil {
		t.Fatal("旧配置摘要被接受")
	}
	boot, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.cancel()
	connection := &connection{boot: boot}
	op, err := protocol.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	params := protocol.PrepareAgentParams{OperationID: op, ManifestRef: config.Content.ManifestRef, ManifestDigest: config.Content.ManifestDigest, ConfigDigest: config.Content.ConfigDigest, WorkspaceRef: "wrong", WorkspaceDigest: config.Content.WorkspaceDigest}
	_, err = connection.operate(context.Background(), protocol.MethodPrepareAgent, params)
	var failure *protocol.Error
	if !errors.As(err, &failure) || failure.Code != protocol.CodeFailedPrecondition {
		t.Fatal(err)
	}
	if len(boot.preparations) != 0 || len(boot.executions) != 0 || boot.reserved {
		t.Fatal("拒绝操作留下可启动结果")
	}
}

// TestManifestCommitLedger 验证真实清单渲染接入账本后，取消结果与发布边界一致且重放不重复安装。
func TestManifestCommitLedger(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			catalog, err := manifest.NewCatalog()
			if err != nil {
				t.Fatal(err)
			}
			entry := catalog.List()[0]
			boot, err := New(Config{Catalog: catalog, Content: protocol.Preparation{PreparedID: "registered", ManifestRef: protocol.OpaqueID(entry.Ref), ManifestDigest: protocol.Digest(entry.ManifestDigest), ConfigDigest: protocol.Digest(prepare.ConfigDigest()), WorkspaceRef: "workspace", WorkspaceDigest: protocol.Digest(entry.ManifestDigest)}, Build: "test", ImageDigest: protocol.Digest(entry.ManifestDigest)})
			if err != nil {
				t.Fatal(err)
			}
			defer boot.cancel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root := t.TempDir()
			executable := filepath.Join(root, "probe")
			data, err := fs.ReadFile(agents.Builtins(), "probe/probe.sh")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(executable, data, 0755); err != nil {
				t.Fatal(err)
			}
			calls := 0
			var home string
			boot.installPreparation = func(ctx context.Context, plan manifest.Plan) error {
				calls++
				// 保留真实清单内容和摘要，只把资源边界映射到测试独占目录。
				home = filepath.Join(root, filepath.Base(plan.Home))
				plan.Home = home
				plan.Executable = executable
				if !after {
					cancel()
				}
				err := prepare.Install(ctx, plan, os.Getuid(), os.Getgid())
				if after {
					cancel()
				}
				return err
			}
			connection := &connection{boot: boot}
			id, err := protocol.NewUUID()
			if err != nil {
				t.Fatal(err)
			}
			params := protocol.PrepareAgentParams{OperationID: id, ManifestRef: boot.config.Content.ManifestRef, ManifestDigest: boot.config.Content.ManifestDigest, ConfigDigest: boot.config.Content.ConfigDigest, WorkspaceRef: boot.config.Content.WorkspaceRef, WorkspaceDigest: boot.config.Content.WorkspaceDigest}
			result, err := connection.operate(ctx, protocol.MethodPrepareAgent, params)
			if after {
				if err != nil {
					t.Fatal(err)
				}
				prepared := result.(protocol.Preparation)
				if boot.prepared == nil || boot.prepared.PreparedID != prepared.PreparedID || boot.executions[prepared.PreparedID].Path == "" {
					t.Fatal("发布后取消未登记")
				}
				if _, err := os.Stat(filepath.Join(home, ".config/probe/settings.json")); err != nil {
					t.Fatal(err)
				}
				// 模拟成功响应丢失后用相同操作重放。
				replay, err := connection.operate(context.Background(), protocol.MethodPrepareAgent, params)
				if err != nil || replay.(protocol.Preparation).PreparedID != prepared.PreparedID {
					t.Fatal("成功重放改变结果", err)
				}
			} else {
				var failure *protocol.Error
				if !errors.As(err, &failure) || failure.Code != protocol.CodeCancelled {
					t.Fatal(err)
				}
				if boot.prepared != nil || len(boot.executions) != 0 {
					t.Fatal("取消留下可启动准备")
				}
				if _, err := os.Stat(home); !os.IsNotExist(err) {
					t.Fatal("取消留下HOME")
				}
				if _, err := connection.operate(context.Background(), protocol.MethodPrepareAgent, params); err == nil {
					t.Fatal("失败重放变成功")
				}
			}
			if calls != 1 {
				t.Fatal("重放重复安装", calls)
			}
		})
	}
}
