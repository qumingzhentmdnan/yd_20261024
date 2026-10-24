# 真实 Guest 实现方案分析

日期：2026-10-24。此次范围为新 worktree 中的实现分析和只读方案评审，不实现生产代码。基线 `2fabcd0` 已包含 `docs/guest-foundation.md` 列出的 protocol、Host 接口、Fake、guestcontract 及 fixture；现有 VM/QEMU 生命周期代码也已包含。任务提示词中的完整开发要求作为后续实施目标，本轮以用户要求的“分析后 review”为范围。

## 1. 现状与结论

可以复用公共基础逐层实现，但不能直接把 spike 移入生产目录。推荐顺序为真实 codec 的内存双通道 → boot 状态和操作提交 → Linux PTY → 终端状态机 → QEMU 装配和真实故障验证。

- `internal/protocol` 已有严格 DTO、方法关联、帧检查、指纹及安全随机源；未实现设备时间预算、请求账本或状态机。
- `internal/host/guest/client.go` 是消费方唯一接口，保持不变。`WaitSession` 是本地等待封装，不新增 wire 方法。
- `fakeguest` 的状态转换和故障点可作行为参考；生产 Handler 不依赖 testkit，不复制 DTO，也不把 Fake 作为真实服务端。
- `guestcontract.Run` 覆盖外部生命周期，但不覆盖完整 wire、真实设备、全部容量及恢复边界，需要分别补充。
- 当前 QEMU `args` 只有块设备，没有 virtio-serial controller、control/terminal chardev、Guest 内核设备发现配置或端点交付。仅实现 rpc 包无法完成真实 VM 验收。
- spike 重连依赖控制台 `CONTROL_CLOSED` 协调，不能作为生产恢复方案。当前协议仍为候选，落地后才谈冻结。

## 2. 文件归属与依赖

| 范围 | 建议文件 | 责任 |
| --- | --- | --- |
| Host | `internal/host/guest/rpc/` | 实现 Client/Terminal、请求账本、控制读写、退出等待及终端授权 |
| Guest | `internal/guest/` | boot 状态、方法 Handler、操作记录、attachment 与输出缓冲 |
| Linux 资源层 | `internal/guest/runner/`、`internal/guest/device/` | PTY/降权/进程组、可取消 virtio 文件读写 |
| 入口 | `cmd/guest-agent/` | 固定配置、设备与 boot 初始化、唯一守护进程、故障退出策略 |
| 受控工作负载 | 测试辅助程序及镜像构建脚本 | 持续运行、原始字节回显、窗口查询、退出尾部、忽略 TERM 子进程 |
| 测试与复现 | 上述包测试、`scripts/` 下新的正式验证入口 | 调用共享 contract/fixture，记录独立真实 VM 证据 |

共享改动必须在实施前说明：QEMU 端点装配及其测试属于 `internal/host/vm/qemu/` 的协作改动；协议错误分类若需扩展则集中放在 `internal/protocol`。不扩展 VMBackend、Driver、Process 或 guest.Client 来返回业务连接。不修改其他 worktree，也不依赖其中未提交内容。

Linux PTY 和 poll 可使用仓库已有的 `golang.org/x/sys/unix`，若生产直接导入，应将现有 indirect 标记改为直接依赖并说明原因；不新增 PTY 第三方包。可移植 Host RPC 与 Linux 设备/Runner 分离，使用构建约束明确平台支持。

## 3. 可信通道装配及代次

建议为 QEMU 驱动增加具体的 `OpenGuestChannels(ctx, instance)` 能力（名称待实施时确定，仅为 qemu 具体 API，不进入 VM 生命周期接口），返回绑定同一 VMInstance 的 control 连接和终端打开能力。RPC 构造函数接收可信 instance、control 及终端 opener；业务调用不能传入路径、epoch 或 vmRef。终端必须可在 detach 后重新打开，不能只交付一次性文件对。

Launch 为每次 epoch 创建独占 0700 运行目录，配置两个独立 socket 和命名 virtserialport。端点目录使用驱动生成的短内部名字并保留 instance 映射，避免把最长 128 字符 epoch 直接塞入 Unix socket 路径。端点打开与启动/停止/回收共用驱动的运行同步边界；旧 instance 不能借稳定路径连到新实例。返回前核验当前运行映射，退出时失效并关闭已交付连接。Host Manager 重新启动时不能仅凭磁盘中的 socket 名认领通道，必须与现有恢复策略一致。

设备适配使用非阻塞 fd 和 poll/取消唤醒，关闭必须让阻塞读写有界结束；不能假设字符设备支持 net.Conn 的 deadline。接收首字节前允许空闲等待；收到首字节后才启动整帧 2000ms 预算，用前缀 reader 将首字节交回共享 ReadFrame；不另写 framing。写预算覆盖头和 payload 的全部短写。

