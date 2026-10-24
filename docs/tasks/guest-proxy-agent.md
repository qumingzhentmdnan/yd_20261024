# Guest Bridge 与独立 proxy 通道任务

## 1. 目标和范围

打通以下真实链路，Guest 保持 `-nic none`、没有通用外网：

```text
Guest 内非 root 固定 HTTP 测试程序
  → 仅监听 Guest 127.0.0.1 的 HTTP Bridge
  → 独立 safeagent.proxy virtio-serial 端口
  → Host 通道适配器
  → internal/host/gateway
  → Host 本地 TLS/SSE 测试上游
```

本轮先支持一个在途 proxy 请求，拒绝或有界处理第二个请求，行为须冻结并测试；不能建立无限排队或偷偷串行等待到无期限。保留未来扩展空间不等于实现多路复用框架。

不实现 Workspace Import/导出、Manifest 通用加载、MCP 会话/工具授权、真实 Vault、真实供应商 SDK 或 CLI、UI 和完整业务 Manager。Guest 固定测试程序只验证 HTTP 行为，不冒充实际 Codex/Claude Agent。不得把 Host 的测试服务加入 Guest 通用网络。

## 2. 基线与必读资料

最低代码基线为包含以下实现的提交：

- `44f3a1b`：真实 Host RPC、Guest Handler、非 root PTY、双通道装配和真实 TCG 测试。
- `631f2df`：Host Workspace 快照及规范导入包。本任务不修改该模块。
- `6742a52`：Host Gateway 核心、明确假凭证和本地传输探针。

必读：

- AGENTS.md。
- design/host-gateway.md、internal/host/gateway/{README.md,types.go,gateway.go,transport.go}。
- docs/host-gateway-spike-2026-10-24.md 及 scripts/spikes/host-gateway。
- docs/guest-agent-implementation.md、scripts/guest-agent/README.md。
- design/guest-protocol-v0.1.md、internal/protocol/{README.md,frame.go}、internal/guestwire/wire.go。
- internal/host/vm/qemu/{backend.go,channels.go}、internal/host/guest/rpc/client.go。
- cmd/guest-agent/main_linux.go、internal/guest/device/device_linux.go、internal/host/guest/rpc/integration_test.go。

若存在 docs/merge-validation-2026-10-24.md，读取合并后重验结果：协议及全仓 test/vet/race 已通过，重建 race Guest 后 13 个独立 TCG 场景通过。该文档和 artifacts 可能只在主工作区，缺失时不能伪造已经执行本 worktree 的验证。

本地 toolchain、rootfs、内核及基础盘不随检出交付。先核对实际可用的绝对素材路径、工具和平台。没有对应环境时明确报告真实验证未运行，继续能独立完成的设计和本机验证。

## 3. 文件归属与共享修改

| 范围 | 建议位置 | 约束 |
|---|---|---|
| proxy 协议与编解码 | 独立的公共 proxy 协议包，路径由设计确定 | Host/Guest 共用同一 DTO、codec、fixture；不依赖 Host Gateway 或 testkit |
| Host 适配器及薄装配 | internal/host 下独立 proxy/bridge 子包 | 调用现有 Gateway.Serve/Bind/Revoke，不复制凭证、路由、DNS/IP 或 TLS 策略 |
| Guest HTTP Bridge | internal/guest 下独立 bridge 子包 | 固定逻辑服务映射，只监听 127.0.0.1，不携带真实上游地址或凭证 |
| QEMU 通道 | internal/host/vm/qemu | 新增 safeagent.proxy 及真实实例端点交付，保留 control/terminal 行为 |
| Guest 装配与镜像 | cmd/guest-agent、scripts/guest-agent 或新的独立验证入口 | 接入新设备、固定测试程序及 Bridge 配置；不破坏既有固定 PTY 验证 |
| 测试与证据 | 对应包测试、docs/design/reviews、artifacts | 临时文件和原始日志放忽略目录，保留失败及修复证据 |

