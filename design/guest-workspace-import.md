# Guest Workspace Import v0.1

状态：codexx 两轮设计评审允许进入可丢弃 spike；生产实现前的禁止覆盖发布意见已采纳，可丢弃真实TCG原型已完成并经第三轮复审；生产导入已实现，codexx最终代码复审通过，见实现记录。基线 957fc53。只覆盖 Linux/amd64、一次 boot 导入一个工作区。沿用 workspace bundle v1，不加入导出、增量同步、自动重试或通用文件服务。

## 生命周期与边界

Host Stage → VM Start → Hello → GetState → Import → Gateway Bind/Ready → Prepare → StartSession。GetState必须成功且无准备、无会话；不符合时不得开始导入。Host Stage 可在启动前完成；Guest 接收必须在 Hello 成功后，Prepare 之前。Import 只建立真实工作区，不替代 Manifest 配置安装。没有 Host 文件系统挂载，QEMU 继续 -nic none。

Guest 启动时 /workspace 是 root 拥有且不可由 UID1000 写的目录，project 不存在。导入守护进程为 root，设备 0600，不继承给工作负载。落盘 staging 位于同一文件系统的 /workspace/.import-随机值，外层始终0700/root；归档为staging/bundle.tar，待发布项目为staging/tree。只有完整校验成功才将tree rename为/workspace/project，外层不发布。项目文件与目录最终归 UID/GID1000，文件0644/0755、目录0755，不保留其他元数据；更改所有权时仍由0700 root staging隔离。发布后允许 Agent 修改，这是可写副本，登记摘要仅表示导入时基线，不代表运行中内容永远不变。

每 boot 仅接纳一次 Begin；导入成功后不能替换项目。失败、取消、控制断线、协议错误均使当前导入和控制连接失效，Host 回收本次 VM；不支持同 boot 重传或恢复。成功响应丢失不能假定失败未落盘，仍回收 VM 后新 epoch 再导入。Guest 重启守护进程沿用既有不可重入标记，不能丢失登记后重新服务。

## 物理通道与身份

新增 safeagent.workspace（nr=4）、workspace.sock，独立于 control、terminal、proxy。QEMU Channels.OpenWorkspace 只交付一次，并复用当前 VMInstance 的运行记录和句柄跟踪；不接受调用方传入 socket 路径。VMBackend/Driver 抽象无需新增方法。

Host 具体导入客户端接收已经 Hello 的 *rpc.Client 和同 VMInstance 的 *qemu.Channels；与 proxy 一样核对 Identity，并监听控制 Done。Guest Begin 携带 vmRef、epoch、connectionID，与 Boot 当前已完成 Hello 的控制身份逐项匹配；在 Boot 锁下预留唯一导入资格，未 Hello、已有准备/进程、已导入或已预留均拒绝。身份字段只绑定既有通道状态，不是 Guest 远程证明，也不允许工作负载凭字符串取得 Host 权限。

Guest 与 Host 导入双方始终有有界读写期限，不能依赖 Guest 设备关闭必然让 Host socket 收到 EOF。控制失效关闭 workspace。Host workspace模块只关闭自己拥有的workspace句柄、返回错误并取消装配context，不直接关闭control或清理VM。Guest workspace模块只向Guest入口报告错误；Guest入口是设备/控制生命周期所有者，由它关闭control并等待workspace任务退出。Host装配者按Revoke（若已有绑定）→关闭连接→独立context Stop/Wait/Destroy收尾。现有proxy.Run已经拥有后半段生命周期：Import在调用它之前串行完成；外层装配在进入proxy.Run前负责失败清理，进入时明确移交所有权，此后只由proxy.Run清理，不能外层defer和其cleanup回调各执行一次。导入任务结束且所有回调退出后才可移交，不新增并行VM清理者。

## 线协议

独立 internal/workspacewire，不修改已冻结 SAGP control/terminal v0.1。16字节头：magic SAWI 4字节、major=0、minor=1、kind 1字节、reserved=0、payloadLength uint32大端、transferID uint32大端。每帧 payload 最大16KiB；transferID本版本固定1，不复用。kind固定 Begin=1、Ready=2、Data=3、End=4、Committed=5、Rejected=6。头错误立即关闭，不扫描magic。