首版明确选择安全收缩的恢复策略：控制断线后整个逻辑连接失效，不透明重连；若无法无控制台协作证明 virtio 新旧代次隔离，该 VM 不再接受新连接，Host 停止并以新 epoch 重启。单独 terminal detach/rebind 仍须真实验证，旧读写者退出且设备关闭后才释放授权。不允许凭 sleep 或重新 open 就宣称已隔离旧字节。内存通道上的同 boot 控制重连测试可验证操作表，但不能替代真实设备恢复证明。

Guest 守护进程采用每 boot 不可重入标记与独占锁（boot ID 区分，位于由 PID1 初始化的私有运行目录），在接受设备请求前建立；进程重启或丢失内存记录后拒绝服务，由 Host 回收 VM。锁只能阻止并发实例，不能单独证明未丢状态。入口和启动脚本禁止同 boot 自动重建空操作表；标记初始化失败、异常退出及清理策略必须有故障测试。此版不实现持久化事务恢复。

## 4. Host 请求账本与 Handler

Host 使用唯一控制读循环；每个请求保留原 Request 和状态（排队、已发出、等待、取消后待迟到、已收到响应），最多 4096 条且本连接不淘汰。解码先取得严格 Response 的 requestID 以定位原请求，再用 DecodeResponse 校验全部关联和结果。第一次合法迟到响应仍须完整校验后丢弃；重复或未知响应使连接失败。序号分配与控制写入顺序串行，避免并发调用让较大 ID 先上 wire。分配过但未发出的 ID 不能被当作合法迟到响应。

Host 最多 16 个在途请求，在本地等待槽位时遵守 context，预取消不写帧。调用超时唤醒本地等待并在原 Request 写出后串行发送 Cancel；Cancel 不回滚副作用。控制失败关闭连接、取消 writer/reader、唤醒所有 pending 和 WaitSession，不隐式停止 Runner。达到 4096 次后停止接纳并显式重建 client；真实环境若尚不支持安全同 boot 重连，报告需要新 VM，不自动重试 Start。

Host 握手前由可信装配层校验选用镜像的来源与预期摘要；Hello 保存本次 NewNonce challenge，除 DecodeResponse 的关联校验外，显式核对回显 challenge 完全一致、version 属于本次 supportedVersions。镜像校验失败不打开业务通道；响应语义不匹配立即失效连接。Hello 成功后必须 GetState 并合并已有状态；恢复查询失败则不开放后续业务，关闭 client，避免用空状态猜测是否已经启动。增加格式合法但 challenge 不匹配、版本未被提议和握手后查询失败的测试。重复 Hello 仍按接口返回 FAILED_PRECONDITION。

Guest 控制读循环按序检查身份、requestID、Cancel 和请求容量，业务用最多 16 个有界任务执行。timeoutMs 自完整请求接收起算。Hello 绑定在读循环串行路径完成，防止后续业务抢先执行。控制响应/事件共用容量 32 的 FIFO、唯一 writer，入队和实际写各最多 2000ms；饱和即失效连接，不为队列无限生成 goroutine。Attach 的 Bind 计时由完整写出 response 的确认回调启动。

需要集中解决的 codec 差距：DecodeRequest 当前在返回 envelope 前同时校验已知方法 params，错误没有区分 envelope 损坏与 params 无效；而 Handler 需先判身份及握手，再对合法 envelope 的方法参数返回 INVALID_ARGUMENT。建议在 protocol 中提供具有明确阶段的解码结果/错误（保留 DecodeRequest 的严格语义），允许 Handler 识别已验证 envelope。不能只依据返回的部分 Request 或所有 INVALID_ARGUMENT 猜测阶段，也不能由 Handler 自行复制 JSON 校验。实施前补无效 envelope、合法 envelope/坏 params、坏身份/坏 params 的优先级 fixture/测试。

## 5. boot 状态、预留与提交

boot 持有固定 vmRef/epoch、全部准备记录、operation 表、历史 session、当前 attachment 的引用；连接只持有 connectionID、请求任务和授权。control 失效清理连接及 attachment，不删除 boot 记录或自动 Stop。GetState 深拷贝、排序，提交状态后才发布 SessionExited。

Prepare 只能匹配镜像中实际登记的固定 Manifest/config/workspace 内容，构建时计算摘要，启动时核验；引用不存在返回契约错误，不根据客户端摘要生成内容。固定登记应明确注明实验能力，不称为 Import/Gateway。每次成功准备生成新的不可变 preparedID，旧 ID 保留，GetState 只指最近一次。