共享修改须先在设计中列清理由和影响：当前 RPC Client 的 connectionID 与失效 context 是私有状态；当前 QEMU 只交付 control/terminal；不要假设已有 OpenProxy 或公开的 Done/身份查询。确定最小可信对接方式，可以增加必要的具体装配接口，但不要为了传业务连接扩展 VMBackend/Driver/Process，不机械扩大所有 guest.Client/Fake 的接口。

当前 protocol 的 Port/Kind 仅允许 control/terminal，guestwire 也受该校验约束。不能把新流伪装成 terminal 或放宽旧端口的类型检查。设计中比较受控扩展公共 framing 与独立 proxy namespace，选定一种后提供版本、codec 和 fixture；不在两端各自复制协议，也不静默改写已有 v0.1 控制语义。

## 4. 先设计并验证，再开始生产代码

先交付最小设计，至少冻结：

1. **连接与启动顺序**：设备名、Host socket、实例绑定、Guest 初始化、Hello/GetState、Gateway Bind、何时接受 HTTP 请求。proxy 不能早于可信握手开放，也不能依赖 Guest 自报身份建立授权。
2. **请求和响应**：版本、消息类型、ID 有效域、头与 body 分隔、长度/数量上限、逐块响应、正常结束与异常中止。若复用 framing，明确新增类型及端口隔离规则。
3. **取消及复用**：客户端断开、半关闭、超时、取消和终止确认；旧请求迟到数据如何处理，失败后物理通道是否可再使用。不能直接照搬 Unix socket 的 EOF/重连假设。
4. **背压及预算**：单帧、每方向队列、在途请求、HTTP headers/body、帧读取与写入预算；慢读不能堵 control/terminal，不能为每帧或每次 Read 创建无界 goroutine。
5. **Host 身份和失联**：VMInstance、控制连接、proxy 连接与 Gateway BindingID 的可信关联；停止、崩溃、断联及新 epoch 的失效顺序。明确探测预算、失败规则及控制面繁忙时的处理。
6. **资源所有权**：谁关闭输入/输出、取消回调怎样退出、何时释放并发资格、谁在客户端已关闭后仍能回收 VM；不以请求 context 执行必须完成的 VM 清理。

在 scripts/spikes 下使用可丢弃原型和手工命令验证本轮新增的真实设备风险，至少覆盖 proxy 双向传输、单方向停滞/取消、设备关闭观察、并发控制/终端可用性。Host HTTP/TLS 的旧探针只能作机制参考，不能替代新 proxy 设备实测。

设计和探针结果调用 codexx 只读评审；修复有效问题并在原 session 复审。通过后再实现正式模块。不要把原型导入生产包，生产代码必须消费评审后确定的公共 codec 与 fixture。

## 5. 必须保留的安全与生命周期语义

- Gateway 身份只能来自可信 Host 装配，不能相信 Guest HTTP 头、URL 或 proxy payload 中的 VM/epoch/connectionID/BindingID。Guest 业务输入仅携带逻辑服务、受限路径和内容。
- 继续使用 Host Gateway 的服务白名单、请求/响应大小、头清洗、凭证注入、DNS/IP/TLS 与不跟随重定向策略。只用 fakecredential 的明确测试假值；Guest 不得获得 Host 测试认证值、credentialRef 或真实凭证配置。
- 当前 Gateway 请求体先有界缓存，响应为流。适配器须遵守其 Body.Close 可并发解除 Read 的契约。Go 服务端原始 r.Body.Close 不能保证解除阻塞上传，必须实际设置读取 deadline，且等待取消回调退出后再结束 handler；写响应逐块 Flush，并有写超时/取消。
- 真实 virtio Guest 关闭设备不保证 Host socket 收到 EOF。Host 必须用已评审的有界控制探测/失效机制，检测后先 Revoke 取消代理，再关闭连接并回收 VM；不能只依赖读循环 EOF。
- 当前不支持同 boot 控制重连，禁止新增自动重连或盲目重放请求。控制连接失效后按既有策略回收 VM，用新 epoch 重新启动和绑定。
- proxy 单独断线至少取消该通道全部在途请求并撤销对应授权；是否保留控制连接供诊断或直接回收 VM，设计中明确。除按已授权生命周期清理外，不让普通 HTTP body 关闭或终端 Detach 隐式终止工作负载。
- SSE 已发送后中断不能伪造成功 EOF、补一段“成功 JSON”或静默重试。网络/流错误须在适配层以已冻结的中止语义传给 Guest 客户端。

