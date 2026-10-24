# VM 生命周期开发基线

当前可执行契约以本目录的 backend.go、types.go、errors.go 为准。design 下的接口提案和调研保留背景信息，不覆盖这里的类型与行为。创建方法统一为 Create。

此基线只包含生命周期定义、内存 fake 和共用测试，不包含真实 QEMU 后端、Guest RPC、终端或 Gateway 实现。

## 定义和调用边界

- VMRef 标识持久资源；VMInstance 用 Ref + Epoch 标识一次 VMM 运行。
- Create 验证配置并准备资源；同标识、同配置幂等，不同配置返回 ErrConflict。配置是可比较的值类型，可直接比较。
- Start 运行中返回同一实例，停止后重启生成新 epoch；成功返回后不再受该次请求 context 控制。
- Inspect 返回当前 VM 状态，允许观察 Starting/Stopping，但不保证每次过渡都可见；不能把连接失败当作已停止。当前不定义设备通道接口或业务 framing。
- Stop 使用正数 GracePeriod；只有 ForceAfterTimeout=true 才允许强停。成功代表 VMM 已退出；旧 epoch 返回 ErrStaleInstance。
- Stop 对存在资源的非当前 epoch 返回 ErrStaleInstance；Wait 对没有运行记录的实例返回 ErrNotFound。前者检查控制目标是否仍为当前运行，后者查找历史退出结果，两者用途不同。
- Wait 返回固定退出结果，取消只影响当前等待者；退出码非零不当作查询错误。结果保留到 Destroy 成功。
- Destroy 只删除 Created/Stopped 的私有资源；不删除基础镜像。重复调用成功，失败保留记录供重试。
- 所有方法支持并发调用和 context 取消。context 必须非空；不为内部编程错误增加自动替代值。
- 配置检查、状态校验和 epoch 校验属于已确定的契约。其他校验只在真实输入或资源边界有明确需要时增加。

Go 1.27.3，module 为 safeagent；当前没有第三方依赖。VMConfig.Validate 不访问文件系统，也不证明镜像可信或平台兼容；这些由真实后端和 Host 镜像解析流程负责。加速器必须显式配置，KVM 不可用不能自动回退 TCG。

## Fake

位置：internal/testkit/fakevm。

```go
backend := fakevm.New()
backend.SetGracefulShutdownSupported(false)

gate := fakevm.NewGate()
backend.SetGate(fakevm.OpStart, gate)
// 在测试协程启动操作后，通过 gate.Entered() 等待到达，再由 gate.Release() 放行。

backend.FailNext(fakevm.OpDestroy, errors.New("模拟清理失败"))
// backend.Exit(instance, code, signal) 模拟 VMM 自行退出。
// backend.Stats() 返回成功完成的资源变更次数。
```

Fake 不读取镜像路径、不执行命令、不启动 VM。故障发生在操作副作用之前，不模拟部分磁盘删除或真实宽限期。epoch 仅保证在同一个 fake 对象内不复用；不实现后端重启恢复。

## 共用契约测试

位置：internal/testkit/vmcontract。Run 接受工厂，每个场景获得独立后端和有效配置。真实后端从显式启用的集成测试调用它，工厂自行提供本地内核与基础盘，测试不得联网下载。

覆盖创建冲突、重复启动、取消、并发等待、重启和旧 epoch、销毁限制等行为。Fake 专有测试另覆盖故障注入、闸门、外部退出和强停。

```sh
source /home/yedong/workspace/sandbox/artifacts/toolchain/activate.sh
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...  # 需要本机 C 编译器，不能将未运行记录为已通过。
```

artifacts/go.mod 仅将本地工具和缓存与主模块隔开，避免 go test ./... 扫描 Go 工具链自身的测试；没有业务代码。

## 给下一个 worktree 的 Agent

请基于当前基础提交创建新 worktree，负责 internal/host/vm/qemu 及其测试和说明，不重写共享定义、fake 或契约测试。需要变更共享契约时，先提出具体问题与最小改动。

交付要求：

1. 实现 vm.VMBackend，先支持已经手工验证的 Linux/amd64/microvm。
2. 构造函数注入受控数据目录、QEMU 和 qemu-img 路径；配置中的文件路径由 Host 解析，生产代码不写死当前 workspace 路径。
3. 使用 argv 启动 QEMU，不用 sh -c，不挂载宿主项目，默认无通用网卡。
4. 显式选择 KVM 或 TCG，unsupported 直接报错；其他平台只明确标注未支持。
5. 串行化同资源的生命周期操作，管理每次启动的 epoch 与 VMM 退出结果；不要依赖 QMP stop 完成关机。
6. microvm 不保证 ACPI 关机，明确 graceful/force 的能力和行为；停止保留磁盘，销毁失败可重试。
7. 复用 vmcontract.Run，在环境变量或集成标签显式启用后启动真实 QEMU；默认单元测试不启动 VM、不下载资源。
8. 所有新增注释使用中文；实现保持直接，不增加没有当前用例的兜底、兼容层和框架。
9. 输出测试结果与限制，不宣称已验证硬件加速、其他平台、Guest 协议或完整隔离。

本机手工验证证据在 docs/validation-2026-10-24.md。artifacts 被忽略，不会自动出现在新 worktree；可以使用原工作区的工具和只读基础镜像路径。每个 worktree 必须使用自己独立的运行目录、overlay 与 socket，不共享运行中的 VM。
