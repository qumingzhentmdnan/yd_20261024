SafeAgent：跨平台 AI Agent 安全沙箱技术设计
黑客松 MVP · 可扩展 Agent Manifest · QEMU microVM · 无沙箱内真实凭据
技术设计方案
2026-10-24
文档信息与结论
字段	内容
文档版本	v1.0（技术方案，未开发验证）
目标场景	两天黑客松，安全优先、用户友好、主流 Agent 可扩展
首批 Agent	Claude Code、Codex；DSH 尽力适配
桌面目标	Windows / macOS / Linux，首先验证两个具体环境
运行底座	QEMU + Linux Guest；x86_64 microvm / ARM64 virt
可扩展方式	内置可信 Agent Manifest + 模板；不为每个 Agent 编写 Go Adapter
安全边界	真实凭据留宿主；工作区副本；无任意外网；非 root Agent
实施前提	QEMU 驱动层视为可复用模块，但启动、设备、跨平台仍需实测
核心决策。 一个沙箱对应一个独立 Linux VM。宿主机负责 VM 生命周期、用户界面、凭据保存、模型/MCP 代理与项目导入导出；Guest 内的通用 Runner 通过“Manifest + 配置模板”安装前验证、配置并运行不同 Agent。Agent 本身可视为不可信进程，不能通过修改 Manifest、环境变量或内置设置突破宿主隔离。
MVP 定义。 在一个目标平台实际完成“选择 Codex → 导入项目 → 运行 PTY → 模型请求经宿主代理认证 → GitHub 或测试 MCP 调用 → 验证越权访问失败 → 审核导出”，第二平台至少完成 VM 启动和 Agent 运行的冒烟测试。不要把“支持全部主流 MCP、所有桌面平台开箱即用”包装为两天已交付能力。
1. 目标、范围和边界
1.1 产品定位
SafeAgent 是本机 AI Agent 的统一安全运行器。用户不用理解 QEMU、Linux、Namespace、TOML 或 MCP JSON，只需：选择 Agent、选择项目、连接服务、点击运行、审核结果。底层把不可信的 Agent 限制在 VM 中，将访问外部模型及 MCP 的认证集中到宿主机侧。
1.2 功能需求
ID	功能	级别	MVP 验收点
F01	沙箱生命周期	P0	创建、启动、停止、销毁；错误可回收
F02	通用 Agent Runner	P0	无 agent-specific switch；按 Manifest 启动
F03	交互式终端	P0	PTY、ANSI、Ctrl+C、Resize、退出码
F04	项目副本导入导出	P0	无宿主机直接读写挂载；导出需用户确认
F05	模型 API 凭据隔离	P0	Guest 不包含真实 Key；合法请求可通过 Gateway
F06	Guest 网络强制控制	P0	非授权外网直连失败，不能只依赖环境变量
F07	远程 MCP Gateway	P0	1 个真实或可复现的 MCP 连通和凭据隔离
F08	Claude/Codex Manifest	P0	两个 Agent 通过同一个 Runner 运行
F09	Agent Manifest 扩展	P0	增加配置无需修改/重新编译 Runner
F10	DSH Manifest	P1	启动、模型调用和 MCP 中至少完成指定验证
F11	内置桌面向导	P1	选择 Agent / 项目 / 连接 / 启动
F12	审计与安全事件展示	P1	至少记录模型/MCP 路由结果及拒绝事件
F13	本地 stdio MCP 通用隔离	P2	后续设计，不承诺通用免密兼容
F14	完整跨平台安装器/签名	P2	不纳入两天正式承诺
1.3 非功能需求
•	安全：安全策略由 Host 执行，配置文件无法扩大沙箱权力；默认拒绝。
•	兼容：Agent 不需要知道 Windows/macOS；Guest 内统一 Linux 用户空间。
•	可扩展：新 Agent 由可信 Manifest、模板及已有 Runner 协议实现。
•	可维护：Manifest 与 Agent 二进制固定版本，失败可以回滚。
•	性能：MVP 推荐 2 vCPU、3 GiB 内存、差分虚拟磁盘；只作为试验起点，不是性能保证。
•	可解释：用户可以看到 Agent 及 MCP 是否连通、为何被阻止、哪些文件发生变化。
1.4 不承诺的安全目标
•	不保证防御所有 VMM/Guest 内核逃逸漏洞。
•	不保证避免模型通过合法获准的模型 API泄露已导入的代码或文本。
•	不保证任何第三方 stdio MCP 均能在不接触真实密钥的情况下原样运行。
•	不支持沙箱中的任何程序绕过 Host Gateway 后自动携带凭据。
•	不把 CLI 的 --approval never、danger-full-access 等标志作为安全策略；Agent 自带审批与 VM 隔离是不同层次。
2. 技术选型与平台矩阵
2.1 核心选型
组件	方案	选型原因
VMM	QEMU	统一跨 Windows/macOS/Linux 的虚拟机控制层
x86-64 机器类型	microvm（待目标平台实测）	无 PCI/ACPI 等部分旧设备，适合精简 Guest
ARM64 机器类型	virt	QEMU ARM64 不应直接照搬 x86 microvm 参数
硬件加速	Windows WHPX / macOS HVF / Linux KVM	与宿主架构一致时避免全软件指令模拟
Guest OS	精简 Linux（glibc 用户空间）	Agent 二进制、Node.js、Git 生态兼容
Host 控制服务	Go	系统进程、I/O、跨平台维护与 RPC
Guest Runner	Go	独立可执行文件，PTY 管理、模板渲染
UI	先本地 Web UI，后续 Wails	黑客松减少 GUI 打包与签名负担
终端	xterm.js + PTY	复用 Claude/Codex 现有终端 UI
配置模板	YAML Manifest + TOML/JSON 模板	不改 Go 代码即可新增 Agent
Vault	Windows Credential Manager / macOS Keychain / Linux Secret Service	真正的用户凭据留在宿主机
重要兼容说明。 -M microvm 是 QEMU x86 machine type；Apple Silicon ARM64 需要 -M virt 等匹配的设备与内核。Windows 的 WHPX 能用于 QEMU，但不能假设某个具体版本的 WHPX 一定支持 microvm 所需全部设备选项；应建立 OS × CPU × QEMU 版本的启动兼容测试矩阵，失败不得静默回退到不受控的非隔离进程。
2.2 第一阶段验证矩阵
宿主	CPU	QEMU 加速	Guest 机器类型	当前交付承诺
Windows 11	x86_64	WHPX	microvm（需实测）	主演示平台候选
macOS Intel	x86_64	HVF	microvm（需实测）	次平台冒烟候选
macOS Apple Silicon	ARM64	HVF	virt	次平台冒烟候选
Linux	x86_64	KVM	microvm	技术开发/CI 候选
Linux	ARM64	KVM	virt	后续
镜像至少区分 linux-amd64 与 linux-arm64。内核、rootfs、Guest Runner 和 Agent 二进制必须架构匹配。QEMU 的硬件加速与微型机器类型支持分属不同能力，不应将 Linux/KVM 的性能数据照搬到 WHPX/HVF。
3. 系统总体架构
┌────────────────────────────────────────────────────┐
│ Windows / macOS / Linux Host                         │
│                                                      │
│  Local UI ─── Host Manager (Go)                      │
│                 ├─ VM Backend / QMP                  │
│                 ├─ Workspace Stager                  │
│                 ├─ Host Credential Vault             │
│                 ├─ Model HTTP Gateway                │
│                 ├─ MCP Gateway                       │
│                 ├─ Policy & Audit                    │
│                 └─ Terminal Gateway                  │
│                       │ control / I/O                │
└───────────────────────┼──────────────────────────────┘
                        │ 受控 VM 通道 / 虚拟设备
