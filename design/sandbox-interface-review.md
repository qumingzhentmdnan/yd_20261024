# 开源沙箱接口调研与修订建议

调研日期：2026-10-24。范围：官方文档、协议和源码的静态分析；没有在本机运行这些项目。main/latest 为可变来源，实际集成前需要固定版本。本文是 interfaces-and-fakes.md 的修订建议，尚未修改原接口或实现代码。

## 参考实现与结论

| 参考实现 | 可观察到的设计 | 对 SafeAgent 的启示 |
|---|---|---|
| [Firecracker Go SDK](https://github.com/firecracker-microvm/firecracker-go-sdk/blob/main/machine.go) | Machine 提供 Start、Shutdown、StopVMM、Wait；Wait 可多调用者等待 | VM 需要退出通知；启动请求的 context 和 VM 生命周期必须分开约定 |
| [Kata Agent 协议](https://github.com/kata-containers/kata-containers/blob/main/src/libs/protocols/protos/agent.proto) | 分开启动进程、信号、等待、stdio 与窗口调整 | 保留执行、连接、退出三种生命周期；不要把所有动作塞进终端对象 |
| [E2B Python SDK v2.5.0](https://docs.e2b.dev/sdk-reference/python-sdk/v2.5.0/sandbox_sync) | 分开 Sandbox、Commands、PTY；支持连接已有实例、终止与等待 | 应明确实例、进程与连接的身份；不必照搬其云端 kill/TTL 语义 |
| [Daytona Process SDK](https://www.daytona.io/docs/en/typescript-sdk/process/) | PTY 提供创建、连接、输入、等待与 disconnect | 断开终端连接和结束进程应分离；重连不自动代表输出重放 |
| [OpenSandbox Lifecycle](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/sandbox-lifecycle.yml)、[执行协议](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/execd-api.yaml) | 生命周期与执行有独立规范，创建可直接到运行态 | VM 后端和 Guest 执行分层合理；Create 是否启动必须在我们自己的 API 中明确 |
| [OpenShell Provider v0.1.5](https://docs.nvidia.com/openshell/v0.1.5/how-it-works/providers/overview) | 网络准入和凭据端点绑定是独立检查，placeholder 在代理侧解析 | 授权必须绑定目标端点和运行实例，凭据绑定不能替代网络隔离 |
| [QEMU QMP run-state](https://github.com/qemu/qemu/blob/master/qapi/run-state.json) | QMP stop 是暂停 vCPU；关机事件与进程退出也有区别 | 我们的 Stop 必须明说“停止 VMM 进程”，不能直接映射到 QMP stop |

这些项目分属 VMM SDK、Guest runtime、云沙箱和代理安全运行时，不能把同名操作视为相同承诺。以下建议基于本项目“本地 QEMU、保留磁盘审核导出、默认无通用外网”的需求作出。

## 1. VMBackend

原职责拆分合理，但 Boot 同时返回通信连接，混合了 VM 启动与连接的生命周期；Inspect 无法及时通知异常退出，原文提及 VMConnection.Done/Wait 但未固定。建议改为：

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

- 创建方法统一命名 Create，幂等性通过契约表达：VMConfig 包含稳定 VM 标识，同标识、同配置返回已有资源，同标识、不同配置返回 CONFLICT。实现必须比较配置摘要，不能在重复调用时额外创建磁盘。
- Boot 改 Start：管理的是一次 QEMU 运行，名字更容易与 Wait/Stop 对应；Boot 并非错误，只是不能暗示 Guest 已就绪。
- VMRef 是持久资源；VMInstance 是一次运行，至少包含 VMRef 和 Host 生成的 epoch。Stop/Wait 指定实例，防止延迟请求误操作重启后的 VM。
- Start 不返回 GuestClient。通道管理组件根据实例建立/接收受控连接；具体可由内部 VMRuntime 聚合器装配，MVP 不必为每个步骤再建一个抽象工厂。
- Wait 等待指定 VMM 实例退出并缓存结果；连接中断不等于 VMM 退出。并发等待者都能获得相同结果，各自 context 取消只结束等待。
- StopOptions 指明 graceful timeout 和 force-after-timeout，Stop 成功表示 VMM 已退出。是否支持 Guest 优雅关机由设备能力决定；不支持则明确返回或按显式策略强制结束。
- Start 的 context 只约束启动动作，返回成功后 HTTP 请求取消不能杀死 VM；VM 由服务级 supervisor 管理。启动超时但结果不确定时通过 Inspect/操作记录核对，不能盲目再次启动。
- Destroy 只删除已停止资源；Stop 保留磁盘。Inspect 可观察 unknown/disconnected，不能把无法查询等同于 stopped。

## 2. GuestClient

GuestClient 作为 Host 端 RPC 客户端名称合理；GuestAgent 容易与 Codex/Claude 混淆。原方法部分缺少对象名，建议：

```go
type GuestClient interface {
    Handshake(context.Context, HandshakeRequest) (GuestInfo, error)
    PrepareAgent(context.Context, PrepareAgentRequest) (PreparedAgent, error)
    StartSession(context.Context, StartSessionRequest) (SessionInfo, error)
    AttachTerminal(context.Context, SessionID) (TerminalConnection, error)
    StopSession(context.Context, StopSessionRequest) error
    WaitSession(context.Context, SessionID) (SessionExit, error)
    Status(context.Context) (GuestStatus, error)
}
```

- Hello 在线协议中可以保留；客户端方法用 Handshake 表示一次协商。它不是远程证明，也不提供 VM 身份。重连协商和存活探测要分清。
- PrepareAgent 的结果必须带 preparedID、manifestDigest、配置摘要；StartSession 引用该 preparedID，避免启动使用过期的可变配置。
- Session 定义为一次顶层 Agent 运行及其受管理的子进程，不是网页连接、模型对话或持久 shell。若以后提供任意执行，另建 ProcessClient，不扩张这个接口。
- StopSession 定义为有界停止整个会话进程树；如果只发送一个 signal，应叫 SignalSession，并由调用者随后 WaitSession。MVP 先保留 StopSession，不同时加入无实际用途的两套控制。
- 正常非零退出记录在 SessionExit，RPC error 表示没有可靠获取结果；context 取消不终止远端进程。
- Status 比 GetState 更明确是观察值，不能通过 GuestStatus 修改 Host 状态机。

## 3. TerminalConnection

```go
type TerminalSize struct {
    Cols uint16
    Rows uint16
}

type TerminalConnection interface {
    io.ReadWriteCloser
    Resize(context.Context, TerminalSize) error
}
```

Terminal 不是错误命名，但 TerminalConnection 清楚表达它是一次可关闭的连接。尺寸结构体避免 cols/rows 两个同类型参数传反。窗口尺寸范围由实现校验。

[Go io 文档](https://pkg.go.dev/io) 不保证所有 Reader/Writer 实现并发安全，因此本项目需显式保证：允许一个读协程、一个写协程和并发 Resize/Close；Close 或 Attach context 取消解除阻塞中的读写，只关闭 attachment；不关闭共享 GuestClient；不停止 Session。重复 Close 成功。

首版承诺实时重新连接，不承诺输出补读。后台仍需持续排空 PTY 到有界环形缓冲或丢弃策略，不能等网页 Attach 后才读 PTY，也不能断线后无限积压。未来若要求可靠补读，再增加带字节序号的 AttachOptions/结果及过期游标错误，单纯 io.Reader 无法描述缺失区间。

退出事件和终端尾部输出有竞态：收到 SessionExit 后继续排空终端至 EOF/有界 drain 超时，不能立即丢弃最后输出。

## 4. GatewayBindings

Bind/Revoke 的方向合理，但 Bind 可能被误读为监听端口，Bindings 容易被理解为一个集合。建议按授权对象命名：

```go
type GatewayAccess interface {
    Grant(context.Context, WorkloadIdentity, AccessSpec) (GrantID, error)
    Revoke(context.Context, GrantID) error
}
```

- WorkloadIdentity 是 Host 确认的 sandboxID + VM 启动 epoch，不等于一根易断开的 socket；ChannelIdentity 可留在传输层，用于映射到该工作负载身份。
- AccessSpec 包含稳定 grant 操作 ID、serviceRef、credentialRef、策略版本、限额和租约。目标 scheme/host/port/path/method 由 Host 服务注册表解析，不由 Guest 选择。
- 网络允许与凭据允许分别校验；VM 没有通用网卡的边界仍需实际验证。
- 授权只对当前 epoch 有效；断联撤销后重连也不能自动复活旧 grant。Revoke 幂等、阻止新请求并取消本地在途转发，不能承诺撤回上游已经执行的工具调用。
- GrantID 是管理记录标识，不是 Guest bearer token。MVP 不增加动态 Update/列表/通用策略引擎，有实际用例再扩展。
- 若团队更偏好“绑定”术语，GatewayAuthorizations 或 BindingManager + Create/Revoke 也可；关键是记录身份、服务和时效的关系，而不是仅保存 sandboxID→API Key。

## 5. 对 Fake 和契约的影响

需同步新增以下行为，不只改方法名字：

1. VMRef 跨重启不变，epoch 改变；旧实例的 Stop、旧 grant、旧 session 不能影响新实例。
2. Start 返回后取消请求 context 不杀 VM；取消 Wait 不杀 VM/Session；取消 Attach 只关闭连接。
3. VM 真实退出与仅通道断开有不同事件，但两者都会触发授权失效策略。
4. Stop 超时/强杀/失败和部分清理可独立注入；重复 Wait 返回同一退出结果。
5. preparedID 失配拒绝启动；Prepare/Start 重试不能重复启动 Agent。
6. 无前端时 Guest 仍排空 PTY，缓冲有界；退出尾部输出、Close 解阻塞有确定性测试。
7. 撤销取消在途代理，但测试不能假定上游副作用被回滚。

首批冻结：资源与实例身份、context 生命周期、Stop 完成条件、退出结果、Prepare/Start 绑定、终端断连语义、授权撤销。命名可随上述语义集中修改；不要在缺乏实现验证时过早冻结大而全接口。
