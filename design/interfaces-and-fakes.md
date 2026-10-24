# 并发开发的前置接口与 Fake 设计

状态：待实现的接口提案。本文补充 design.md；其中 Go 签名表达边界，尚不是已编译的接口。本文未实现生产组件或 fake。当前只确定协议设计原则，尚未冻结可供 Host、Guest、Fake 分别实现的线协议；须通过第 7 节的决策与实测门槛。

## 1. 目标与依赖方向

优先让 Host 编排、VM、Guest、Gateway、工作区可以独立开发，再用同一组契约集成。Manager 持有用例逻辑；HTTP handler 只处理鉴权、DTO 和响应。Guest 永远不能获得 Host 管理接口。

```text
HTTP API → Manager → VMBackend
                   → GuestClient → RPC → Guest 服务
                   → WorkspaceStore
                   → GatewayBindings

Guest Bridge → 设备数据流 → Host Gateway → 固定上游
                                   └→ CredentialStore
```

Manager 不代理每个模型数据包，也不把 Vault 交给 GuestClient。模型/MCP 数据流与控制、PTY、文件流在解析和调度上分开；是否分为独立物理设备尚待 spike 决定，不能据此各自选择拓扑。共享业务值类型放 internal/domain；线协议放 internal/protocol；Manifest schema/校验放 internal/manifest。domain 不依赖各实现包。消费方定义小接口，避免把所有接口堆进 protocol。

## 2. 第一批必须固定的值类型

| 类型 | 必须约定的语义 |
|---|---|
| SandboxID / SessionID / OperationID | Host 生成；Guest 回传的 ID 仅用于核对，不提供身份授权 |
| SourceRef | Host 授权目录注册后的不透明引用，不能用它拼接宿主路径 |
| ManifestRef / ImageRef | 版本和摘要绑定；解析后才进入启动流程 |
| SandboxState | 使用主设计的状态机；每次变更有单调递增 revision |
| SessionExit | 明确 exitCode、signal、reason；进程退出与通信失败不同 |
| BaselineID | 导入文件内容、路径、类型和权限的不可变基线 |
| ChangeSetID / Digest | Host 验证并固定后的导出快照；不能直接相信 Guest 的哈希 |
| ApprovalID | Host 保存的单次批准记录，绑定 sandbox、source、baseline、changeSet digest 和有效期 |
| Error | 稳定 code、可展示 message、requestID、recoverable；内部 cause 不直接返回 UI |

建议首批错误码：INVALID_SPEC、INVALID_STATE、NOT_FOUND、CONFLICT、UNAUTHORIZED、POLICY_DENIED、PROTOCOL_MISMATCH、GUEST_DISCONNECTED、TIMEOUT、RESOURCE_LIMIT、CLEANUP_FAILED。

所有外部操作接受 context。requestID 用于追踪，operationID 用于去重，二者不能混用。同一 operationID 配不同参数返回 CONFLICT。去重结果由 Host 持久化；Guest 内操作去重至少覆盖同一次 VM 启动。

## 3. 管理面接口

以下签名中的结构体需在编码前补齐字段与 JSON golden fixture；不直接将 Go 内部结构体暴露为 HTTP DTO。

```go
// manager 包定义；VMConfig 由 Host 从已验证的 Spec 生成。
type VMBackend interface {
    Create(context.Context, VMConfig) (VMRef, error)
    Boot(context.Context, VMRef) (VMConnection, error)
    Inspect(context.Context, VMRef) (VMStatus, error)
    Stop(context.Context, VMRef) error
    Destroy(context.Context, VMRef) error
}

type GuestClient interface {
    Hello(context.Context, HelloRequest) (HelloResponse, error)
    Prepare(context.Context, PrepareRequest) (PrepareResult, error)
    Start(context.Context, StartRequest) (SessionInfo, error)
    AttachTerminal(context.Context, SessionID) (Terminal, error)
    StopSession(context.Context, StopRequest) error
    WaitSession(context.Context, SessionID) (SessionExit, error)
    GetState(context.Context) (GuestState, error)
}

type Terminal interface {
    io.ReadWriteCloser
    Resize(context.Context, uint16, uint16) error
}

type GatewayBindings interface {
    Bind(context.Context, ChannelIdentity, BindingSpec) (BindingID, error)
    Revoke(context.Context, BindingID) error
}
```