┌───────────────────────┼──────────────────────────────┐
│ QEMU Linux Guest      ▼                              │
│  safeagent-guestd (Go)                                │
│   ├─ Manifest Resolver / Config Renderer              │
│   ├─ Process Supervisor + PTY                         │
│   ├─ Guest-local HTTP bridge → Host Model/MCP Gateway │
│   └─ Workspace /home/agent /workspace/project         │
│             │                                         │
│             ├── Claude Code                           │
│             ├── Codex                                 │
│             └── DSH（不同沙箱选其一）                 │
│                                                       │
│  无真实凭据；非 root；工作区副本；无任意外网         │
└───────────────────────────────────────────────────────┘
设计约束：Host Manager、Vault、Gateway 处于可信侧；Guest 内的一切应用数据和 Agent 执行代码均视为不可信。Host 不能相信 Guest 送回的 sandbox_id、用户路径或文件名；VM 身份由 Host 建立的连接绑定，不由 Guest 自报。
3.1 分层责任
层	核心职责	禁止事项
UI	展示服务、配置、授权、终端	不直接读取/写入真实 Token 配置文件
Host Manager	验证 SandboxSpec、启动 QEMU、工作区和策略	不执行来自 Guest 的任意宿主命令
Credential Vault	Key/Token 安全存储和撤销	不将 Token 返回给 Guest
Model Gateway	模型代理路由、认证注入、服务白名单	禁止任意目标 URL、Header 注入和开放转发
MCP Gateway	MCP 认证、协议转发、有限审计	不向 Guest 传真实 OAuth Token
Guest Runner	Manifest 解析、渲染、PTY、进程生命周期	不能更改 Host 网络策略或主机挂载
Agent CLI	在 /workspace 内工作、使用代理	不持有真实 Key、不直接写宿主工作区
4. 主要对象与生命周期
4.1 状态机
NEW → PREPARING → BOOTING → GUEST_READY → AGENT_RUNNING
                                       │          │
                                       │          ├── AGENT_EXITED
                                       │          └── FAILED
                                       └── FAILED
AGENT_EXITED → STOPPING → STOPPED → DESTROYED
任意异常状态 → CLEANUP_PENDING → STOPPED / FAILED_CLEANUP
•	所有创建/删除动作幂等：重复请求不应产生额外 QEMU 进程或孤儿磁盘。
•	Host 每个沙箱保存 sandbox_id / vm_pid / qmp_endpoint / disk_overlay / state / image_digest / manifest_digest。
•	Agent 崩溃不等于 QEMU VM 崩溃；进程退出时返回明确的 exit code。
•	QEMU 异常退出后关闭该 VM 的代理能力并清理临时 Host 资源。
•	运行中的 Guest 与 Host 断联时，按预定超时让外部代理访问失败关闭；不要继续默许无身份认证的访问。
4.2 用户运行流程
1.	用户选择 Agent、项目与已连接的服务。
2.	Host 将 UI 参数规范化为 SandboxSpec，验证 Manifest、镜像架构、服务白名单。
3.	Workspace Stager 在 Host 从用户授权目录构建隔离副本，筛查敏感文件并固定导入基线。
4.	Host 创建写时复制磁盘、启动 QEMU，将专属控制端点绑定到 VMRef + boot epoch；此时尚不授予 Gateway 访问能力。
5.	Hello 协商完成后，将工作副本传入 Guest 并核对完成记录，再建立 Gateway 授权；Guest Runner 执行 PrepareAgent，检查二进制与 Manifest、渲染配置，成功后进入 GUEST_READY。
6.	Host 调用 StartSession，引用 preparedID 与工作区摘要；Guest 创建 PTY 并启动 Agent。
7.	Web UI 通过认证的 WebSocket 连接终端流。
8.	Agent 的模型/MCP 请求被 Guest-local Bridge 送到受限 Host Gateway。
9.	结束后由 Host 从 VM 中提取变更，显示 Diff、冲突与审核结果，再应用到宿主项目。
10.	先撤销会话级代理权限，再停止 VM，按用户选择保留或删除差分盘。
5. SandboxSpec：唯一业务层配置
原则：SandboxSpec 描述用户选择；Manifest 描述某个 Agent 如何运行；Host Policy 描述强制安全能力。三者不可混合。
apiVersion: safeagent/v1
kind: Sandbox
metadata:
  id: sbx-demo-001
