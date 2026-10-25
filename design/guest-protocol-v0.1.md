# Guest 控制与终端协议 v0.1 候选

状态：接口与线协议候选，尚未冻结。共享 DTO、编解码、Host 接口、有状态 Fake 和共享行为测试已落地，见 `../docs/guest-foundation.md`；生产传输与 Runner 未实现。固定工作负载原型已完成 Linux/amd64/microvm/TCG 正常及 race 验证，见 `../docs/guest-rpc-spike-2026-10-24.md`；生产 Handler/Client、Manifest/Import 和全部容量边界尚未实现或验收。不能把原型通过当作本文全部字段已经实现。

字段定义以 [JSON Schema](schemas/guest-protocol-v0.1.schema.json) 为候选契约，正反例见 [负载 fixture](fixtures/guest-protocol-v0.1.json)。本文规定 Schema 无法表达的字节、状态、关联与并发语义。Schema、正文不一致时必须修正，不由实现方自行选择。原型中的 `params` 大结构体和固定摘要不是生产接口。

从仓库根目录运行 `python3 scripts/validate-guest-protocol.py` 校验 Schema、负载正反例和[参数指纹向量](fixtures/guest-operation-v0.1.json)。校验工具使用 Python `jsonschema`（本机已有），用于验证 Draft 2020-12 契约；不引入 Go 生产依赖。该命令不验证真实通道或状态机。

## 1. 范围与拓扑

当前目标为 Linux/amd64、QEMU microvm、显式 TCG；产品硬件加速和其他平台另行验证。

- `safeagent.control`：独立 virtserialport，双向控制 request/response/event/cancel。
- `safeagent.terminal`：独立 virtserialport，双向终端帧；v0.1 每个 VM 同时仅一个 session、一个 terminal attachment，不引入多个 PTY 的调度。
- `safeagent.proxy`：保留为独立技术验证端口，代理/文件业务不在本次规范范围；不能把其 payload 塞进控制帧。

Host 每次启动使用私有 0700 目录创建专属 socket，将受控句柄绑定已有 `VMInstance{Ref, Epoch}`。Guest 自报 VM ID、镜像摘要或 nonce 都不能覆盖这个映射。非 root Agent 不持有 virtio 设备句柄。

每个端口分别读写；控制与终端不得共用一个阻塞写队列或长期持有的全局锁。数据端口慢读时只对自身施加背压。

## 2. 共同帧头

所有整数采用网络字节序。头长固定 16 字节，payload 长度不包含头。每端口按字节流解析，允许头和 payload 任意分片，也允许一次读取多帧。

| 偏移 | 长度 | 字段 | 候选值或语义 |
| --- | --- | --- | --- |
| 0 | 4 | magic | ASCII `SAGP` |
| 4 | 1 | major | 0 |
| 5 | 1 | minor | 1 |
| 6 | 1 | kind | 下表 |
| 7 | 1 | flags | 0，其他值拒绝 |
| 8 | 4 | payloadLength | 控制最多 65536 字节；终端最多 16384 字节 |
| 12 | 4 | streamID | 控制恒为 0；终端使用 Host 分配的非零值 |

| kind | 端口 | 含义 |
| --- | --- | --- |
| 1 | control | Request，UTF-8 JSON |
| 2 | control | Response，UTF-8 JSON |
| 3 | control | Event，UTF-8 JSON |
| 4 | control | Cancel，UTF-8 JSON |
| 16 | terminal | Bind，UTF-8 JSON |
| 17 | terminal | Bound，UTF-8 JSON |
| 18 | terminal | Data，原始 PTY 字节 |
| 19 | terminal | Fin，空 payload |
| 20 | terminal | Abort，固定错误码 JSON |

底层为本地 Unix socket 与 virtio 字节通道，候选不另加 CRC/MAC；magic、长度和状态检查只用于解析约束，不构成完整性认证或远程证明。校验算法为“无”，不得各自增加 checksum 字段。

读头后先检查长度再分配 payload。坏 magic、不支持版本、未知 kind、错误端口、非法 flags、超限帧或半帧 EOF 均终止当前协议连接，不尝试扫描下一个 magic 恢复。任何端口半帧不能交给业务层。