- VMConnection 是控制与数据通道的装配占位，不代表已决定物理通道数量；连接关闭必须唤醒全部等待者。字段与 Done/Wait 归属由通道 spike 确认，VM 进程退出与通道断开必须区分。
- ChannelIdentity 由 Host 接受器根据已知 VMRef 和本次启动 epoch 创建，不从 Guest 消息反序列化；epoch 改变后旧绑定失效。
- Create 使用 VMConfig 中稳定的 VM 标识去重：同标识、同配置重复调用返回已有资源，同标识、不同配置返回 CONFLICT；不通过方法名前缀表达幂等性。Boot 已运行时返回现有启动结果，不重复创建进程或第二套消费者。
- Stop/Destroy/Revoke 重复调用成功；并发重复调用须串行化，清理失败保留可重试资源记录。
- Prepare 对应线协议 PrepareAgent，只接收内置 Manifest 引用、摘要和限域配置；不接收任意命令、Host 路径或真实凭据。相同 operationID 和参数重复调用返回相同 preparedID，参数不同返回 CONFLICT；运行中不能替换准备结果。
- Start 对应线协议 StartSession。StartRequest 含 Host 分配的 sessionID、operationID，以及 preparedID、workspaceRef、workspaceDigest。PrepareResult 绑定 Manifest、配置与工作区摘要；Guest 核对导入完成记录和准备结果后才能启动。摘要用于一致性核对，不是对恶意 Guest 的远程证明。MVP 每个 VM 同时仅一个 Agent session，第二次启动不能意外重复执行。
- AttachTerminal 通过控制 RPC 建立 terminal stream，后续输入输出仅走该流，不再设置逐次 WriteTTY/WriteTerminal RPC。Resize 对应控制 RPC ResizeTerminal。首版仅允许一个写入者；慢读有界背压，断线重连不重启进程，也不承诺历史输出重放。输出不按行处理，保留原始 ANSI/UTF-8 字节。
- WaitSession 可重复查询退出结果；RPC 断开返回连接错误，不能伪装成 exitCode=0。终端 EOF 不替代退出事件。

HTTP 语义：POST sandboxes 创建已验证配置和准备资源；POST /start 启动 VM 并到 GUEST_READY；POST /sessions 才启动 Agent。创建与启动为可查询的异步操作，响应返回 operationID；UI 查询 GET /operations/{id}。操作完成状态与 SandboxState 分开。相同幂等键在同一用户、路由范围内去重。

## 4. 生命周期编排与失败回滚

```text
校验 Spec/Manifest/镜像 → Host Stage 固定导入基线 → 创建 VM 资源
→ Boot → Hello 协商成功 → 将工作副本传入 Guest 并核对完成记录
→ 绑定 Gateway → PrepareAgent → GUEST_READY → StartSession → AGENT_RUNNING
```

Host Stage 在 VM 启动前发生，Guest Import 在 Hello 后发生，两者是同一次导入的不同阶段。GUEST_READY 在这里特指已准备好启动 Agent；Hello 成功仅表示控制协议就绪，不提前进入该状态。最小 spike 可使用镜像内置且固定摘要的测试工作区，不将它当作产品导入功能已完成。

Hello 内 Guest 自报的摘要只作一致性诊断，不构成远程证明；Host 必须自行校验选定镜像。身份来自 Host 为 VMRef + boot epoch 建立的专属端点与受控句柄，不从 Guest 自报字段建立。Hello 前禁止业务 RPC 和代理。若引入 Host challenge，只用于关联本次握手、防止误接收旧握手，不能证明 Guest 可信或替代端点访问控制；最小协议不要求另加 Guest 内 capability。绑定失败不得启动 Agent。