spec:
  runtime:
    backend: qemu
    architecture: amd64
    vcpus: 2
    memoryMiB: 3072
    imageRef: linux-agent-base@sha256:EXAMPLE_DIGEST
  agent:
    manifestRef: codex@1.0.0
    modelRef: openai-responses
    commandMode: pty
  workspace:
    sourceRef: chosen-by-user-001   # Host 持有实际路径映射
    importMode: copy
    exportMode: reviewed
  integrations:
    mcpRefs: [github]
  security:
    profileRef: strict-desktop-v1
    allowHostMounts: false
    externalNetwork: gateway-only
    forwardSshAgent: false
禁止：任意 hostPath、任意 -device、任意 QEMU 参数、任意外网域名、任意 Vault key 作为用户可编辑的 Spec 属性。imageRef 和 manifestRef 必须按签名/摘要和版本白名单解析。例子中的 digest 是格式示意，不是真实镜像指纹。
6. 配置驱动 Agent Manifest
6.1 Manifest 模型
一个 Agent 是一个声明式安装物：可执行程序 + 允许的启动参数 + 环境变量映射 + 用户配置模板 + MCP 映射 + 能力声明。Runner 不针对 Agent 名称编写 switch。
apiVersion: safeagent.io/v1
kind: AgentManifest
metadata:
  id: codex
  version: 1.0.0
  displayName: Codex CLI
  support:
    os: linux
    architectures: [amd64, arm64]
    modes: [pty]
  provenance:
    source: builtin
    packageRef: codex-cli-pinned   # 映射到可信预装包
runtime:
  executable: /opt/agents/codex/bin/codex
  args: []
  cwd: '${workspace}'
  terminal: pty
  user: agent                   # 只能引用预设非 root 用户
  environment:
    CODEX_HOME: '${home}/.codex'
    SAFEAGENT_PROXY_KEY: 'proxy-managed'
files:
  - destination: '${home}/.codex/config.toml'
    template: templates/codex-config.toml.tmpl
    format: toml
capabilities:
  mcp:
    mode: streamable-http
    renderer: toml-map
  model:
    protocol: openai-responses
  resume: optional
6.2 Runner 通用执行管线
Load → Validate → Resolve Variables → Render Typed Config → Prepare Isolated HOME → Spawn PTY → Monitor → Collect Exit。
动作	Runner 规则
Load	只加载已签名或内置 Manifest；绑定版本和 SHA-256
Validate	限制 schema、文件路径、可执行文件、变量、模板目标
Resolve	只允许固定变量，如 ${workspace}、${home}、${modelGatewayURL}、${mcpGatewayURL}
Render	JSON/TOML 使用结构化序列化或严格上下文转义，避免文本注入
Environment	由空白环境变量集合构建，不继承 Host/Guest Manager 的隐私变量
Spawn	execve(argv)，不通过 sh -c 拼接命令，使用非 root 用户和 PTY
Monitor	捕获退出码、限流、超时、日志清理
Security	Manifest 无权修改 VM 网络、真实凭据、Host 文件映射和 QMP
Manifest 虽然是 YAML，但描述了启动可执行程序，因此本身属于需审查的执行包。黑客松只支持内置 Manifest；用户自定义 Manifest 是后续功能，必须增加来源审查和权限边界。
6.3 Agent 示例：Codex
templates/codex-config.toml.tmpl：
model_provider = "safeagent"

[model_providers.safeagent]
name = "SafeAgent Model Gateway"
base_url = "${modelGatewayURL}/openai/v1"
wire_api = "responses"
env_key = "SAFEAGENT_PROXY_KEY"

# [mcp_servers.github] 等由可信渲染器动态构造
Runner 设置 SAFEAGENT_PROXY_KEY=proxy-managed，真实 Key 由 Host Gateway 注入；不复制用户现有的 ~/.codex/auth.json 或整个配置目录。config.toml 放在隔离的用户级 CODEX_HOME，避免受项目不可信配置覆盖。模型名从兼容性测试通过的模型清单选择，不直接承诺某个未验证模型。
6.4 Agent 示例：Claude Code
claude.yaml 的差异主要是启动参数、模型环境变量和 MCP JSON 模板：
runtime:
  executable: /opt/agents/claude/bin/claude
  args:
    - --mcp-config
    - '${generatedMcpConfig}'
    - --strict-mcp-config
  cwd: '${workspace}'
  terminal: pty
  user: agent
  environment:
    ANTHROPIC_BASE_URL: '${modelGatewayURL}/anthropic'
    ANTHROPIC_API_KEY: proxy-managed
Claude Code 自带的工具审批/设置可以继续用于改善体验，但主安全边界仍然是 VM/Host Policy。运行前应检查当前版本的 --mcp-config 等命令行选项，Manifest 与验证过的 Agent 版本绑定。
6.5 Agent 示例：DSH
DSH 支持 Profile 入口及 @deepseek-ai/dsh-mcp-client。Manifest 只描述：
runtime:
  executable: /opt/agents/dsh/bin/dsh
  args: [--profile, safeagent]
  terminal: pty
  cwd: '${workspace}'
