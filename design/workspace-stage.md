# Workspace 基线与导入包设计

状态：Host Stage/Lookup/OpenImport 已实现，设计及代码均经 codexx 复审通过。本文细化 interfaces-and-fakes.md 第 5 节，不修改 Guest 控制协议。评审记录见 reviews/workspace-stage-summary.md；用户已明确停止使用 Claude。

## 1. 范围与依赖

新增 internal/host/workspace，使用标准库，不新增依赖。实现显式文件选择、不可变磁盘快照、持久化元数据、可重复读取的导入包。不是 Git checkout，不读取 Git 对象；复制用户选中的工作树当前字节，包括已修改的跟踪文件。不会挂载宿主目录。

本轮不实现 Git 默认选择器、敏感文件自动筛查、Guest 传输/解包、导出审批和 Apply。调用方必须提供用户已确认的精确文件清单；不得把空清单解释为整个目录。后续 UI/选择器负责默认 Git 跟踪文件及额外文件确认；此入口不宣称已完成敏感内容检测。

另一个 Agent 拥有 Host Guest RPC Client、Guest Handler、PTY Runner。Workspace 不修改其协议、接口或实现，不为尚未导入的工作区伪造 Guest 登记记录。Prepare 必须等未来 Guest Import 成功才能使用真实工作区。

## 2. Host API

具体类型位于 workspace 包，不依赖 protocol；应用层将 Baseline.ID/Digest 显式转换为协议标量。

- NewStore(dataDir, limits) (*Store, error)：dataDir 为调用方管理的私有持久目录，limits 必须显式提供且为正。不绑定 VM SQLite；这是可独立存活的文件资源。
- Stage(ctx, ImportSelection) (Baseline, error)：selection 包含 Host 绝对根目录 Root 和精确相对普通文件 Paths；可以为空，生成空工作区。目录由文件父路径自动生成，不支持显式空目录。
- OpenImport(ctx, BaselineID) (io.ReadCloser, error)：每次返回独立文件句柄，从头读取已发布包；Close 释放该句柄。取消后后续 Read 返回 ctx.Err；本地普通文件正在执行的单次 Read 不承诺被异步打断。ctx 不保存在 Store 中。
- Lookup(ctx, BaselineID) (Baseline, error)：供进程重启后恢复已成功发布的基线描述，不需再次 Stage。

Baseline 包含 ID、SourceRef、Digest、FileCount、TotalBytes、ArchiveBytes。ID 为 b_ 加 16 随机字节的小写十六进制；SourceRef 为 s_ 加独立随机值，仅 Host 用来关联用户源目录；同次 Stage 的 ID 可以作为后续 workspaceRef，需成功导入记录建立后才能传给 Prepare。Digest 为 sha256: 加 64 位小写十六进制，含义见第 4 节。同内容 Stage 返回不同 ID，可得到相同 Digest。

Limits 包含 MaxFiles、MaxFileBytes、MaxTotalBytes、MaxPathBytes、MaxArchiveBytes；均为精确上限，计数含零字节文件，容量用 int64，累计采用剩余额度比较避免溢出。超限不能发布部分基线。错误使用可 errors.Is 匹配的 InvalidSelection、LimitExceeded、SourceChanged、NotFound、CorruptBaseline；上下文与实际 I/O 错误保留底层原因。所有源目录和存储目录错误须去除 os.PathError/os.LinkError 中的绝对路径，仅保留操作名、允许披露的相对路径和底层 errno，通过 Unwrap 保持 errors.Is；不能直接 %w 包装含绝对路径的错误。Missing/permission 等系统错误不统一伪装为 NotFound。

本轮只抽出实际消费的最小接口，不给尚未实现的 Preview/Approve/Apply 返回空成功，不建无用途 Fake。后续工作区消费者需要 Fake 时复用临时目录真实快照，不模拟哈希。

## 3. 文件选择与复制