Request/Cancel 只能 Host→Guest，Response/Event 只能 Guest→Host；Bind 只能 Host→Guest，Bound 只能 Guest→Host，Data/Fin/Abort 可双向发送。错误方向也属于协议错误；控制错误使整个逻辑连接失效，终端错误仅中止本 attachment。Data 不经过 JSON 解码，Fin payload 长度必须为零。

## 3. 控制 Envelope

请求示例（实际摘要应为完整小写 SHA-256 十六进制值）：

```json
{
  "requestID": "1",
  "epoch": "host-generated-epoch",
  "connectionID": "host-generated-connection-id",
  "method": "StartSession",
  "timeoutMs": 5000,
  "params": {
    "operationID": "host-generated-operation-id",
    "sessionID": "host-generated-session-id",
    "preparedID": "guest-generated-prepared-id",
    "workspaceRef": "imported-workspace-id",
    "workspaceDigest": "sha256:..."
  }
}
```

`requestID` 使用不带前导零的十进制字符串，范围 1..2^64-1；每个 connection 单调递增、不复用。`streamID` 是头中的 uint32，仅标识终端 attachment；`operationID` 是 Host 生成的 UUID，仅标识可重试业务操作，三者不能互相替代。

Response 具有相同的 `requestID/epoch/connectionID`，且 `result`、`error` 恰好出现一个。没有 method 字段；Host 用原请求方法选择 `HelloResponse` 等对应 schema 校验，不能只检查通用 Response。每个方法的 Response 都是该方法 SuccessResponse 与通用 ErrorResponse 的互斥联合，合法业务失败不属于协议错误。error 结构为 `{ "code": "CONFLICT", "message": "..." }`，message 最多 256 个 Unicode 字符，不携带凭据、Host 私有路径或上游秘密。

Event 具有 `epoch/connectionID/eventID/event/params`；eventID 是 connection 内递增的十进制字符串。事件只作通知，不替代可查询的状态。

Cancel 具有 `epoch/connectionID/requestID`，目标为该 connection 中原请求。没有额外的 Cancel response，原请求最终仍只能有一个 response。

JSON 必须为单个对象，不接受尾随第二个值或重复键。v0.1 不协商扩展字段，未知字段返回 INVALID_ARGUMENT；破坏 envelope 的输入终止连接。合法 envelope 的未知 method 返回 UNIMPLEMENTED。

控制 Request 先按 RequestEnvelope 校验公共字段与身份，再分派 method；未知 method 的合法 envelope 返回 UNIMPLEMENTED，已知 method 必须进一步按对应 `HelloRequest` 等定义严格校验 params。不能只按 Schema 顶层或 RequestEnvelope 放行已知方法的任意 params。握手前业务请求和错误身份优先按第 5 节拒绝，不执行方法或查询其他身份的资源。

错误码先限定为 INVALID_ARGUMENT、UNIMPLEMENTED、FAILED_PRECONDITION、STALE_INSTANCE、CONFLICT、NOT_FOUND、RESOURCE_EXHAUSTED、CANCELLED、DEADLINE_EXCEEDED、INTERNAL。传输断开是客户端本地错误，不伪造 Guest response。

## 4. 请求并发、超时与去重