files:
  - destination: '${home}/.dsh/profiles/safeagent/config.yml'
    template: templates/dsh-profile.yml.tmpl
    format: yaml
safeagent Profile 的具体 Cordis 插件配置由 DSH 版本适配测试生成，上述路径是产品内的目标布局示例，不作为 DSH 官方固定路径或可直接运行的已验证配置。如果版本要求启动时选择不同 Profile 或 ACP 通道，仍由 Manifest 选择通用 Runner 的 pty / acp 执行模式，不硬编码 DSH ID。
6.6 能力边界与未来插件
配置能表达 CLI 级差异；若某 Agent 只支持专有 RPC/ACP、复杂会话恢复或网页登录，就需要通用协议类型适配器，而不是编写某个 Agent 名称的特殊分支。建议将协议扩展点限定为 PTY、ACP、stdio JSON-RPC，并在 Manifest 中声明。MVP 仅 PTY。
7. Agent 进程与 Guest 安全配置
7.1 Guest 目录布局
/opt/safeagent/guestd                # root 持有，Agent 不可修改
/opt/agents/{claude,codex,dsh}/      # 固定版本，Agent 不可修改
/etc/safeagent/manifests/            # 内置配置，只读
/run/safeagent/control               # 受限控制通道
/home/agent/                         # 每 VM 独立 HOME
/workspace/project/                  # 项目隔离副本
/tmp/                                 # 临时文件，有大小限制
运行 Agent 的账户不可拥有 Guest Runner 的服务账户、控制 socket 或 Host Gateway 配置权限；不向 Agent 提供 sudo、Docker Socket、宿主 SSH Agent 等特权入口。Guest 内服务本身应最小权限运行，但注意独立 VM 的文件系统只防止跨宿主访问，不代表 VM 内 root 永远可信。
7.2 包管理与升级
MVP 使用预装并固定版本的 Agent；Guest 运行时默认没有安装任意软件需要的外网出口。后续版本可通过受信镜像更新、哈希验证的 Package Bundle 安装。禁止由 Manifest 的 install.command 在 Host 上任意执行 shell；自定义包安装只在受限构建沙箱中进行。
8. Host↔Guest 通信与终端
8.1 通信建议
首选通过 virtio-serial 承载 Host↔Guest 有界 RPC；控制、终端、文件和代理必须使用独立的解析与调度边界。物理拓扑尚未冻结：一个设备内部多路复用或独立多端口，需通过目标平台 spike 选择并记录实际 QEMU 参数、设备名和关闭语义，不能由各模块分别选择。最小协议范围与冻结门槛见 design/interfaces-and-fakes.md 第 7 节。QEMU 自带 qemu-ga 不等于业务专用 Agent Runner：它可用于基础运维，但不会自动提供你们需要的 PTY、审计、Manifest 管理和隔离策略。
建议业务 RPC：
Hello(version, imageDigest, guestNonce)
GetState()
PrepareAgent(manifestRef, sandboxScopedConfig, workspaceRef, workspaceDigest) -> preparedID
StartSession(operationID, sessionID, preparedID, workspaceRef, workspaceDigest) -> SessionInfo
AttachTerminal(sessionID) -> terminal stream descriptor
ResizeTerminal(sessionID, cols, rows)
StopSession(sessionID, stopOptions)
SessionExited(sessionID, exitResult)  # Guest 事件；WaitSession 在客户端等待/查询结果
ListWorkspaceChanges() -> changeSetID
ExportChanges(changeSetID) -> chunk stream
帧结构原则：固定协议版本、type、request_id、stream_id、payload_length、payload、校验；最大帧长和并发数严格限制；流必须支持 backpressure。以上尚不是完整线协议。终端输入输出在 AttachTerminal 后只走 stream，不再提供逐次 WriteTTY/WriteTerminal RPC；文件和代理业务消息后续单独冻结。Guest 不得发送 RunHostCommand、任意读写 Host 文件、SetCredential 等 RPC。
8.2 终端链路
xterm.js ↔ WebSocket(本地鉴权) ↔ Host Manager
       ↔ virtio-serial terminal stream ↔ Guest Runner ↔ PTY ↔ Agent
必须实现：UTF-8/ANSI 透明传输、Raw 模式、Ctrl+C、Ctrl+D、窗口 Resize、退格、断线重连策略、流速限制、退出状态。网页终端显示应当进行输出转义，不将 ANSI 或 Agent 文本解释为前端管理 API。
8.3 本地 UI 的攻击面
Host 服务默认仅监听 127.0.0.1，要求随机会话凭据、Origin/CSRF 检查、WebSocket 鉴权、请求体大小限制。localhost 并不天然安全；其他本地进程或恶意网站可能尝试访问本地服务。
9. 模型 API 的无密钥运行
9.1 工作原理
用户在宿主机一次性保存 Key；沙箱内只持有无价值占位符或受限会话能力。
Codex (Guest) → http://127.0.0.1:<guest-bridge>/openai/v1
  → Host Gateway [识别物理 VM 通道身份、检查 allowlist]
  → 从宿主 Vault 取 OpenAI API Key
  → 新建上游 HTTPS 请求 + 认证 Header
  → 官方模型 API → 按原流返回