路径固定为 UTF-8、斜杠分隔的相对路径；拒绝空路径、绝对路径、空组件、.、..、反斜杠、NUL、控制字符、冒号，以及任意大小写的 .git 路径组件；拒绝重复、文件/目录前缀冲突。按原始 UTF-8 字节排序；大小写折叠冲突及 Windows 保留名在本轮拒绝，减少跨平台名称冲突；完整支持仍需按目标文件系统验证。具体保留名表及尾点/尾空格规则在校验器测试中固定。路径长度按 UTF-8 字节计算；检查输入数量后才建立额外索引。

Root 必须是实际目录。Linux 从 / 的已打开目录句柄开始，逐组件使用 O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC 的 openat 打开绝对 Root，拒绝根及其所有祖先路径中的符号链接，最终持有根 fd 直到 Stage 完成。选择授权绑定本次成功打开的目录对象，不承诺此前 UI 选择时的目录对象未被替换；若未来需要该承诺，UI 必须直接交付授权句柄。不能先 Lstat 再打开，不使用会跟随链接且不暴露 fd 的 os.OpenRoot 拼装这条链路，不能依赖 Clean/Join 字符串作为边界。本轮 Linux/WSL 验证，其他平台仅在安全访问能力验证后报告支持。

Linux 源文件打开使用根目录句柄逐组件 openat，父目录 O_DIRECTORY|O_NOFOLLOW，末端 O_NOFOLLOW|O_NONBLOCK；fstat 后只接受普通文件。O_NONBLOCK 防止选中 FIFO 后在检查类型前阻塞。链接、设备、socket、FIFO 全拒绝。硬链接若可由平台识别则拒绝链接数大于 1 的文件，不声称能检测文件的所有外部别名。安全打开的实现独立于选择策略，不能先 Lstat 再普通 Open。

快照复制到私有临时目录，流式计算单文件 SHA-256，并限制单文件/总字节。文件元数据仅保留可执行标志：普通文件统一 0644，可执行统一 0755；目录统一 0755。不保留 UID、GID、时间戳、ACL、xattr、setuid/setgid/sticky 位。私有 Host 存储目录 0700，内容文件 0600。

同一打开文件复制前后比较 size、mtime、平台可用的 ctime；读取数量不符或元数据变化返回 SourceChanged。该检查只发现常见并发修改，不提供全目录同一时刻快照，也不保证发现精心伪造元数据的写入。基线权威内容始终是实际复制到私有区的字节，后续导出必须按文件内容重新核对宿主，不以该检查替代冲突判断。

## 4. workspace bundle v1

包为不压缩 POSIX PAX tar，避免解压炸弹和 gzip 元数据不确定性。布局严格如下：

1. manifest.json：第一个普通文件，UTF-8 JSON，不含 Root/SourceRef/基线随机 ID。
2. files/ 下按 manifest 排序逐个放文件，tar header 名称为 files/ 加相对路径；不写目录条目，空目录不保留。
3. tar 标准结束块；无额外应用数据。

manifest 精确字段为 version（固定整数 1）和 entries 数组。entry 精确字段顺序为 path、size、executable、digest；size 为非负整数，digest 为单文件 sha256:...。使用固定 Go 结构 json.Marshal，无缩进和末尾换行；空数组编码 []。目录语义只来自文件父路径。manifest 自身设置有界大小：最多 MaxArchiveBytes，且生成过程受 MaxFiles/MaxPathBytes 限制。

Workspace Digest = SHA-256(manifest.json 原始规范字节)，绑定路径、文件字节摘要、大小和可执行位；不是 tar 原始字节摘要。接收方必须验证规范 manifest 字节、每个文件摘要/大小以及精确条目集合，不能只校验 manifest 后信任内容。manifest 不允许未知字段、重复 JSON 键、额外条目或重复路径。

