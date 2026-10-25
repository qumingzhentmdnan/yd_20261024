# QEMU 后端实现 Agent 提示词

将下文作为任务交给新 Agent。用户负责准备 worktree；基础代码提交为 `5055743`。本文新增后尚未提交，因此新 worktree 如未包含本文，可从原工作区只读访问：`/home/yedong/workspace/sandbox/docs/tasks/qemu-backend-agent.md`。

---

你负责实现 SafeAgent 的真实 QEMU VM 生命周期后端。请在用户为你准备的 worktree 中完成代码、验证和 review，不要只输出设计方案。

## 1. 已完成的基础

基础提交：`5055743`。以下内容已经实现并通过测试，不要重新编写：

- `internal/host/vm/backend.go`：`VMBackend` 接口。
- `internal/host/vm/types.go`、`errors.go`：基础类型、配置校验和错误定义。
- `internal/testkit/fakevm`：带状态、故障注入和闸门的内存 fake。
- `internal/testkit/vmcontract`：真实后端也需运行的共用契约测试。

先阅读当前 worktree 的：

1. `AGENTS.md`。
2. `internal/host/vm` 下全部文件，尤其是接口注释和 README。
3. `internal/testkit/vmcontract/contract.go`。
4. `internal/testkit/fakevm`，理解已验证行为，不直接照搬为生产后端。
5. `docs/validation-2026-10-24.md`。
6. `design/reviews/vm-foundation-summary.md`。
7. `design/design.md` 中 VM 与安全边界部分。

当前契约以实际 Go 定义和共用测试为准，历史设计提案中的旧签名不覆盖它们。

## 2. 本轮范围与文件所有权

新增 `internal/host/vm/qemu/`，在其中实现 `vm.VMBackend`、测试和使用说明。

只负责 VM 私有资源、差分磁盘、QEMU 进程、必要的 QMP 控制及退出观察。先支持已经手工验证的 Linux/amd64/microvm。

不实现 Guest RPC、Agent Runner、PTY 业务协议、模型/MCP Gateway、Vault、前端或项目审核导出。不修改共享接口、fake、共用 suite、根 go.mod/Makefile 或其他 Agent 的代码。若现有契约确实阻塞实现，说明具体场景并提出最小变更，不自行维护第二套接口或通过修改测试绕过约束。

无需引入 SQLite 或单独的 Image 表。后端需要保留创建配置和资源记录；可在 VM 私有目录保存一份配置文件，不扩展成通用持久化框架。Host 服务重启后的运行接管不属于本轮默认承诺，应如实说明支持范围。

## 3. 需要实现的接口

```go
type VMBackend interface {
    Create(context.Context, VMConfig) (VMRef, error)
    Start(context.Context, VMRef) (VMInstance, error)
    Inspect(context.Context, VMRef) (VMStatus, error)
    Stop(context.Context, VMInstance, StopOptions) error
    Wait(context.Context, VMInstance) (VMExit, error)
    Destroy(context.Context, VMRef) error
}
```

使用 `safeagent/internal/host/vm` 中的类型。以下行为已有契约，不另作解释：

- **Create**：只准备私有资源，不启动 QEMU。同 ID 同配置返回已有资源，配置不同返回 `ErrConflict`；并发调用不创建多份资源。不可变基础镜像不被改写。
- **Start**：VMM 启动完成后返回，不等待 Guest Hello。运行中重复调用返回同一实例；真正重启生成新 epoch，包括 Destroy/Create 后也不能复用。成功后取消请求 context 不杀 VM，不能直接让长驻进程受该请求的 `exec.CommandContext` 控制。
- **Inspect**：观察当前 VM 状态，允许 Starting/Stopping；不能将 QMP 断开或查询失败当作进程已经退出。
- **Stop**：仅控制指定实例；存在资源的非当前 epoch 返回 `ErrStaleInstance`。宽限期必须为正数。成功表示 VMM 已退出，保留磁盘；重复停止成功。请求取消或失败后的实际状态要能查询。
- **Wait**：多个等待者获得同一固定退出结果，取消一个等待者不影响其他调用或 VM。非零退出码放在 `VMExit` 中。历史结果保留到 Destroy 成功；无运行记录返回 `ErrNotFound`。
- **Destroy**：Created/Stopped 可销毁，其他状态返回 `ErrInvalidState`。只删除本模块拥有的私有资源，不删除基础镜像；不存在时成功；清理失败保留可重试记录及退出结果。