停止、VM 崩溃、控制断联时首先标记绑定失效并取消在途代理流，再停止进程/VM、关闭通道和释放临时句柄。清理使用独立且有界的 context，不能沿用已取消的请求 context。网络分区通过 heartbeat/lease 超时失效，不依赖 Guest 主动报告。停止不自动删除待审核文件；Destroy 才按用户选择清理。清理失败进入 CLEANUP_PENDING，不能报告销毁成功。

## 5. 工作区接口与审批

```go
type WorkspaceStore interface {
    Stage(context.Context, ImportSelection) (Baseline, error)
    OpenImport(context.Context, BaselineID) (io.ReadCloser, error)
    Preview(context.Context, BaselineID, io.Reader) (ChangeSet, error)
    Approve(context.Context, ApprovalRequest) (ApprovalID, error)
    Apply(context.Context, ApprovalID) (ExportResult, error)
}

// 文件流与 GuestClient 控制接口分离，使用同一 VM 连接绑定。
type GuestFiles interface {
    Import(context.Context, ImportDescriptor, io.Reader) error
    SnapshotChanges(context.Context, BaselineID) (GuestSnapshot, error)
    OpenSnapshot(context.Context, SnapshotID) (io.ReadCloser, error)
}
```

Preview 消费有大小上限的 Guest 导出包，验证路径、类型、数量、摘要并存入 Host 私有不可变快照，Diff 与 Apply 都读取该快照。审批后不能再从 Guest 读取“当前内容”直接回写。

MVP 归档只允许普通文件和目录，拒绝链接与特殊文件；明确支持删除和可执行位，限制解包大小及路径长度。导入导出共用相同格式、校验器和恶意样例。Guest 不能提供 Host 绝对路径。

Apply 重新检查授权目录和宿主文件基线，发现冲突不写入。检查与写入之间仍需防路径替换：实现须使用目标平台支持的安全文件访问方式，或对无法保证的目录拒绝应用。多文件写入不天然具有事务性；写入前完成全部校验，保留恢复记录，返回明确的部分失败与恢复状态，不宣称全目录原子提交。

## 6. 数据面：模型与 MCP

Gateway 按 Host 绑定身份接受请求，而不是暴露通用任意 URL RoundTrip：

```go
type Gateway interface {
    Serve(context.Context, ChannelIdentity, ProxyRequest) (ProxyResponse, error)
}
```

ProxyRequest 只含逻辑 serviceRef、method、受限相对路径、允许的 headers、流式 body。ProxyResponse 含 status、过滤后的 headers 和流式 body。上游地址、认证方式、credentialRef 仅存在 Host 服务记录中。CredentialStore 定义在宿主可信侧，不进入 domain/protocol。

Body 不使用无限制 []byte。必须定义请求大小、并发数、空闲超时、总超时和有界缓冲；SSE 分块及时 flush。取消、断联、撤销绑定须关闭上下游 body 并取消请求。移除 Guest Authorization、Host 等不可信头；默认禁止重定向。MCP 的 sessionID 按 binding + service 隔离。工具 allowlist 必须由真实协议解析执行，不把透明字节转发标为工具级授权已完成。

## 7. 最小线协议决策与冻结门槛

第一阶段只冻结 v0.1 控制协议与终端流。文件与 Gateway 流可以复用后续确定的 framing，但业务字段另行评审；不得临时把文件或模型 body 塞进控制 JSON。v0.1 是开发前的最小候选协议，真实设备测试通过前不能标记为已验证或稳定 v1。

### 7.1 必须先作出的决策