tar header：TypeReg、uid/gid=0、uname/gname 为空、mtime=Unix epoch、mode=0644 或 0755；manifest 为 0644。编码显式使用 Go tar.FormatPAX；仅在 UTF-8 或长度要求下允许扩展头携带 path 键，禁止其他 PAX 键、全局扩展头、GNU 扩展、链接、特殊条目、未登记目录和文件。size 必须能由标准八进制头表示（至多 8589934591 字节），不能隐式增加 size 扩展。Go 工具链固定；PAX 扩展头命名及 path 记录编码由黄金字节样例固定，升级工具链时检查样例。读取时检查原始扩展头，不仅检查 tar.Reader 合并后的 Header；拒绝未知/重复 PAX 键及重复扩展头。UTF-8 路径允许中文，不进行 Unicode 归一化。本轮只保证 Linux 原始字节语义；跨平台解包需补充目标文件系统冲突检测，不能将本轮路径策略视为完整跨平台保证。

将来 Guest Import 必须先在私有目录完整校验再发布，失败不得登记 workspaceRef；wire 流关联、身份绑定、配额协商、导入确认和取消另行设计。本文仅冻结包格式，不给现有控制/terminal 端口新增消息。

## 5. 存储、发布与生命周期

dataDir 下使用 baselines/<ID>/ 保存 host.json、manifest.json、bundle.tar；host.json 保存 version、Baseline 各字段和 Host Root 映射，仅供 Host，读写统一使用独立的 1 MiB 上限，不占用导入包限额。Root 永不写入 bundle、Guest 配置或常规错误文本。调用方显式配置限额，不允许向外暴露 dataDir。

Stage 在同文件系统 staging/<随机 ID>/ 复制文件、生成包。每个文件及元数据写完关闭并同步，再同步临时目录；最后原子 rename 到 baselines/<ID>，同步父目录后返回。达到发布点后不得因 ctx 取消撤销已成功提交的基线；若 rename 后父目录同步失败，返回 I/O 错误，目录可能已存在，后续可 Lookup；不承诺系统掉电场景绝对持久。无成功返回不意味着肯定未写入。新 Stage 不复用旧操作 ID，不承诺幂等重试。

未发布文件不被 Lookup/OpenImport 枚举；普通错误/取消立即清理本次临时目录，清理失败返回可见错误，不掩盖原始失败。进程崩溃留下的 staging 目录不自动清理，避免误删另一并发 Stage；由后续显式维护任务处理。本轮不实现全局 GC、Delete 或 VM Destroy 联动，运行清理与用户待审核内容保留分开设计。

已发布基线不再写入；OpenImport 不读取源目录，也不重建 tar。Lookup 验证 host/manifest 的版本、ID、摘要及有界大小，OpenImport 在首次打开时流式校验完整包与登记信息，校验通过后重新定位到开头；校验耗时服从 ctx。每次打开均校验，不做全包内存缓存。原始头校验通过生成期望规范头并逐字节比较完成，包含完整 PAX 扩展头和记录，不另外编写通用 PAX parser。内容损坏或截断返回 CorruptBaseline；权限/I/O 错误保留原因，不同时标记为内容损坏。私有存储同 UID 恶意进程不在不可变保证的威胁边界内。

并发 Stage 各用独立临时目录，无全局锁；并发 OpenImport 各有句柄。Store 不持有长期文件句柄，无 Close 方法；不涉及跨 Store 的删除并发。

## 6. 验证与实施门槛

设计只读 review 解决阻塞问题后，实现 types/selection、Linux 安全复制、bundle 编解码和 store；公共包校验器放 workspace 包供未来 Guest 解包复用，不依赖 Host 私有元数据。非 Linux 先明确返回平台不支持，不用普通 Open 静默替代。

测试覆盖：空/二进制/Unicode/可执行文件、嵌套路径、确定性摘要与包、重启 Lookup、源修改后已发布快照不变、并发独立读、取消清理、精确容量边界、源修改故障闸门、路径穿越/重复/前缀/大小写冲突/保留名、根及祖先目录链接替换、父目录与末端链接替换、硬链接/FIFO、错误路径脱敏及 errno 保留、不合法 tar/PAX/manifest/摘要/截断/尾部数据。测试用显式同步控制竞态，不用 sleep 猜测。

运行 go test、go vet 与相关包 race；文件系统手工命令验证 Linux 安全打开与 tar 格式行为后再实现对应路径。不需要 QEMU，测试结果仅证明 Host 工作区模块，不当作 Guest Import 或端到端验收。