microvm 不保证 ACPI 关机可用。不支持优雅关机时，`ForceAfterTimeout=false` 返回 `ErrShutdownUnsupported`，不能擅自强停。显式允许强停时可按契约终止。QMP `stop` 只是暂停 vCPU，不能作为关机实现；QMP `quit` 也不能冒充 Guest 优雅关机。

## 4. 实现约束

- 构造时注入 QEMU、qemu-img 路径和受控数据根目录。生产代码不写死原 workspace 路径。
- 使用结构化配置和 argv 启动进程，不使用 `sh -c`，不开放任意用户 QEMU flags。
- 默认无通用网卡，不共享或挂载宿主项目目录，不向 Guest 传真实凭据。
- 加速器必须显式选择。KVM 不可用直接返回错误，禁止自动回退 TCG；不支持的平台直接明确报错。
- 同资源的生命周期变更需要并发协调；等待进程或 QMP 时不长期占用全局锁。
- 进程退出由单一回收路径处理，供多个 Wait 调用观察；异常退出及启动失败都必须释放本次私有句柄和进程。
- QMP 实现限制消息大小，并正确关联响应、区分事件、处理超时和断联；只实现当前生命周期需要的控制命令，不搭建通用 RPC 框架。
- 共享类型只做已有结构校验；真实文件、目录所有权、删除边界和进程资源等必要检查放在实际资源操作处，不重复堆叠校验。
- 当前不冻结 Guest 通道或业务 framing。必要的宿主设备端点保持为后端内部实现，不为未来需求修改共享接口。
- 严格遵守 `AGENTS.md`：中文注释；每个具名函数/方法以名称开头说明；每个结构体字段有说明；较长方法按关键步骤注释；不写无依据的防御性代码、静默兜底或未来兼容层。只有确有可读性收益才使用 lo，不为引包而引包。

## 5. 本机可用环境

原工作区：`/home/yedong/workspace/sandbox`。可在你自己的 worktree 中执行：

```sh
source /home/yedong/workspace/sandbox/artifacts/toolchain/activate.sh
```

该脚本提供 Go 1.27.3、本地 QEMU 8.2.2、qemu-img 和 GCC。工具与缓存位于原工作区的 artifacts，不会自动复制到新 worktree。

本机没有 `/dev/kvm`，已验证的模式为显式 TCG。不要把本机结果描述为 KVM/WHPX/HVF 或跨平台验证。

原工作区中可只读参考：

- `artifacts/validation/start-vm.sh`：实际成功启动参数。
- `artifacts/validation/guest-kernel/boot/vmlinuz-6.8.0-147-generic`：测试内核。
- `artifacts/validation/initramfs.cpio.gz`：临时 BusyBox Guest，不是正式 Agent 镜像。
- `artifacts/validation/workspace-base.raw`：只读测试基础盘。
- `artifacts/validation/rootfs/init`：设备测试所用 init。

不要直接运行原启动脚本作为生产后端，也不要使用原工作区的运行中 overlay/socket。每个测试必须创建自己独立的临时运行目录、overlay 和端点，结束后回收自己的进程；不修改共享基础盘。

## 6. 验证与交付

1. 默认 `go test ./...` 不启动真实 VM、不下载镜像、不访问外网。
2. 用单元测试覆盖参数构造、资源记录及必要错误路径；不要复制实现写无意义测试。
3. 从显式启用的集成测试中调用 `vmcontract.Run`，工厂提供后端与真实本地配置。给出具体启用方式和运行命令。
4. 真实 TCG 集成需验证创建/启动、幂等、并发等待、取消、停止/重启、旧 epoch、销毁和退出回收；确认基础盘未修改、没有遗留本轮 QEMU 进程。
5. 补充真实后端专有的启动失败、QMP 断联和清理部分失败重试验证；不为故障注入扩大公共接口。
6. 运行相关测试、`go vet ./...` 和 `CGO_ENABLED=1 go test -race ./...`。实测与未测分别报告。
7. 完成后按 `AGENTS.md` 调用 codexx 和 Claude CLI 做只读 review，相关讨论和修复复审复用当前 worktree 中各自的 session，处理有效问题。codexx 使用 `gpt-6.1-sol`、medium；记录 Claude CLI 的实际模型。评审进程不要再递归调用其他 reviewer。
8. 不自动暂存、提交、建分支/worktree、合并或回退；这些由用户明确授权。若用户后续要求提交，提交信息使用中文。

最终回复列出：实现文件、接口覆盖情况、验证命令和结果、已知限制、如何构造和使用后端。不把 fake 通过当作真实 VM 通过，也不把 TCG 冒烟当作完整安全沙箱完成。