| 决策 | 冻结前必须交付的内容 |
|---|---|
| 传输拓扑 | 在“一个设备内部多路复用”和“独立 control/terminal/data 端口”中选择一种，明确各流归属；提交主平台 OS/CPU/QEMU/机器类型、实际启动参数、Guest 设备名、端点权限与关闭行为。其他平台不可未经验证直接照搬 |
| Envelope | 确定 request/response/event/stream 各自字段、二进制头、字节序、长度与校验算法；requestID 关联一次调用，streamID 关联一次数据流，operationID 关联可重试操作，不互相替代 |
| 请求与错误 | 固定 response 的 result/error 互斥、错误码、重复 requestID、未知/重复 response、迟到响应及缓存/淘汰规则；错误 message 不含秘密 |
| 取消和时限 | 固定 deadline 的传输方式、cancel 消息与完成竞态、half-close/EOF/abort 的区别；请求取消不自动终止已创建 Session，终端关闭不终止进程 |
| 流生命周期 | 固定 AttachTerminal 响应与流开始的顺序、streamID 分配方/有效域/复用规则、终端双向数据、流关闭、每流背压及有界调度 |
| 握手和身份 | 固定握手状态图，定义 Host 端点→VMRef+epoch+连接的映射；旧端点关闭，新连接不得复活旧授权，收到旧 epoch 或未 Hello 的业务消息必须拒绝 |
| 去重和重试 | 明确 operationID 缓存作用域、参数规范化摘要、容量、保存时限和状态查询；Start 超时不代表未执行，不能换新 operationID 盲目重试 |

单帧 64 KiB、每 VM 最多 16 个活动数据流仅为初始提议；最终数值、控制头开销、并发上限和信用窗口需写入候选规范与共同 fixture。不能让 Host、Guest、Fake 各自解释这些默认值。

### 7.2 v0.1 消息范围

候选控制方法统一为 Hello、GetState、PrepareAgent、StartSession、AttachTerminal、ResizeTerminal、StopSession；退出通知为 SessionExited。StartSession 返回 SessionInfo，AttachTerminal 单独返回终端流描述；不并存 WriteTTY/WriteTerminal。WaitSession 是客户端等待/查询退出结果的抽象，其事件订阅与 GetState 恢复方案须在 envelope 冻结时写清，避免把长时间等待占满控制处理队列。

最小成功链路：QEMU 启动 → Hello → GetState → PrepareAgent → StartSession → AttachTerminal → PTY echo/Resize → StopSession → SessionExited。测试 CLI 使用预装受信测试 Manifest，不引入任意命令 RPC。

### 7.3 Day 1 spike 的可执行验收

1. 在指定真实目标平台启动 Guest，记录 QEMU 配置和实际设备拓扑。
2. Host 建立并关闭连接，所有阻塞读写和 RPC 等待者均能在有界时间内退出；区分连接断开与 VMM 退出。
3. Hello 后 Host 记录 VMRef + epoch + 连接关联；未握手业务请求、旧端点和旧 epoch 消息均被拒绝。Guest 自报摘要不作为身份认证。
4. 使用测试 Manifest 完成上述最小成功链路，能看到终端原始输出、窗口变化及可信的退出结果。
5. 同一 operationID 并发或超时重试 StartSession，只产生一个进程；不同参数返回 CONFLICT。
6. 半帧、超长帧、未知类型、未知 stream、重复/迟到响应和中途断线均按候选规范处理，错误有确定预期。
7. Fake 与真实编解码/通道适配器消费同一组黄金字节 fixture；高层 FakeGuestClient 测试不能替代这一项。

通过后集中提交拓扑决策、线协议规范、fixture 与实测结果，才能宣布 v0.1 冻结并交给多 Agent 独立实现。随后立即验证“无通用外网 Guest → Bridge → Host Gateway → 单次流式模型请求”；文件/MCP 完整字段可后置，但不能把这个关键风险验证推到第二天。

## 8. Fake 的层次与实现要求

fake 是带状态的替代组件，不是每个方法固定返回 nil 的 stub。默认不执行 Host 命令、不启动真实 CLI、不访问外网、不读取真实 Vault。

| Fake | 行为 | 主要使用者 |
|---|---|---|
| VMBackend | 内存资源表、启动 epoch、就绪闸门、崩溃事件、可重试清理失败；通道用内存 pipe | Manager |
| GuestClient | Prepare/Start 状态检查、脚本化终端输入输出、resize 记录、独立退出事件、断联 | Manager/API/前端联调 |
| WorkspaceStore | 测试临时目录和真实哈希，独立 Guest 副本；审批后快照固定；模拟宿主并发修改 | 导出流程 |
| GatewayBindings | 按 epoch/service 授权、撤销、过期、取消在途调用 | 生命周期与隔离逻辑 |
| CredentialStore | 仅注入显式测试假值；不提供生产 Key 回退路径 | Gateway |
| 模型上游 | 本地 HTTP 服务，SSE 多块、延迟、429、半途断开、取消可观测 | 真实 Model Gateway |
| MCP 上游 | 固定协议基线，initialize、tools/list、只读 tools/call、会话、错误、SSE | 真实 MCP Gateway |
| Clock/EventSink | 手动推进时间；有界事件收集与字段检查 | lease/超时/事件测试 |