- 每 connection 最多 16 个在途请求；超限请求返回 RESOURCE_EXHAUSTED。控制读循环不得同步等待进程退出；Stop 等等待由有界处理任务执行。响应写由单独的有界队列串行发送。
- `timeoutMs` 是接收完整请求后开始计算的相对预算，范围 1..30000，不传墙钟绝对 deadline；Host 自己仍执行原 context 的 deadline。
- Cancel 与完成竞态按 Guest 的实际完成点决定。取消不能回滚已经提交的 Prepare/Start，Host 超时也不能推断进程未创建。
- 收到重复或倒退 requestID 是协议错误并关闭连接；不缓存重复 request 的 response。业务重试使用新 requestID 和原 operationID。
- Host 为本 connection 已发出但已取消/超时的请求保留迟到响应识别记录。迟到的第一个 response 仍完整校验身份、方法与结果后丢弃；重复 response 或从未发出 requestID 的 response 关闭连接。非 GetState 请求每 connection 最多 4096 个，历史记录不提前淘汰；GetState 不占用该历史容量，而有独立的 16 个尚未完整验证响应的账本槽位（包含调用方已取消者）。发送登记前原子占额，满额返回 RESOURCE_EXHAUSTED 且不发送；完整验证成功或错误 response 后释放，连接终止释放全部只读登记。序号始终 uint64 严格递增、不得重用，耗尽明确拒绝。Guest 同样不把 GetState 计入历史请求数，保留原业务任务与响应队列限额；operation 去重不受影响。旧镜像仍有全部请求4096限制，必须与新 Host 配套装配，不通过自动重连规避。
- Prepare/Start 的 operation 去重属于 Guest 的 boot epoch，控制重连不清空。最多保存 256 个操作，直至 VMM 重启；不按 TTL 淘汰，满时拒绝新操作。GetState、Cancel 和 Stop 不占用新操作槽位。
- 同 operationID、同方法及同语义参数返回原结果；相同 ID 的并发调用等待同一执行结果；不同参数返回 CONFLICT。比较结构化字段，不能直接比较 JSON 字节或依赖 map 遍历顺序；规范摘要在 schema 固定后统一实现。
- 一旦操作被接纳，占用记录并保留最终成功或失败结果。未接纳的参数错误、权限错误或容量错误不建立记录。接纳后取消产生的最终结果也保留；不能用相同 ID 偷换参数。
- `StartSession` 的启动提交点必须在单一 session 创建路径内定义；请求响应丢失后，Host 用 GetState 查询，或使用原 operationID 重试，不能换 ID 盲目再启动。

控制响应/事件共用容量 32 的 FIFO 写队列，只有一个写者；入队预算 2000 ms，每帧实际写预算 2000 ms，两者分别计时且不超过连接剩余生命周期。任一超时使 connection 失效并唤醒全部等待者，不无限堆积响应；队列饱和不通过无限生成后台写任务规避。该数值已用于原型，容量与超时边界仍须专门验收。

以下超时是候选接收/清理策略，必须随契约测试落地：无首字节的空闲读取可持续等待至连接取消；任一端口收到首字节后，完整该帧的预算为 2000 ms，超时按半帧失败处理。控制发送仍用上述写预算；终端 Data 写可等待自身背压，直到 attachment 取消或退出后排空预算届满。调用方有更早 deadline 时取更早值，不把超时解释为操作未执行。

## 5. Hello 与连接边界

Host 先打开该 VM 的专属端点，每次连接生成新的 connectionID。第一个成功请求必须是 Hello；握手前其他业务请求拒绝但不建立授权。Hello 的 params 包含 Host 认定的 vmRef（现有 `vm.VMRef.ID`）、一次性 challenge 和 supportedVersions；epoch、connectionID 使用 envelope 字段，不在 params 重复传入。Guest 回显 challenge，并返回 version、guestBuild、诊断 imageDigest。随后 Host 必须 GetState 查询已有 session 状态。

身份来自 Host 的端点与句柄控制，challenge 只用于关联这次握手，不是 Guest 可信证明。Host 在发起 Hello 前校验自己选用的镜像。

Guest 首次 Hello 记录本次 boot 的 vmRef 与 epoch；同次 boot 的后续 Hello 不允许替换二者。同一控制连接只允许一次成功 Hello，重复 Hello 返回 FAILED_PRECONDITION。未握手的业务请求返回 FAILED_PRECONDITION；首次握手身份与本 boot 已固定身份不符时，返回 STALE_INSTANCE 后使连接失效。握手后收到错误 epoch 或 connectionID 是协议错误，直接关闭本连接，Host 记本地 stale-identity 错误，不承诺错误身份能够收到业务 response。