方向和顺序固定：Host Begin → Guest Ready → Host Data* → Host End → Guest Committed。元数据是固定字段、拒绝未知/重复键和尾部的UTF-8 JSON；Data为包原始字节。Ready/End为空；空工作区仍包含非空tar。Begin字段：vmRef、epoch、connectionID、workspaceRef、workspaceDigest、archiveBytes。workspaceRef使用Host Baseline.ID，摘要绑定规范manifest而非tar；archiveBytes为正整数且不超过上限。Committed精确字段workspaceRef、workspaceDigest、fileCount、totalBytes，Host与Baseline逐项比较；不接受Guest返回Host路径。Rejected仅包含固定code枚举 INVALID_STATE、INVALID_BUNDLE、LIMIT、IO，不包含内部错误文本或凭据，随后双方终止该boot业务。

从Begin开始总预算120秒，单帧从首字节到完成最多5秒；等待Ready、首个/下个Data和End也最多5秒；Host从End写完起等待Committed最多30秒（给Guest校验、提取与发布），Committed一旦开始仍须5秒内读完；所有阶段，均取总预算与调用context剩余时间最小值。总预算覆盖接收、校验、提取、发布；本地文件系统调用取消为协作式，不能承诺中断单次内核调用。传输首字节等待Hello后由Host调用context限制，Guest等待Begin受控制生命周期限制。超时不放大、不重试。

Guest累计Data恰好archiveBytes，少/多、End负载、错误方向、未知kind/id、半帧、接收期间重复Begin或提前End均失败。首次合法End结束归档输入，随后校验器从私有归档句柄读到真实EOF；不能直接把永久设备流交给ValidateBundle等待物理EOF。归档尾部额外字节仍被规范校验拒绝。首次End后不再消费Host帧，验证/提交/回执后关闭唯一通道；额外帧不会成为第二次操作，本版不提供关闭前逐一拒绝额外帧的确认，也不新增后台读者。无每帧goroutine，固定接收任务与串行写者，背压由设备阻塞和deadline实现。

## 包验证与落盘

首版固定上限：4096文件、单文件16MiB、内容总量64MiB、相对路径1024字节、归档128MiB；Host导入必须在发送Begin前检查Baseline的ArchiveBytes/FileCount/TotalBytes，以及规范manifest中每个文件的大小和路径字节长度；超Guest固定限额即返回workspace.ErrLimitExceeded，不发送Begin，并由装配者回收本次VM，不协商提高Guest限额。Store为实际导入消费者增加OpenImportForGuest(ctx,id,limits)返回Baseline、Manifest和已定位到起点的io.ReadCloser；与现有OpenImport共用内部校验路径，使用Store限额与Guest限额逐项较小值完成原本就必须执行的一次完整校验，避免客户端再重复扫描。原OpenImport仍执行现有Store限额契约。此API变更只服务实际跨Guest限额消费者，不新增通用工厂或兼容模式。归档先流式写入root私有staging/bundle.tar（0600），接收容量受archiveBytes和归档限额双重约束；结束后从同一已打开归档句柄执行一次规范验证与提取，产物始终处于root私有区。归档不向Agent暴露。最大临时内容占用为归档128MiB加提取64MiB及有界元数据；这是逻辑字节限额，不是底层文件系统物理占用保证。ENOSPC明确失败。

将Host包格式公共部分迁至internal/workspacebundle：Limits、Entry、Manifest、规范路径策略和校验；Host保持既有API，通过类型别名和委托保持调用方兼容。Guest不依赖internal/host/workspace的Store。只维护一套规范头/PAX/路径/摘要解析器，通过同一遍历核心提供Validate与Extract，Extract在写入私有文件时计算并验证哈希，错误时整个staging作废；整个目录直到所有条目及tar尾校验后才可发布。不使用宽松tar解包器，也不先完整Validate后再做一次相同校验。

落盘只使用可信根目录句柄和逐组件openat/mkdirat，父目录O_DIRECTORY|O_NOFOLLOW，文件O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC，拒绝链接、设备、FIFO、硬链接条目和覆盖。已有路径策略拒绝.git等危险组件。不能以字符串Clean/Join代替根约束，不能调用tar命令解压外部包。原始tar扩展头按既有规范逐字节验证，中文长路径复用同一fixture。

接收/提取/设权限完成，删除私有bundle后，在Boot锁下复核控制身份有效和context、确认无准备/会话，通过根句柄调用renameat2(RENAME_NOREPLACE)将staging/tree原子发布为project并登记Imported；目标已存在则失败，禁止普通rename覆盖{workspaceRef,digest,fileCount,totalBytes}。锁仅覆盖最终本地rename与内存登记，不持锁传输或哈希。此后取消不撤销已经发布的记录；Committed写失败使boot失效并回收。Guest成功是当前VM内可见发布，不承诺重启/掉电持久，不新增数据库。普通失败清理本次staging，清理错误显式合并返回；Host独立VM回收处理不可清理的残留。

## 与 Prepare 的衔接及文件归属