模型/MCP fake 应替换“上游服务”，保留真实 Gateway 的认证清洗、路由、限额、会话逻辑。另设可通过内存通道连接真实 Guest RPC handler 的测试层，补足直接 FakeGuestClient 绕过编解码的问题。

统一放 internal/testkit 下按 vm、guest、workspace、upstream 分包；只抽出被生产代码使用的最小接口。fake 需要并发安全、资源关闭、调用记录；测试通过屏障/闸门控制时序，避免用 sleep 猜测。真实网络 timeout 测试使用有界 deadline，与手动时钟驱动的业务 lease 测试区分。

故障注入按操作和发生阶段配置，例如 BootBeforeReady、DisconnectAfterStart、ProxyAfterChunk(2)、ApplyBeforeWrite、DestroyOnce。应能断言调用顺序和取消已生效。测试 fake 自身只覆盖这些关键状态语义，不逐方法复制实现。

## 9. 共用契约测试与验收

将可复用测试写成接收工厂的 suite，分别运行 fake 与真实适配器；QEMU 和 OS Vault 需要独立环境标签，不要求默认测试启动真实 VM。

首批场景：

1. 相同创建/启动 operationID 并发调用，只产生一个资源/进程；参数不同报冲突。
2. Hello 不匹配或 Gateway 绑定失败，Agent 不启动且资源可回收。
3. Stop/Destroy 重复调用；中间清理失败重试后无遗留资源。
4. 控制断开后绑定失效，在途代理取消，等待中的 RPC 退出。
5. 慢终端/代理读者不会使内存无限增长，取消不会永久阻塞。
6. 另一 VM 的 session、binding、MCP session 均不可复用。
7. 未批准不回写；批准后 Guest 修改不影响快照；宿主冲突阻止导出。
8. 假上游观察到 Host 注入的测试认证，Guest 认证头不透传，日志不含测试秘密。
9. SSE 首块在上游结束前可读；客户端取消会关闭上游请求。

Fake 测试不能证明 QEMU 网络隔离、真实非 root 权限、Host 文件边界、OS Vault 可用或跨平台设备兼容。design.md 的 S01/S02/S04 等仍须真实 VM 和平台测试，结果分别标注。

## 10. 并发落地顺序与所有权

1. 集成负责人：初始化 go.mod、共同类型、消费方接口、错误码、消息 fixture、契约 suite；统一维护 go.mod/go.sum。
2. VM Agent：实现 VMBackend、真实通道 spike 和对应 fake；随后实现 Host/Guest 文件传输及工作区。
3. Guest Agent：实现 GuestClient fake、真实 RPC handler/client、Manifest/Runner/Bridge；使用假上游联调。
4. Gateway Agent：实现绑定 fake、Vault fake、模型/MCP 假上游和真实 Gateway。
5. 集成负责人：实现 Manager，先用全部 fake 跑启动/停止/导出流程，再逐个替换真实组件，最后接 UI。

fake 仅允许在测试或显式开发入口装配，开发界面必须标注模拟模式。生产入口构造失败直接报错，禁止自动回退 fake、内存 Vault 或 Host 原生进程执行。

协议相关模块独立并发前先通过第 7 节门槛；此前可并行做设备、PTY、上游服务的独立技术验证，但不各自定义线协议。高层集成的首个合入门槛是契约与全部 fake 闭环；随后优先完成真实 VM → Bridge → Gateway → 流式上游这一条链路。本文的接口草案需在 spike 后做一次集中修订，后续变更同步修改实现、fake、fixture 和 suite。