控制 EOF/协议错误立即使当前 connection 授权和 terminal 绑定失效，Host 撤销该连接依赖的 Gateway binding；Guest session 不因 socket EOF 自动退出。新连接必须重新 Hello、GetState 和显式重绑，不能复活旧权限。旧帧、旧事件和旧 terminal token 不得在新 connection 生效。

物理重连、设备 EOF 的可靠识别和新旧帧清理必须在真实通道原型中验证，不能只用内存 fake 证明。无法可靠区分连接代次时应关闭本轮连接并停止重用，不能把文本实验的 reopen 循环直接移植为生产恢复机制。

## 6. 生命周期和方法

固定顺序为 Host Stage → VM Create/Start → Hello → Guest Import → Gateway Bind → PrepareAgent → StartSession → AttachTerminal。Stage 和 Import 是同一工作副本的两个阶段；最小 spike 使用镜像内置固定摘要工作区，不能宣称产品 Import 已完成。

| 方法 | 关键输入 | 关键结果与约束 |
| --- | --- | --- |
| Hello | 版本、VMRef、epoch、connectionID、challenge | 绑定连接；不得接受 Guest 自报身份覆盖 Host 映射 |
| GetState | 无业务参数 | 已准备结果、当前 session、固定退出结果；用于恢复查询 |
| PrepareAgent | operationID、内置 Manifest 引用及摘要、配置摘要、workspaceRef/digest | preparedID 绑定上述内容；不接收任意命令/Host 路径/凭据 |
| StartSession | operationID、sessionID、preparedID、workspaceRef/digest、初始 rows/cols | 返回 SessionInfo；核对准备与导入摘要，创建 PTY 后设置尺寸再启动非 root 进程 |
| AttachTerminal | sessionID、Host 分配的 streamID | 回显 streamID，返回单次 bindToken；此时不发送终端字节 |
| ResizeTerminal | sessionID、streamID、rows、cols | 两维均为 1..65535；对实际 PTY 设置窗口 |
| DetachTerminal | sessionID、streamID | 释放 attachment，不停止进程；重复 detach 成功 |
| StopSession | sessionID、graceMs、forceAfterTimeout | graceMs 为 1..30000；停止 Guest 进程组，成功必须表示已观察到子进程退出 |

`DetachTerminal` 已在原型验证；Host `Terminal.Close` 映射为该方法，正常返回表示旧读写循环均已结束、Guest 旧设备句柄已关闭、授权已释放，而非只发送取消信号。若请求取消或超时，完成状态未知，先 GetState/重建连接，不能立即假设已可重绑。重复同一已释放 attachment 的 detach 成功；请求指向另一当前 attachment 时返回 NOT_FOUND，不能关闭新的 attachment。

MVP 每 VM 同时仅一个工作负载进程组；主进程已退出但组内仍有存活成员时不能 Start 新 session。每 boot 最多保留 64 个 session，退出结果不淘汰；操作记录最多 256 个。先查原 operationID，再检查容量；满后新创建返回 RESOURCE_EXHAUSTED，已有操作的重试、查询和停止仍可执行。

SessionExited 事件包含 sessionID 与固定的 code/signal/reason，必须来自真正的子进程等待。Guest 在状态存储中提交退出结果后才发送事件。WaitSession 是 Host 对事件和 GetState 的等待封装，不占用一个长驻 wire request；订阅和状态查询须避免“查询后订阅”丢事件。

## 7. 终端 attachment

Host 分配 connection 内不复用的非零 streamID；同一时刻只有一个 attachment。AttachTerminal response 成功后，Host 才向 terminal 端口发送 Bind，包含 sessionID、epoch、connectionID 和一次性随机 bindToken。Guest 核对控制侧记录后返回 Bound，然后才能开始 Data。token 不构成对恶意 Guest 的认证。

Guest 从 AttachTerminal 成功响应完整写出开始给予 Bind 最多 2000 ms；未绑定即撤销本授权。Host 从发送 Bind 开始等待 Bound 最多 2000 ms，且不超过 AttachTerminal 调用的剩余 context；失败须关闭端口并有界 Detach，不能把“控制 RPC 成功”当作已获得 Terminal。没有跨连接透明重试；新授权必须使用新 streamID/token。

