# Manifest 最小模块设计

状态：设计评审已通过，最小模块已实现；验证及代码评审记录见 docs/manifest-implementation.md。范围为内置 Manifest 的加载、校验与纯配置产物生成，不修改 Guest 入口、Runner、proxy 协议或生命周期。

## 1. 范围和依赖

新增 internal/manifest 与 agents 的内置资源封装。现有 agents/codex、agents/claude 的文件均为空占位，本轮保留，新增一个 probe@1.0.0 固定测试程序清单。不会将其登记到现有真实 Guest，也不宣称真实 Codex/Claude CLI 兼容。

Manifest 不执行命令、不读 Host HOME/环境/凭证、不联网、不写文件；仅输出可校验的运行计划及配置字节。后续 Guest 安装层负责安全落盘、核对实际预装可执行文件及选择非 root 用户。

新增依赖仅用于真实格式处理：go.yaml.in/yaml/v3 固定 v3.0.5，解析 YAML；github.com/BurntSushi/toml 固定 v1.6.0，结构化编码 TOML 及测试反解。JSON 使用标准库。不自写 YAML/TOML 语法，也不引入通用模板引擎。共享 go.mod/go.sum 仅作这两个必要依赖的增量修改，保留其他 Agent 的修改，不自动全仓 tidy。参考 [YAML 项目](https://github.com/yaml/go-yaml)、[TOML 固定版本](https://github.com/BurntSushi/toml/tree/v1.6.0)。

## 2. 信任来源与 API

只接受 Go embed 编入程序的显式资源清单，不能由 RPC、任意 fs.FS、路径或 URL 添加用户 Manifest。测试通过包内解析入口构造坏样例，不为测试开放生产绕过入口。

- NewCatalog() (*Catalog, error)：读取明确登记的内置 YAML、模板和测试程序字节，校验 schema、包策略及模板，形成私有只读定义。
- Catalog.List() []Descriptor：返回独立描述快照，用于 Host 选择。Descriptor 包含 Ref、DisplayName、ManifestDigest 和声明的平台列表。
- Catalog.Load(ref, expectedDigest, platform) (*Definition, error)：按完整 id@version 白名单、预期摘要、OS/架构选择；不自动选最新版，不回退到其他平台。返回私有不可变 Definition，不允许调用方改字段绕过校验。
- Definition.Render(Values) (Plan, error)：Values 只含可信 Host 选择的 Guest Home、Workspace 与 ModelGatewayURL；校验这些实际输入后生成全新独立产物，不读取文件或环境。

Package 白名单由内置登记层提供 Ref、确切可执行路径及其内置字节，不相信 Manifest 自报包路径。probe 的 executable 固定为 /opt/agents/probe/bin/probe，packageRef 为 probe-builtin-v1。输出 ExecutableDigest 绑定实际嵌入的 probe 脚本字节；它不能证明 Guest 镜像安装内容正确，后续安装层必须自行校验。

ManifestDigest = SHA-256(规范 JSON 指纹记录)，记录包含原始 YAML 摘要、可执行文件摘要、按模板路径登记的模板摘要。模板变化、YAML 注释/空白变化和程序变化均改变该摘要。使用 sha256: 加 64 位小写十六进制。

## 3. YAML 字段与验证

保留 design.md 的最小字段：apiVersion、kind、metadata（id/version/displayName/support/provenance）、runtime（executable/args/cwd/terminal/user/environment）、files（destination/template/format）、capabilities.model.protocol。apiVersion 固定 safeagent.io/v1，kind 固定 AgentManifest，source 固定 builtin；user 固定 agent，terminal 固定 pty，cwd 固定 ${workspace}。

probe 声明 Linux/amd64、pty 和 test-http。真实 Agent 协议及其他平台必须通过独立版本/镜像验证后再扩内置清单，不以字符串声明代替验证。

YAML 最多 64 KiB、单文档、节点最多 2048、深度最多 16、单个字符串最多 4096 字节。先解析 Node 检查锚点/别名/merge/重复键/标签及节点预算，再用 KnownFields 的类型解码拒绝未知字段；这两阶段分别防展开/类型混淆和字段漂移。所有清单叶子必须是字符串，整数/布尔/null 等隐式类型拒绝。拒绝自定义标签，映射键必须为字符串。

id 为 ASCII 字母数字及 _-，version 为三段十进制版本，不接受 latest、范围或伪装路径。OS/架构/mode 列表非空、无重复且符合本轮支持集。完整 Ref 必须和内置登记匹配。Runtime 可执行路径必须等于包白名单，不接受 /bin/sh 或任意可执行文件；参数作为独立 argv 输出，不通过 sh -c 拼接。

args 最多 64 项，environment 最多 32 项，files 最多 16 项。环境从清单显式构建，不继承父进程。probe 包只允许 HOME、PATH、PROBE_CONFIG、SAFEAGENT_PROXY_KEY；HOME 必须为 ${home}，PATH 必须为 /usr/bin:/bin，占位认证必须为 proxy-managed，不接受加载钩子环境变量。允许项按包登记，不为 Agent 名称写 Runner 分支。

## 4. 变量与目标路径

只允许 ${home}、${workspace}、${modelGatewayURL}，清单与模板加载时校验语法，拒绝未知变量、裸 $HOME、未闭合变量。不求值 shell、命令、函数或动态模板。Render 使用已确定的替换器，替换值只插入一次，不再次解析值里的变量语法。

Values 三项各最多 2048 字节，超限在替换前拒绝。Home 必须为 /home/agent 或其规范子目录；Workspace 必须为 /workspace/project 或其规范子目录，均为 Linux 绝对路径，禁止 NUL/控制字符、非 UTF-8 和清理后变形路径。不把 Host sourceRef 的实际路径传入这里。每个展开后字符串最多 16 KiB，先按已有变量次数与输入长度计算所需字节，再分配替换结果；所有展开字符串及固定配置键的累计预算为 256 KiB，覆盖 argv、环境和配置树。

ModelGatewayURL 必须为 http://127.0.0.1:明确端口，可带规范路径前缀；不含用户信息、查询、片段或编码路径变形。它只是未来 Bridge 地址，不能据此访问 Host 上游；端口由 proxy 装配传入，本模块不占用端口或决定协议路径。

file.destination 必须以 ${home}/ 开头，后缀完全静态，不允许其他变量、..、空组件、反斜杠、绝对路径及重复/文件父目录冲突。配置只输出到隔离 HOME，不能输出到 workspace；模板引用也必须是内置 templates/ 下的规范路径，不能任意读文件。后缀及冲突只在加载时校验，Render 不重复执行同一检查。

Plan.Files 只提供相对 HOME 的 Path、Format、Mode=0600 和 Content；不提供 Host 目标路径。本轮不实现安装器，路径校验不等于文件系统防链接替换，未来落盘必须使用受限根句柄。

## 5. 结构化配置模板

模板来源统一使用严格 JSON 对象描述配置数据，format 决定最终输出 json 或 toml，不使用 text/template，也不接受原始 TOML 文本插值。现有空 .tmpl 文件不代表已支持的模板语法。

JSON 模板最多 64 KiB、深度最多 16、节点最多 2048。解析前拒绝非法 UTF-8 和孤立 UTF-16 surrogate 转义，合法 surrogate pair 允许；按解码后的键检查重复，拒绝未知尾部、null、浮点数和超出 int64 的整数。键不允许变量，字符串值才做已验证替换。本模块选择对象嵌套、同类标量数组、字符串、布尔、int64 的公共配置子集，不支持对象数组/嵌套数组；这是本轮范围，不宣称 TOML 规范禁止所有混合数组。键/字符串/整数等均由正式序列化器编码，不能用拼接规避转义。

每份渲染配置最多 256 KiB，总配置文件最多 1 MiB，完整 Plan 的摘要输入最多 2 MiB。展开前累计预算先限制配置树及 argv/环境，编码写入有界缓冲，最终 Plan JSON 在既有组件预算约束下编码并检查上限。JSON 编码器会先构造内部缓冲，TOML 编码器使用 bufio 并可能构造当前字符串的转义内容；不声称外层限长 Writer 能阻止所有内部缓冲，内存上界来自先限制源节点/字符串和展开总量。模板路径排序后绑定摘要，文件和环境排序输出，序列化相同输入得到相同字节。

## 6. 运行计划与摘要

Plan 包含版本、ManifestRef/ManifestDigest、PackageRef、Executable/ExecutableDigest、OS/Architecture、User/Terminal、Home/Directory、Args、排序 Environment、排序 Files 以及 PlanDigest。切片/配置字节每次 Render 新建，调用方修改返回产物不得污染定义或后续 Render。

PlanDigest = SHA-256(json.Marshal(Plan 将 PlanDigest 设为空字符串后的值))，绑定路径、权限、配置字节、argv、环境、平台及清单/程序摘要。该摘要代表生成时的产物；修改产物后原摘要不再有效。

它不自动等于现有 protocol.Preparation 的 ConfigDigest。当前 Guest 固定登记还把 ManifestDigest 绑定到测试脚本字节；本轮不改这条契约，未来接入必须另行设计实际安装与登记映射，不能把新摘要直接填入旧登记冒充已安装。

错误按 errors.Is 区分未知引用、摘要不匹配、不支持平台、清单无效、资源超限、渲染输入无效；真正编码错误在发生处返回，不为类型上不可能发生的内部状态加兜底，不做静默默认/降级。

## 7. 验证与评审

设计通过后先用命令验证 YAML 严格解码和 TOML/JSON 转义往返，再实现纯模块。测试包括内置加载/摘要选择、未知/重复字段与别名、大小/节点/深度边界、包路径/用户/环境/变量拒绝、配置目标穿越及冲突、Bridge URL 拒绝、双格式反解、引号/换行/反斜杠转义、模板/程序变更摘要、确定性及返回数据隔离。

运行相关测试、全仓 test/vet/race；并发 Render 仅用于确认定义只读及产物独立。不需要重复启动 QEMU，因为没有修改 Guest/Runner/协议；不将模块测试记为 Guest 准备或真实 CLI 验证。

代码完成后复用同一 Manifest 专用 codexx session 做代码及冗余/防御性代码审查，修复有效意见、原 session 复审。用户已停止 Claude；不执行 Git 写操作。