MVP 不需要对任意 HTTPS 实施透明中间人解密。应使用 Agent 提供的自定义 base_url，将协议兼容的模型请求显式指向 Guest-local Bridge，再由可信 Host Gateway 发起 HTTPS。模型 Provider 间请求结构不同，因此网关首先只实现 OpenAI Responses 和/或 Anthropic Messages 两种明确测试通过的协议类型。
9.2 Gateway 授权模型
credentialBinding:
  sandboxRef: sbx-demo-001
  service: openai
  credentialRef: vault://openai-main
  allowedHosts: [api.openai.com]
  allowedPaths: [/v1/responses]
  maxConcurrent: 2
  requestBytesLimit: 2097152
  expiresWithSandbox: true
Host 以通道级身份确定 sandboxRef，不允许 Guest 在 URL 参数中伪造该字段。代理必须：固定上游 Host 与路径；禁止 SSRF/重定向到不允许的目标；限制 Host/Header 覆盖；移除 Guest 发来的上游 Authorization；不记录 API Key；设置速率、大小、连接数和请求超时；支持流式 SSE 响应与取消。
9.3 Guest 网络隔离必须独立实施
不能只设置 HTTP_PROXY 或 ANTHROPIC_BASE_URL 就声称隔离生效。 Agent 可以自己执行 curl 或绕过环境变量。
首选安全运行模式：Guest 在 Agent 执行阶段没有通向外网的通用网卡或默认路由；只保留 VM↔Host 的受控设备通道，并由 Guest-local Bridge 提供 127.0.0.1 模型/MCP 端口。开发阶段若使用用户态网络/TAP，必须在 QEMU 网络与 Host 防火墙同时配置目的地限制，并通过 IPv4、IPv6、DNS、UDP、直连 IP 负向测试后才能称为安全模式。
对需要下载依赖/联网搜索的 Agent：MVP 暂不支持任意外网；未来增加单独的无凭据 Web Proxy，按域名和资源类别授权，不混用模型认证代理。
9.4 OAuth/账号登录限制
Claude/ChatGPT 官方账号登录可能涉及浏览器、设备码、Cookie 或专有认证流程，不能等同于 API Key。MVP 以用户在宿主配置的 API Key 模式为主；不承诺透明代理所有订阅账号登录。
10. MCP Gateway 设计
10.1 边界
•	远程 Streamable HTTP MCP：由 Host Gateway 处理目标地址、OAuth/Token、会话及流式消息。
•	Guest 内 Agent：只获得 http://127.0.0.1:<bridge>/mcp/<service> 的代理地址，不持有真实认证信息。
•	Host 中 Vault：保存 OAuth Refresh Token、API Key、所需 Client 资料；Guest 不可调用 getRawSecret。
•	本地 stdio MCP：若需要特殊主机权限和环境 Key，单独隔离并进行兼容性验证。不将“零沙箱内 Key”错误宣传为“任何 MCP 进程都拿不到 Key”。
10.2 MCP 协议兼容
MCP 的标准 Transport 包括 stdio 与 Streamable HTTP；实际协议版本随 SDK 迭代，MVP 明确选用一个测试基线，不在两天内从零重写全协议栈。优先复用成熟 SDK，正确处理 initialize / tools/list / tools/call、JSON-RPC 错误、HTTP Session、SSE、取消、Header 与 OAuth 回调。新版 MCP 协议的无握手模式与旧版握手模式有差异，必须做版本协商/选择与回归测试。
10.3 统一逻辑服务记录
apiVersion: safeagent.io/v1
kind: MCPConnection
metadata:
  id: github
spec:
  transport: streamable-http
  upstreamURL: https://EXAMPLE-REAL-SERVICE/mcp
  credentialRef: vault://github-oauth
  allowedAgentRefs: [claude, codex, dsh]
  allowedTools: [list_issues, get_issue]