Bound 必须先于首个输出 Data 写入端口；Host 必须收到 Bound 才发送输入。旧 token、未知 stream、重复 Bind/Bound、无 attachment 的 Data、FIN 后的同向 Data，均中止该 attachment；不得污染控制解析器，也不借此停止 Agent。

终端 Data 是 PTY 原始字节，包括 ANSI、Ctrl+C/Ctrl+D；不是 JSON，也不转换换行。Fin 是流方向 EOF：Host Fin 停止输入转发，但不能等同于向 PTY 注入 Ctrl+D；Guest Fin 表示输出已结束。PTY 不具备 pipe 那样的输入 half-close，不能假装关闭 PTY 一半。Abort/Detach 释放两向 attachment，进程继续运行。

每方向最多一个 16 KiB 待写数据帧，采用独立物理端口的阻塞背压，v0.1 不引入 credit/window 消息；控制循环不等待终端写完成。已绑定时另有最多 64 KiB PTY 待发缓冲，满时暂停 PTY 读取；未绑定/已 detach 时继续排空，溢出丢弃最旧字节并累加 session 生命周期内的 droppedBytes。Bound 返回该计数的无符号十进制字符串（不是 JSON number），随后重放保留内容；不保证终端输出持久化或恰好一次交付，已从缓冲取走但未确认送达的字节也可能在断线时丢失，droppedBytes 不包含这类损失。

慢终端输出不得阻止 StopSession 终止进程；达到停止宽限期后按显式策略处理进程组。进程退出事件与尾部终端字节走不同端口，Host 不能假定收到 SessionExited 时已经收到所有输出；只有输出收集结束且缓冲已排空后才能发送终端 Fin。Host 观察进程退出后给予终端最多 2000 ms 排空时间（包括收到 Fin）；超时本地将本次输出标记为不完整，主动 Detach，不虚构 Fin。Detached 缓冲仍按上述规则排空与丢弃。2000 ms 排空策略是待生产验证的候选，不宣称原型已覆盖该超时分支。

## 8. 精确字段与调用契约

### 8.1 值类型与 JSON

所有方法的 params、result 和事件均使用 Schema 中独立的具名类型，必填字段不得靠 Go 零值补全。成功无返回值使用 `{}`，不使用 `null`；可空字段必须显式为 `null`。禁止重复键、非 UTF-8、非单一对象及未知字段；Schema 验证前仍须由解码器检查这些字节条件。request/eventID 和 droppedBytes 必须额外检查 uint64 上界，正则表达式不代替数值检查。JSON 整数字段不接受字符串或小数。

| 类型 | 表示与生成方 | 生命周期 |
| --- | --- | --- |
| vmRef | `vm.VMRef.ID` 原样编码为字符串，沿用 1..64 ASCII 字符校验 | 持久 VM 资源 |
| epoch | `vm.VMInstance.Epoch` 原样传递，不重新生成；通道候选额外要求 1..128 字符 | 一次 VM 启动；通道限制不修改 VM 接口 |
| connectionID / operationID | Host 生成的小写规范 UUID v4 | 一次连接 / 一次 boot 中的操作 |
| sessionID | Host 生成的 OpaqueID | boot 内不可复用 |
| preparedID | Guest 生成的 OpaqueID，引用不可变准备记录 | boot 内不可复用 |
| workspaceRef / manifestRef / guestBuild | OpaqueID：首位 ASCII 字母或数字，其余字母、数字、`_`、`-`，总长 1..128 | 对应已登记资源或构建标识；不是路径 |
| Digest | `sha256:` 后恰好 64 个小写十六进制字符 | 固定内容的摘要 |
| challenge / bindToken | 分别由 Host / Guest 使用安全随机源生成的 32 字节，编码为 64 个小写十六进制字符 | 一次握手 / 一次 attachment |

`SessionInfo` 必含 sessionID、preparedID、workspaceRef、workspaceDigest、state、groupState、exit。state 只描述主进程 running/exited；groupState 描述进程组 running/empty/unknown，不能用主进程退出推断组已清空。exit 为 null 或固定 SessionExit；主进程退出后不可再改写。unknown 阻止启动下一组，不是默认成功。