## 6. 实施顺序

1. 核对基线、素材与现有测试结果，写最小设计和可执行验收清单。
2. 新 proxy 真实设备探针，记录全部命令与失败，完成 codexx 设计复审。
3. 公共 codec/fixture，以及内存通道上的真实 Host 适配器与 Guest Bridge；调用真实 Gateway 和本地 TLS/SSE 上游，不替换整个 Gateway。
4. 装配身份、绑定、取消与薄生命周期协调，使用闸门覆盖握手前、注册/撤销、响应交付和新 epoch 的竞态。
5. 接入 QEMU 独立 proxy 端口、Guest 设备和固定 HTTP 工作负载，跑真实 VM 链路。
6. 补慢上传/慢读、半帧、超长帧、异常关闭、控制失联及资源回收；既有 control/terminal 回归必须继续通过。
7. 代码 review、修复、原 session 复审；另做冗余/防御性代码专项审查，重跑受影响检查，完善进度与验证文档。

## 7. 验收标准

### 普通验证

```sh
source artifacts/toolchain/activate.sh
python3 scripts/validate-guest-protocol.py
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...
```

新增协议的校验器/fixture 检查也必须执行。独立 spike module 不包含在根 go test ./... 中，应另列命令及结果。测试环境若限制本机 socket/设备操作，按正常权限机制运行，不能把 EPERM 当作已通过。

### 真实 VM 最低断言

- 使用当前正式代码新构建 Guest 镜像，明确 Linux/amd64、microvm、TCG，无 Guest 通用网卡/默认外网。
- 非 root 固定工作负载通过 Guest-local Bridge 发起请求，Host 本地 TLS 上游收到实际数据，只看到 Host 注入的测试认证。
- 上游未结束时 Guest 已看到 SSE 首块；二进制分块不被当成逐行文本任意改写。
- Guest 不能覆盖服务地址/Host/认证；未授权服务/路径、未 Hello、旧 epoch/连接和撤销绑定均被拒绝。
- 第二个并发请求按冻结策略有界拒绝或处理；代理流一直占额直到 EOF/Close/取消。
- 部分上传停滞、Guest 客户端中途关闭、下游慢读、上游停滞、总/空闲超时和 proxy 断线能结束等待并释放资源；已开始的响应异常不会被当作成功。
- 大响应/慢读期间 GetState、终端输入输出与 Stop 仍可用；不能仅证明 Host 本地 HTTP 不阻塞。
- 控制失联经有界机制被检测，Gateway 绑定撤销、在途上游取消，VM 被回收；新 epoch 不接受旧请求或迟到数据，不承诺旧 epoch 自动恢复。
- Stop/Wait/Destroy 用独立清理 context；核对基础盘前后 SHA-256、所有自有 QEMU、Bridge 端口、连接、goroutine/文件句柄的必要释放证据。
- 至少重跑既有真实 Guest 正常闭环、共享契约、控制故障、终端故障；具体命令按新 worktree 的实际绝对镜像路径给出。

真实供应商、真实 Vault、KVM、跨平台运行、真实 Agent CLI 均不在本轮必需条件；未运行的内容明确标记，不能用 Fake、宿主网络探针或旧 TCG 日志替代本次实现验收。

## 8. 最终交付

交付正式代码与测试、proxy 协议与黄金 fixture、可复现构建/验证命令、设计及 review 结论、当前进度与未支持范围。原始运行日志/镜像/会话放 artifacts，不提交真实凭证或大体积素材。

报告每轮有效问题与修正、不采纳意见及其具体依据；所有失败/超时保留，不能把重跑成功覆盖成首次就通过。不得依靠猜测 sleep 控制竞态，也不得为未实际需要的状态增加防御性兜底。

只使用本 worktree 的 codexx session；具体命令与权限规则遵守 AGENTS.md，首次/恢复均显式 xapi/gpt-6.1-sol/medium。Git 写操作仅在用户明确授权范围内进行；没有提交授权时完成代码与验证后停留在可审查变更状态，不擅自提交。