以上为内部连接记录示例，URL 必须使用实际配置的服务端点。allowedTools 在 Gateway 具备可靠检查前只是声明；不应声称工具级阻拦已经生效。Manifest 只决定如何把这条 Host MCP 连接输出成 Claude JSON、Codex TOML、DSH Profile。不能把 credentialRef 解析为真实 Token 写入 Guest。
10.4 安全策略
Host Gateway 必须按服务白名单路由，验证 URL Scheme、域名、目标 IP（含 DNS 解析变化）、端口、重定向、Session 所属沙箱；不能做无约束的通用 HTTP 转发。MCP 调用返回内容应当视为不可信输入，避免输出日志或 UI 注入。MVP 可采取“仅开放测试服务与有限只读工具”的安全策略，不承诺完整语义风险识别。
11. 工作区隔离与变更导出
11.1 复制优先
从宿主选择的目录导入到 VM /workspace/project；运行阶段没有双向文件系统挂载。默认使用 Git 跟踪文件作为副本基准，额外未跟踪文件由用户勾选。对 .env、私钥、云认证缓存、.git/config 等执行敏感信息告警，默认不自动导入，用户可显式调整。
边界：Agent 能读取所有真正复制进 VM 的内容，且可能发送到获准模型 API；敏感文件筛查只是降低误导入风险，不保证识别所有秘密。
11.2 安全复制
•	Host 将用户选择规范化到授权目录，处理符号链接、硬链接和越界目标；拒绝设备文件、FIFO、Unix Socket 等特殊文件。
•	Guest 解包时严格禁止 ../、绝对路径、重定向到目录外的符号链接；对文件数、总大小、单文件大小及压缩比设限。
•	将 Host 源目录映射为不透明 sourceRef，Guest 不知道本机绝对路径；传输逐块校验大小、摘要。
•	不把宿主目录当作 QEMU 的可写共享目录。
11.3 导出
Guest 返回文件变更列表、哈希以及变更数据；Host 基于导入时基线比对当前宿主文件版本。用户预览 Diff，手动确认导出，遇到宿主修改冲突必须停止覆盖、提示三方比对或另存。优先使用 Git Patch 或受控文件级拷贝；严禁 Guest 直接给出待写入的绝对宿主路径。
12. 关键接口定义
12.1 Host 本地 HTTP API（草案）
方法	路径	描述
GET	/api/v1/agents	列举内置 Agent Manifest 与兼容性
GET	/api/v1/sandboxes	查询沙箱列表及状态
POST	/api/v1/sandboxes	验证 Spec，准备工作副本和 VM
POST	/api/v1/sandboxes/{id}/start	启动 VM / Guest Agent
POST	/api/v1/sandboxes/{id}/sessions	根据 Manifest 启动 Agent
POST	/api/v1/sandboxes/{id}/stop	优雅停止 Agent/VM
DELETE	/api/v1/sandboxes/{id}	清理 VM/差分盘与授权
GET	/api/v1/sandboxes/{id}/changes	查看导出预览
POST	/api/v1/sandboxes/{id}/export	应用已批准的变更集
GET	/api/v1/mcp/connections	已连接的 MCP 服务
POST	/api/v1/mcp/connections	进行 Host 侧连接/授权
WS	/api/v1/terminals/{sessionId}	认证的 PTY 双向流
Host API 使用固定 schema，不允许用户在任何请求中传任意可执行命令路径、任意 QEMU flags、Vault 原始查询操作。错误响应包含 code / message / request_id / recoverable，审计日志使用相同 request_id。
12.2 Guest RPC（草案）
RPC	输入	输出
Hello	协议版本、Guest 构建信息	协商结果；身份由 Host 通道关联确定
PrepareAgent	operation_id、Manifest 与限域配置、workspace_ref/digest	prepared_id、manifest/config/workspace 摘要
StartSession	operation_id、session_id、prepared_id、workspace_ref/digest	SessionInfo
AttachTerminal	session_id	终端流描述；后续字节走 stream
ResizeTerminal	session_id、cols、rows	ACK
StopSession	session_id、stop_options	停止结果
GetState	空	运行进程与状态
SessionExited（事件）	session_id、exit_result	无响应；结果可查询恢复
ListChanges	project_ref	变更元数据
ReadChangeChunk	change_set、offset、len	chunk + SHA-256
本表为业务消息草案，不是完整 envelope 定义。v0.1 只冻结控制与终端；文件与 Gateway 消息另行冻结。Prepare/Start 等内部 Go 方法通过客户端映射到上述规范名称，不得另起一套线协议。
13. 持久化与凭据模型
Host 可先用本地 SQLite 保存 sandbox / session / image / manifest / mcp_connection / credential_ref / audit_event 元数据；真正的密钥留系统凭据管理器，不写入 SQLite 或 YAML。退出后撤销沙箱级代理能力。
数据	存储位置	生命周期
QEMU 基础镜像	Host 受控数据目录	多沙箱共享、不可变
沙箱 Overlay	Host 沙箱私有目录	创建到销毁；可选择保留
Agent 配置	Guest 私有 HOME	VM 生命周期内
模型/MCP 真正的 Key	Host OS Vault	用户主动撤销前
Host 授权 Binding	SQLite + 内存会话	沙箱启动到停止
终端输出	内存限流；可选本地脱敏日志	依据用户设置
工作副本	Guest overlay	至少到导出审核完成
敏感 Token 不得出现在日志、错误、终端回显、Manifest、模板、Guest 环境或 crash dump 中。Guest 的占位字符串或短期能力令牌虽然不是原始 Key，仍属于可调用能力，需要绑定沙箱、范围、时效和速率。
14. 安全威胁模型与防护策略
14.1 攻击主体
1.	恶意 Prompt Injection 导致 Agent 执行不预期命令。
2.	恶意或受损的项目依赖、构建脚本、Agent 插件。
3.	有漏洞或恶意的第三方 MCP。
4.	恶意 Guest 试图伪造 Host RPC、注入身份或访问越界文件。
5.	本地恶意程序/网站试图调用 Host UI/Gateway API。
14.2 安全控制表
风险	防护	验证方式
读取宿主机秘密	VM 不挂载 Host Home；授权导入副本	Guest 读专门准备的宿主假秘密失败
VM 任意联网	运行阶段没有直接外网出口，仅受控模型/MCP 桥	IPv4/IPv6/DNS/UDP/直连 IP 失败
获取真实 API Key	Vault 在 Host；Gateway 认证注入	Guest env、fs、进程、日志扫描无真实 Key
Agent 伪造模型网关目标	固定服务/Host/Path/证书校验与重定向限制	各种代理 SSRF 变体被拒绝
Guest 操作 Host	RPC 方法白名单 + 通道身份绑定	注入任意 Host 路径/命令无效
Manifest 执行任意脚本	内置白名单、argv 模式、可执行路径验证	sh -c/绝对 Host 路径无法配置
工作区回写破坏	Diff 审查、哈希比对、冲突检测	宿主文件并发修改时不覆盖
MCP 代理滥用	受限 endpoint、服务/工具 allowlist、日志	非白名单 MCP 工具不能执行
本地 UI 跨站请求	绑定 loopback、会话 token、Origin/CSRF	外部来源访问失败
14.3 四个最容易误报的“安全”
•	Agent 拿不到真实 Key ≠ Agent 不能滥用已授权 API。
•	MCP 通过 Gateway ≠ 所有 MCP 本地进程都有相同的隔离边界。
•	Linux VM 与 Host 隔离 ≠ Guest 里所有已导入的数据不能通过模型发出。
•	Agent 终端中看不到审批弹窗 ≠ Host 已经强制实施了底层网络/文件隔离。
出错策略：任何校验失败（Manifest、镜像、Host Gateway、网络策略、Agent 启动），不退回普通宿主进程执行，不开放任意网络作为修复方式。
15. 两天黑客松研发计划
15.1 明确交付策略
时间内完成“一个主平台端到端闭环 + 第二平台启动验证”是合理目标；正式三平台全功能、完整安装器、任意 MCP、本地 stdio 凭据隔离不列入硬性交付。可由 2–4 名工程师并行；单人应砍掉 DSH、跨平台验证和复杂 UI。
时间	工作内容	可验收产物
Day 1 0–2h	目标机器 QEMU/Guest 通道与最小协议 spike；确定镜像	拓扑实测记录；以 interfaces-and-fakes.md 第 7 节为验收目标，未通过则不得宣布协议冻结
Day 1 2–5h	Manifest Schema、Runner PTY、Codex 启动	不改 Go 代码启动 Codex
Day 1 5–8h	工作副本导入、Host Model Proxy、单服务绑定	模型请求成功，无真实 Key 入 Guest
Day 2 0–3h	MCP Gateway + Claude Manifest	同一 Runner 启动 Claude 并调用 MCP
Day 2 3–5h	网络/文件/凭据负向测试	可复现拒绝事件，安全边界真实有效
Day 2 5–7h	简单 UI、Diff 预览、第二平台冒烟	一键启动、导出审核、第二平台启动
Day 2 7–8h	演示预案、日志、修复	3 分钟演示和可复现脚本
研发砍项顺序：先砍动态安装、DSH 的 ACP、高阶审计、跨平台完整打包、双平台 MCP；不得砍“真实 Key 不进 Guest / 宿主无直接挂载 / 网络默认拒绝”的 P0 安全校验。若无法实现可信网络隔离，就应在演示说明“模型/MCP 代理已通，但严格沙箱安全尚未达标”。
15.2 分工（3 人示例）
人员	主责	联调点
A	QEMU / Host Manager / 工作副本 / 控制通道	Guest Hello、VM Lifecycle
B	Guest Runner / Manifest / PTY / 两个 Agent	StartSession + 模型代理 URL
C	Vault / Model Proxy / MCP Gateway / Web UI	Credential Binding + MCP Discovery
每 2–3 小时集成一次，所有模块使用同一 sandbox_id、request_id、协议版本和事件格式。
16. 测试计划与验收门槛
16.1 功能测试
编号	测试场景	通过标准
T01	同一 Runner 启动 Codex、Claude	两个独立 Manifest；无需 Go 特殊分支
T02	新增演示 Agent Manifest	加 YAML/模板后直接启动测试 CLI
T03	PTY 控制	可输入、Ctrl+C、Resize、终端颜色正确
T04	断线与进程退出	无孤儿 VM；退出码可见；终端不混流
T05	无 Key 模型代理	Host Key 生效；Guest 无原始 Key
T06	MCP 远程工具	tools/list、一次只读 tools/call 成功
T07	文件副本工作流	Guest 修改不立即改变 Host 文件
T08	审核导出	明示 Diff；冲突不会自动覆盖
T09	第二平台冒烟	目标 CPU 架构镜像、Guest 启动、Agent 命令运行
16.2 负向安全测试
编号	场景	通过标准
S01	Guest 访问 Host 预制假密钥文件	不可访问
S02	Guest 直连任意外网	阻止；覆盖 IPv4/IPv6/DNS/UDP
S03	Guest 请求未授权模型域名/路径	Gateway 拒绝
S04	Guest 尝试读取真实 Token	进程/环境/配置/Guest 磁盘找不到
S05	Guest 伪造其他 sandbox_id	Host 以物理通道身份拒绝
S06	恶意归档文件穿越路径	Host/Guest 解包校验拒绝
S07	未批准的工作区更改	Host 原始文件保持不变
S08	Host Gateway 不可用	访问失败关闭，不改为外网直连
S09	未受信任 Manifest	无法启动或扩大特权
S10	恶意网页请求 localhost API	Origin/会话验证拒绝
所有测试都使用专门准备的假凭据和独立测试项目，不使用真实私钥或生产服务资源。
17. 可观测性与故障处理
统一事件字段：timestamp / request_id / sandbox_id / session_id / component / event_type / outcome / reason_code / duration_ms。禁止日志记录原始 Key、完整模型 Prompt、敏感 HTTP Header；如需保存审计详情，由用户显式授权并进行脱敏。
异常	用户可见提示	系统动作
缺失硬件虚拟化	“无法使用安全虚拟机，请启用对应系统功能”	不启动不安全降级
Guest Runner 未就绪	“沙箱启动失败” + 诊断 ID	停止 VM、回收句柄
未绑定模型凭据	“请先连接模型服务”	停止 Agent 请求，不生成假成功
MCP OAuth 失效	“需要重新连接 GitHub”	暂停/拒绝 MCP 请求
工作区存在敏感内容	“检测到可能的私密文件”	导入前由用户选择
Diff 冲突	“原文件已变化，无法直接覆盖”	保留 VM 变更，等待处理
安全代理失联	“外部访问已暂停”	默认拒绝所有认证代理请求
18. 关键风险与待验证决策
风险	影响	缓解/决策
QEMU microvm 在 WHPX/HVF 目标组合的设备兼容	可能启动失败	第一小时先实测；必要时使用同架构简化标准机器类型，不降低 VM 边界
ARM64 使用 x86 microvm 参数	启动失败	ARM64 单独使用 virt 机器类型和镜像
Guest HTTP Bridge 通道与网络机制不完善	无法代理模型/MCP	提前做“单请求双向透传”技术 Spike；封闭外网优先
Claude/Codex API Key 模式差异	Agent 无法认证	固定测试版本和 Provider 模式，不承诺官方网页登录
MCP 版本和 SSE/会话处理复杂	工具调用失败	复用支持目标版本的 SDK，先做一个真实服务
本地 MCP 需要真实 env Key	无法通用无凭据	P2；远程 MCP 先行，明确进程权限边界
两天开发覆盖过宽	结果不完整	保证单平台闭环和安全测试，其他功能作为演示加分
Host Proxy 本身被滥用	SSRF、跨沙箱凭据使用	Channel 身份绑定、Upstream Allowlist、限制重定向、请求配额
重大决策 A：MVP 不要求一个沙箱内运行多个 Agent。每个 VM 的配置只选择一个 Agent；多 Agent 同 VM 需要另做 Guest 进程级隔离和资源管理。
重大决策 B：Host Vault 与 Gateway 是可信基础设施；Guest 内 Agent 无法直接获取 Key，但能使用授权的服务调用能力。对这种能力需要审计、过期和权限范围控制。
重大决策 C：Manifest 不是任意脚本引擎；它只声明一组安全的、结构化的运行操作。默认仅允许产品内置、经过版本验证的 Manifest。
19. 仓库结构建议
safeagent/
├── cmd/
│   ├── safeagent-host/           # 宿主 Manager HTTP 服务
│   └── safeagent-guestd/         # VM 内通用 Runner
├── internal/
│   ├── host/
│   │   ├── vm/                   # QEMUBackend / QMP / overlays
│   │   ├── workspace/            # 导入、快照与导出
│   │   ├── credential/           # OS Vault 与 Binding
│   │   ├── modelproxy/           # 认证 HTTP/SSE 代理
│   │   ├── mcp/                  # MCP HTTP/OAuth Gateway
│   │   └── api/                  # localhost HTTP + WebSocket
│   ├── guest/
│   │   ├── manifest/             # YAML schema / validation
│   │   ├── render/               # 结构化模板渲染
│   │   ├── runner/               # exec + PTY + lifecycle
│   │   └── bridge/               # Guest local Gateway bridge
│   └── protocol/                 # RPC/事件 schema
├── agents/
│   ├── codex/                    # agent.yaml + templates
│   ├── claude/
│   └── dsh/
├── images/
│   ├── amd64/                    # guest kernel/rootfs references
│   └── arm64/
├── frontend/                    # React + xterm.js
├── tests/
│   ├── functional/
│   ├── isolation/
│   └── compatibility/
└── docs/
    ├── threat-model.md
    ├── manifest-schema.md
    └── acceptance.md