Boot增加受同一mu保护的importState（empty/receiving/committed/failed）和可空Imported记录，预留时empty→receiving并阻止Prepare/Start；失败→failed且不再接纳业务，发布时receiving→committed并保存Imported。Imported字段固定WorkspaceRef、WorkspaceDigest、FileCount、TotalBytes，提供只读值快照；不把绝对路径放入wire或调用方可修改的记录。

本轮同时闭合现有Prepare：镜像可信装配显式区分旧fixed测试模式与imported模式，模式不能由请求选择。旧fixed模式保留现有契约；imported模式的Prepare保留Manifest/config与不可变Config.Content的匹配，workspaceRef/digest改为匹配committed的真实Imported，禁止改写Config.Content。executeOperation生成新的Preparation时，仅拷贝固定Manifest/config字段，并将workspace字段填为Imported的值；不能直接返回旧config.Content里的固定工作区。此处准备仍仅登记既有固定程序，并不声称支持Manifest配置安装。Start沿用Preparation匹配，并在构造Runner配置时将Dir固定为/workspace/project，其余UID/Args/Env不在本轮扩展。原模式Dir保持镜像登记值。未来Manifest Prepare也必须消费同一Imported记录，不并存另一套导入成功标记。

不把工作区摘要充当Manifest摘要。后续Manifest Prepare必须查真实Imported并匹配workspaceRef/digest，且启动Dir固定该项目；ManifestDigest绑定内置执行包、ConfigDigest的具体映射与PlanDigest分开在Prepare设计冻结。原固定工作负载测试保留独立镜像模式，不把fixed-workspace测试记录当作用户导入成功。

预计修改：internal/workspacebundle、internal/workspacewire、internal/guest/workspace、internal/host/workspaceimport；以及必要的Boot预留/登记、QEMU第四通道、Guest入口和镜像脚本。Manifest包、Runner公共类型/环境和现有控制DTO不在本轮修改范围；Boot传给Runner的Dir是本轮必要衔接。并行Prepare工作应先消费上述Imported契约，Boot与入口由一个任务统一修改。

## 编码前真实验证门槛

设计经codexx复审后先写可丢弃spike，不写生产导入。使用独立initramfs/overlay/Unix端点，复用device.Conn和当前microvm,acpi=off，不修改共享素材。

1. 第四设备可发现，Host传送包含中文、二进制、零字节和可执行文件的规范bundle；Guest校验、受限根提取、rename；串口给出实际摘要、权限/所有者，UID1000进程读取项目且不能访问设备或root staging。
2. 以确定闸门验证发布前看不到project、取消/截断/篡改摘要/尾部多数据/路径穿越/链接条目均不发布，清理本次staging；不是只验证tar命令成功。
3. workspace背压期间独立控制ping和终端echo继续；Guest关闭不冒称Host EOF；Host取消和Guest关闭均解除各自阻塞任务，进程任务显式等待。
4. 基础盘前后摘要相同，每台自有QEMU都在独立清理预算内回收，不操作其他任务进程。

spike先验证设备与文件系统原语；正式线协议、真实Boot身份及Prepare门禁由后续实现验收，不把原型视为完整生产验收。任何门槛失败先定位，不直接开始生产代码。

生产验收另含共享codec黄金字节/限额/短写、真实临时文件系统提取安全、身份/epoch/提交取消竞态、Host Stage→真实Guest Import成功与失败、control/terminal/proxy旧回归，以及全仓test/vet和相关race。Fake、宿主文件系统、TCG、KVM与跨平台分别报告。真实供应商与Agent CLI不在本轮范围。

## 设计评审记录

复用原Workspace codexx session，xapi/gpt-6.1-sol/medium；未调用Claude。第一轮3项有效问题（Prepare真实登记、生命周期所有权、GetState门禁）均采纳；第二轮允许进入spike，补充的RENAME_NOREPLACE建议已采纳。另补清root私有外层/tree布局、Preparation结果字段和30秒提交响应预算。原始记录位于artifacts/review-sessions/workspace/codexx-import-design-{1,2}.txt，不提交运行记录。

第三轮设计与证据复审通过：可结束spike、作为后续实现输入；采纳发送前Guest限额检查，明确在Store既有校验路径一次完成，不再客户端重复哈希。真实结果及未验证项见[验证记录](../docs/guest-workspace-import-validation.md)。

生产实现和当前验收见[实现记录](../docs/guest-workspace-import-implementation.md)。Guest入口额外强制workspaceReady屏障，导入发布并写完回执后才允许proxy Ready；Host调用方仍按Import完成后Bind/Run的顺序组装。