operation 按 `(epoch, operationID)` 查重，保存方法、结构化输入、共享 Fingerprint、完成通知及固定成功/错误。顺序为结构校验 → 旧操作查重/冲突 → 新操作引用和生命周期/容量校验 → 原子预留。256 操作、64 session 的已预留容量均计入，不等进程创建后再计数。同参等待者取消只退出自身，不取消操作所有者的执行；所有者取消或断线在提交前固定失败，提交后不能改写成功。

不能照搬 Fake 的“解锁执行再检查”就去创建真实进程。新 Start 在锁内预留唯一启动资格和 sessionID，Prepare/其他 Start 必须看到该资格；锁外分配 PTY和执行受控启动。Start 返回实际进程创建后，即进入不可回滚的登记路径：即使此时 ctx 取消也登记 SessionInfo、固定操作成功并交给唯一 Wait；不能因后置 ctx 检查漏记已创建进程。创建前取消应关闭全部预分配资源并固定失败。进程极快退出时，Wait 的发布须在 session 登记之后；操作缓存为提交时独立快照，不随后续退出变化。故障闸门应覆盖预留后、创建前、创建后登记前和登记后响应前。

## 6. Linux PTY 与进程组

Runner 仅接受登记记录，不接受 RPC 命令、路径、环境或 UID。打开 PTY master/slave，设置初始 rows/cols、slave 归属及预期终端模式；固定程序使用 Setsid/Setctty 和明确非 root UID/GID、清空补充组，在 exec 前完成降权。全部非 stdio fd 必须 CLOEXEC，特别是 virtio 句柄。关闭父进程中的 slave；每个子进程唯一 Wait，不使用请求 context 自动杀进程。

主进程退出、组空和 PTY 输出结束三个通知独立维护。Linux `/proc` 检查进程组可执行成员，解析 stat 的 comm 边界，忽略 Z/X，处理进程消失竞态；无法确认则 groupState=unknown 并拒绝新 Start。kill(0) 不区分僵尸，不能替代这一检查。PID1 负责孤儿/僵尸回收，不与 Runner 争抢直接子进程 Wait；组编号复用及停止信号目标有效性必须通过生命周期同步和具体 Linux 行为验证。

Stop 使用单一协调路径管理信号动作及固定退出原因，不在持有 boot 锁时等进程或终端。并发调用保留各自 grace/force/context：无 force 的调用不能自行触发 KILL，另一个显式授权调用可以；单个取消只移除该调用的等待/后续授权，不撤销已发送信号。TERM → 等 grace → 按仍有效的显式授权 KILL → 等真正 Wait 且组无可执行成员。全部调用结束后协调任务不能继续依据已取消请求自行强停。取消、设备错误和超时不构造 Exit；signal 需规范化为 Linux SIG 名称。主进程早于残留组退出时，退出快照不改写，但 Stop 仍必须处理残留成员。

PTY Read 的 EIO 仅在 Linux PTY 已关闭的明确场景按输出结束处理，其他文件/进程错误原处返回。工作负载退出后若后代仍持 slave，不能凭 Wait 虚构 Fin。

PTY master 由 session 的单一资源管理路径拥有，boot 历史记录不拥有必须永久存活的 fd。启动失败关闭 master/slave 和全部预分配资源；成功后父进程关闭 slave。输出收集结束后先标记输出完成、禁止新的 PTY 输入和 Resize，取消并等待现有输入/Resize 对 master 的使用结束，再一次性关闭 master、结束收集及其取消资源；不能在仍有 I/O 引用时关 fd 后让编号复用。输出缓冲与 Fin 的发送可以在 master 已关闭后完成，历史仅保留不可变状态和有界尾部缓冲。主进程 Wait 本身不触发 master 关闭，残留后代持 slave 时继续收集；排空超时先 Detach，切换为 detached 排空策略，不能为释放 attachment 而关闭仍运行工作负载的 PTY。VM 整体关闭时另行取消并回收全部 session 资源。测试应逐项统计快速退出、启动失败、残留后代及多次历史 session 后的 fd/任务，退出后 Attach 仅重放现存尾部并发送 Fin，不能重新打开 PTY；输出结束后的 Resize 返回 FAILED_PRECONDITION。

## 7. attachment、背压与退出排空

授权状态为 awaiting-bind → bound → detaching → released。streamID 本连接不复用，token 使用 NewNonce。Bind 核对 session/epoch/connection/token/stream，Bound 必须在任何 Data 之前完整写出。未绑定和等待 Bound 各自遵守 2000ms 及更早 context。失败关闭终端 fd 并有界 Detach；控制连接继续可用。

每方向最多一个待写 16KiB 帧，PTY 输出另设 64KiB 环形缓冲。bound 满时暂停 PTY 读取；detached 时持续读取、丢弃最旧字节并累计 droppedBytes。输出 writer 不持 boot 锁；输入写 PTY 也必须可取消，否则 Detach 无法等待旧循环结束。Host terminal reader 向调用者交付也用有界缓冲，不能把慢消费者变成无限内存。