SessionExit 必含 sessionID、code、signal、reason。正常退出 code 为 0..255、signal 为空；信号退出 code=-1、signal 使用 Linux 符号名（例如 SIGKILL，不使用 Go 本地化描述 `killed`），实时信号用 `SIGRTMIN+N`。reason 为 process-exited、stopped 或 forced：自行退出、Stop 的 TERM 终止、显式 KILL 终止；取消与连接故障不构造进程退出结果。并发 Stop 由单一停止协调路径记录最终信号动作，退出结果只提交一次。

GetState 必含 prepared（最近一次准备记录或 null）、sessions（按 sessionID 字节序排列的全部历史，最多 64 个）、currentSessionID（最近一次创建的 session 或 null）、attachment（当前连接的授权摘要或 null）。attachment 含 sessionID、streamID、connectionID、state（awaiting-bind/bound/detaching），不返回 token。跨字段引用必须存在且同 session 的 exit.sessionID 一致；这些关联由状态校验补充，不能只依靠 Schema。

### 8.2 方法输入输出与完成点

| 方法 | params 定义 | 成功 result 定义 | 成功含义 |
| --- | --- | --- | --- |
| Hello | HelloParams | HelloResult | 绑定 vmRef/epoch/connectionID，回显 challenge，返回版本、build、诊断镜像摘要；不授予 Gateway 权限 |
| GetState | 空对象 | GuestState | 同一逻辑时刻的只读快照，包含已固定退出结果；不是事件历史 |
| PrepareAgent | PrepareAgentParams | Preparation | Manifest、配置和已导入工作区均校验完成，返回不可变准备记录 |
| StartSession | StartSessionParams | SessionInfo | PTY 创建、初始尺寸与降权设置完成，实际创建进程后提交唯一 session；可能响应前已退出 |
| AttachTerminal | AttachTerminalParams | AttachTerminalResult | 登记待绑定授权，回显 streamID 和单次 bindToken；尚不能发送 Data |
| ResizeTerminal | ResizeTerminalParams | 空对象 | 已对当前 attachment 所属 PTY 应用 rows/cols |
| DetachTerminal | DetachTerminalParams | 空对象 | 旧读写任务结束、设备关闭、授权释放，session 继续存活 |
| StopSession | StopSessionParams | 空对象 | 主进程退出已固定，且进程组已无可执行成员；僵尸不视为可执行成员 |

PrepareAgent 的 manifestRef/digest、configDigest、workspaceRef/digest 全部必填；实际 Manifest、配置和工作区必须由已有导入/镜像登记记录解析，不允许凭摘要创造配置，也不传命令、任意环境变量、路径或凭据。记录不存在返回 FAILED_PRECONDITION。运行中不可替换准备结果；旧 preparedID 保留，不能指向新内容。Import 与 Gateway Bind 不属于本规范的方法，它们的独立契约仍待定义。

StartSession 的 operationID、sessionID、preparedID、workspaceRef/digest、rows、cols 全部必填；工作区必须与准备记录一致。禁止同时存在另一未清空的工作负载组。Attach/Resize/Detach 严格匹配 sessionID 与 streamID，不能把另一 session 的流当成本 session。Stop 的 forceAfterTimeout 必填，不默认为 true。Stop 发送 TERM，按 graceMs 等待；届满且未授权 KILL 返回 DEADLINE_EXCEEDED，允许强停时发送 KILL 并继续等待实际退出/组清空，但仍服从原 timeoutMs 和本地 context。取消不撤销已经发送的信号，重复 Stop 可继续等待；不强行把整个宽限期塞入短于它的请求预算。

参数结构或范围错误返回 INVALID_ARGUMENT；资源引用不存在返回 NOT_FOUND；依赖未完成、错误生命周期返回 FAILED_PRECONDITION；同 operationID 内容冲突或 sessionID 复用返回 CONFLICT；容量不足返回 RESOURCE_EXHAUSTED。进程创建、设备或文件等内部错误返回 INTERNAL，不能一律包装为 DEADLINE_EXCEEDED。未提交失败不伪造 SessionInfo/SessionExit。