20. 最终 MVP 验收定义（Definition of Done）
必须同时满足以下八项，才能称为安全沙箱 MVP：
1.	在主平台启动隔离 Linux Guest，并在其中运行真实 Agent，而非在 Host 上执行。
2.	Claude/Codex 由不同 Manifest 启动；Runner 本身没有针对名称的适配分支。
3.	Agent 正常交互，PTY 支持输入、Ctrl+C、窗口变化和退出状态。
4.	宿主原项目不会因为 Guest 中的修改而自动发生变化。
5.	模型请求使用宿主 Vault 的真实凭据成功完成，但 Guest 不含真实 Key。
6.	Guest 对未授权网络和宿主文件的访问被真实拒绝，而非仅有告警界面。
7.	至少一个 MCP 服务通过 Host Gateway 完成 tools/list 和一次受控 tools/call。
8.	演示包含正向成功 + 负向阻拦 + 审核导出，并记录清楚的已知限制。
若只能完成 CLI 演示，仍可交付技术原型；若安全隔离测试未通过，则必须将成果称为“Agent 运行环境原型”，而不是“安全沙箱已完成”。
附录 A：技术验证顺序
依赖顺序：QEMU 启动 → Guest 控制通道 → Runner PTY → Codex 单 Agent → 模型代理认证 → 网络拒绝绕过 → MCP Gateway → 第二个 Manifest → 项目导出 UI → 次平台验证。
最早应写的是可以复现的 Spike：在目标 Windows/macOS 主机上，启动一个 QEMU Guest，让它通过专用通道向 Host Gateway 发一个 HTTP 请求，Host 用真实 Key 请求上游并返回响应，而 Guest 不具备其他网络出口。 这个 Spike 直接决定后续是否能如期完成安全需求。
附录 B：参考资料（截至 2026-10-24）
1.	QEMU x86 microvm：https://www.qemu.org/docs/master/system/i386/microvm.html
2.	QEMU ARM virt：https://www.qemu.org/docs/master/system/arm/virt.html
3.	QEMU 虚拟化加速简介：https://www.qemu.org/docs/master/system/introduction.html
4.	QEMU Windows WHPX：https://www.qemu.org/docs/master/system/whpx.html
5.	QEMU Guest Agent/控制通道：https://www.qemu.org/docs/master/interop/qemu-ga.html
6.	Claude Code 设置：https://code.claude.com/docs/en/settings
7.	Claude Code MCP：https://code.claude.com/docs/en/mcp
8.	Codex 配置参考：https://developers.openai.com/codex/config-reference
9.	Codex MCP：https://developers.openai.com/codex/mcp
10.	MCP Transport 规范：https://modelcontextprotocol.io/specification/2025-11-25/basic/transports
11.	DeepSeek Harness CLI：https://github.com/deepseek-ai/deepseek-harness/blob/master/apps/cli/README.zh.md
12.	DeepSeek Harness MCP Client：https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/mcp/mcp-client/README.zh.md
文档中的 URL、Manifest 和命令片段作为架构参考与配置草案，不代表已在你们的目标机器上完成集成验证。具体 Agent 参数应与实际锁定的版本进行回归测试。