Guest 只在 PTY 收集结束且缓冲发空后发 Fin；事件到达不代表输出结束。Host 控制读循环观察固定退出结果后启动独立的最多 2000ms 排空计时器，即使业务不 Read 也运行。到期尚未 Fin 则中止数据读写、Detach，Read 在已保留字节后返回 ErrOutputIncomplete；不能以 EOF 代替。排空只到 Fin，不要求调用者在两秒内消费已完整收到的本地字节。附加到历史已退出 session 时也应用已知退出状态，不能漏启动计时。

CloseWrite 的输入串行器保证先前 Data 完成再写唯一 Fin，固定首次结果（包括预取消）；并发等待者可各自取消等待而不改写它。Close 的唯一清理任务自带 2000ms 预算，等待旧两向循环退出并验证 Detach；错误后仍关闭本地 fd且固定结果，不停止工作负载。清理超时不能向调用者保证 Guest 已释放，需查询/失效连接后再考虑重绑。

WaitSession 在状态表先登记订阅，再 GetState；同 session 查询可合并，避免 16 个等待者耗尽 wire 槽位。事件与查询只合并不可逆退出，过时 running 不覆盖 exit，矛盾 exit 为协议错误。所有已观察到的退出结果也用于驱动 terminal 排空，不只在用户调用 WaitSession 时启用。

## 8. 实施批次和验证

1. 集中确定 codec 阶段错误与 QEMU 私有端点交付；列出共享修改、恢复策略及平台限制。用 net.Pipe/Unix socket 接真实 Client/Handler，跑共享黄金帧、负载正反例、握手和身份优先级。
2. 接入固定登记、boot 状态和原子操作；用可控 Runner/故障闸门验证并发同参、跨方法冲突、容量预留、固定失败和提交丢响应，复用 guestcontract。
3. 加 Linux 非 root PTY、唯一 Wait、Stop 协调和输出收集；使用持续工作负载及残留子进程验证，不将快速 echo 程序作为 contract 默认启动任务。
4. 加 Bind/Bound、Detach、半关闭、有界缓冲和退出排空；测试可取消阻塞写、旧循环退出、未知 stream、旧 token、Fin 后 Data、绑定计时、慢读 Stop 和 ErrOutputIncomplete。
5. QEMU 接共享 contract 和独立 wire 故障探针；测试工厂每场景独立 client/VM，Cleanup 用独立 context 在 Client 已关闭时仍可回收。记录完整路径、构建摘要、argv、逐项结果、基础盘前后 SHA-256 和自有 QEMU 回收。
6. 每批相关测试及并发 race，最终按任务执行协议校验、go test/vet/race；代码完成后在本 worktree 同一 codexx session 评审、修复、复审。方案 review 不代替实现 review。

wire 测试至少覆盖 16 pending、32 FIFO、4096 请求账本、256 operation、64 session，取消前后各提交边界，重复/倒退 ID、未知/重复/迟到响应，半帧首字节计时与空闲不计时，写队列/实际写分别超时。容量满时查询/Cancel/Stop/旧操作仍应工作。mock 时钟/闸门用于确定顺序，不能依靠随机 sleep 掩盖竞态。

真实验证最低 Linux/amd64 microvm/显式 TCG：Hello → GetState → Prepare → Start → Bind/Bound → 原始字节 I/O → Resize → Stop → Exit/Fin/GetState；再测控制/终端分别断线、旧身份、半帧/超长帧、取消提交竞态、背压与排空超时、guestd 同 boot 重启拒绝。若采用断线即重启策略，同 boot 重连明确记为不支持而非通过；内存重连和旧 spike 均不能替代它。

## 9. 本轮验证与评审状态

本轮基线检查：协议 Schema、45 个负载正反例及 2 个指纹向量通过；go test ./...、go vet ./...、CGO_ENABLED=1 go test -race ./... 均退出成功。它们验证已有公共代码与 Fake，不构成新生产实现验收。真实 VM 和跨平台验证本轮未运行。

codexx 首轮（xapi / gpt-6.1-sol / medium）提出两项 P2：握手 challenge/version 校验及握手后状态恢复、历史 session 的 PTY 资源释放；均已补入上文。PGID 复用、PID1 唯一 Wait、virtio 取消和代次隔离保留为后续实现的强制验证项，不宣称已解决。session 和原始证据保存在本 worktree 的 artifacts/review-sessions/，不提交。

原 session 复审结论：首轮两项 P2 已在方案层面完整修复，未发现剩余具体有效问题。此结论仅针对方案，生产实现仍须完成上列验证与代码评审。