### 8.3 操作参数规范化与提交

去重键为 `(epoch, operationID)`，方法名参与相等性判断。先校验方法 Schema 的结构和范围，再按以下固定顺序构造 JSON 数组，UTF-8 编码，无空白、无 Unicode/斜杠转义，数字用十进制整数、布尔用 `true/false`；全部字符串受前述 ASCII 类型约束。对数组字节计算 SHA-256。JSON 字段顺序和空白不影响该参数指纹，operationID、requestID、timeoutMs、connectionID 不进入数组。

```text
PrepareAgent: ["PrepareAgent",manifestRef,manifestDigest,configDigest,workspaceRef,workspaceDigest]
StartSession: ["StartSession",sessionID,preparedID,workspaceRef,workspaceDigest,rows,cols]
```

保留结构化参数和指纹，比较结构化字段，不以哈希替代参数相等性。结构/范围校验后先查已有 operation：同参共享或返回固定结果，异参或跨方法复用返回 CONFLICT；只有新 operation 才检查引用、生命周期、权限与容量，并在进入执行前原子预留记录、扣减容量。检查与预留必须在同一串行状态边界内，不能被并发新操作穿透。预留前的失败不留记录；预留后的成功、启动失败、提交前取消均固定结果，直到 boot 结束。已提交的创建不能因调用方取消改为“未创建”；重试必须返回原提交结果。准备的提交点为不可变 Preparation 入表；启动的提交点为实际进程已创建且 SessionInfo 已登记的同一串行路径。重试返回的 SessionInfo 是原提交快照，即便主进程后来退出也不改写缓存；当前状态由 GetState 查询。

共享状态契约现已在 Fake 上覆盖以下用例（生产 Handler 尚未实现，不能记为真实 VM 通过）：已有成功 Start 后，用原 operationID 改为一个格式合法但不存在的 preparedID，返回 CONFLICT；用原 operationID 和原参数在进程已退出或当前运行状态变化后重试，仍返回原结果。新 operationID 使用不存在引用才返回 NOT_FOUND 或对应依赖错误。

若 guestd 丢失 boot 操作记录，禁止在同一 boot 静默重建空表并继续 Start。生产实现须保留所需恢复记录，或将该 VM 标记不可恢复、销毁并以新 epoch 重启；当前 spike 没有验证此故障分支。

### 8.4 Host 消费方接口

接口唯一代码定义位于 `internal/host/guest/client.go`：`guest.Client` 与 `guest.Terminal`。DTO 和标量来自 `internal/protocol`；`AttachTerminal` / `WaitSession` 使用 `protocol.OpaqueID`，`Resize` 使用 `protocol.Dimension`。本文不再复制另一份 Go 签名。

`vm.VMInstance` 沿用 `internal/host/vm`。真实连接装配层应将独立 control/terminal 私有句柄绑定到该 instance；Hello 的 vmRef/epoch 由绑定自动填入，调用方不能覆盖。Fake 使用 `fakeguest.New(Config)` → `Boot.Connect(instance)` 模拟此边界，不提供真实通道。

Terminal.Close 自带最多 2000 ms 清理预算；失败仍关闭本地句柄并返回错误，不能把 Guest 未确认释放当作成功。Client.Close 使本连接上下文失效并回收本地任务，不隐式 StopSession。两者重复调用返回已固定的清理结果，不能每次生成新的 Detach。本地连接错误、输出不完整和远端业务 Error 为可区分的类型，不复用 SessionExit 表达。

Terminal.CloseWrite 串行等待先前输入帧写完后发送一次 Fin，成功后后续 Write 返回本地输入已关闭错误；重复 CloseWrite 返回首次结果。调用前已取消也算首次尝试，固定取消结果且不发送 Fin；并发调用在首次尝试未完成时可各自取消等待，不改变首次结果。它不向 PTY 注入 Ctrl+D、不关闭 PTY master，也不取消输出 Read；需要停止工作负载时显式 StopSession。

WaitSession 先在唯一控制读循环维护的状态表注册等待，再触发 GetState；SessionExited 和查询结果合并到不可逆的固定退出结果。相同 session 的不同退出结果属于协议错误；较早快照的 running 响应可能晚于退出事件到达，忽略其过时运行态，不能回退。等待者取消只移除自身订阅，不发送 Stop、不影响其他等待者。连接断开立即唤醒全部等待者；显式重连后由新 client 的 GetState 恢复，不承诺旧调用透明续接。它等待的是主进程 SessionExit，若需要保证进程组为空，调用 StopSession，二者不混用。

## 9. 原型差距与冻结门槛

当前原型验证了传输/并发及固定工作负载生命周期，但有意简化业务字段。以下原型差距仍须在生产 Handler/Client 中完成适配和契约验收，不能声称现有原型与 Schema 互通：

| 目标契约 | 现有原型 / 后续工作 |
| --- | --- |
| 独立方法 params、严格拒绝其他方法字段 | 原型共用 params 大结构体；生产解码须按 method 校验 |
| Hello supportedVersions/build/imageDigest、32字节 challenge/token | 原型 Hello 仅回显版本/challenge，token 使用较短随机值；补齐正反 fixture |
| 真实 Manifest/config/Import 摘要和引用 | 原型仅 `probe-v1`、`sha256:probe-workspace`，不提供生产导入能力 |
| 固定成功/失败 operation、参数指纹、256操作/64 session容量 | 原型主要覆盖成功去重；接纳失败、guestd 丢状态、所有容量边界待测 |
| SessionInfo/GuestState/SessionExit 精确字段 | 原型带 pid/starts 等诊断，缺少完整 groupState/reason；不得直接作为 DTO |
| uint64 字符串 droppedBytes、统一 Linux 信号名 | 原型 droppedBytes 为 JSON number，signal 为 Go 描述，需统一映射 |
| 终端退出后2000ms排空/中止 | 正常尾部已验证；慢读到超时的输出不完整分支待测 |
| 无实验控制台协作的生产连接代次边界 | 原型重连依赖控制台关闭诊断；生产端点生命周期与恢复策略仍须真实验证 |
| 严格 SessionExited 事件集合、半帧和绑定时间预算 | 原型另有 ConnectionClosed 实验诊断事件；新增超时分支尚未验证 |

完成这些适配后，Host、Guest、Fake 及共享契约测试必须消费同一 Schema/fixture。当前负载 fixture 验证候选字段，共享 `internal/protocol/testdata/frames.json` 验证帧字节；Go 公共编解码、Go spike 和 Python spike 已使用同一文件；二者用途不同，不能把前者称为已有真实设备全链路通过。

冻结仍需交付：

1. 上述 schema 的精确字段、请求参数规范摘要、队列容量和全部超时常量；目前仅是候选，不能声称所有字段已冻结。
2. 独立最小 Host/Guest 原型：在真实设备上完成 Hello → GetState → PrepareAgent → StartSession → Bind/Bound → PTY 输入输出 → Resize → StopSession → SessionExited/GetState。
3. 未 Hello、旧 epoch、旧 connection、旧 token、控制/终端分别断线、Cancel 与提交竞态、并发同 operation 重试和参数冲突的确定断言。
4. 半帧、超长帧、未知 kind/stream、重复与迟到响应、背压与停止的真实通道验证。
5. 共用黄金字节 fixture、编解码和通道适配器测试；以后 fake 与真实实现共享这些 fixture，不能各写自己的 JSON 解释。
6. 对照 `interfaces-and-fakes.md` 第 7.3 节逐项记录结果并做只读 review；未完成项明确标注。生产实现先复用本轮共享基础，任务交接见 `../docs/tasks/guest-agent.md`；完成后集中复审和冻结，不各自重定义协议。

固定工作负载的正常/异常业务链路已有真实 TCG 证据；新增生产字段、完整容量与恢复策略、共享 Fake/真实契约和双 reviewer 流程尚未全部完成，当前仍不开放按本文独立并发生产实现。
