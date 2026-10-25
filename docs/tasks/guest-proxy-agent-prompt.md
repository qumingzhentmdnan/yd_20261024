# 交给 Guest Bridge / proxy 实现 Agent 的提示词

请在用户准备的独立 worktree 中，阅读 `AGENTS.md` 和 `docs/tasks/guest-proxy-agent.md`，完成 Guest-local HTTP Bridge → 独立 virtio proxy 通道 → 现有 Host Gateway → 本地 TLS/SSE 测试上游的最小真实 VM 链路。请完成设计、验证、代码、评审和结果记录，不只输出方案。

严格按“核对基线 → 最小协议与装配设计 → 可丢弃探针验证 → codexx 设计复审 → 正式实现 → 真实 VM 验收 → codexx 代码复审”推进；设计和探针的有效问题解决前，不开始正式生产实现。先完成单并发模型请求闭环，不扩成通用多路复用、文件传输、MCP、真实 Vault、产品 UI 或真实 Agent CLI。

基线必须包含 `44f3a1b` 合入的真实 Guest RPC/PTY、`631f2df` Workspace 和 `6742a52` Host Gateway。优先核对实际文件，不把文档中的旧“尚未实现”当作现状。新 worktree 不继承主工作区未提交文件；本任务两份文档和可选的 `docs/merge-validation-2026-10-24.md` 若未提交，需要由用户同步。缺少必要基线或任务文档时明确报告，不自行执行 Git 操作补齐。

复用现有 Gateway、VMInstance、QEMU Driver、Guest RPC、设备关闭实现及契约；不得另写 Gateway 授权/认证代理或把 proxy body 放入控制 JSON。具体文件归属、协议待定项、身份/取消/失联处理和验收条件，以 `docs/tasks/guest-proxy-agent.md` 为准。没有 `/dev/kvm` 时明确选择 TCG，不能把 TCG、宿主测试或旧 spike 结果记为 KVM/本次真实实现通过。

只调用 codexx 做只读设计及代码 review，不使用 Claude。首次及恢复均显式传入 `-c 'model_provider="xapi"'`、`-m gpt-6.1-sol`、`-c 'model_reasoning_effort="medium"'`。在本 worktree 的 `artifacts/review-sessions/` 记录 session、绝对路径、任务主题及模型；同一任务设计、实现、修复和复审复用本 worktree 的同一个 session，不跨 worktree 使用主工作区的 ID，不因超时换 session。处理有效意见，不盲目添加违反当前 Go 工具链事实或“不写防御性代码”要求的兜底。代码完成后另做冗余/防御性代码专项审查。

所有新增和修改的注释使用中文；每个具名方法/函数和每个结构体字段按 `AGENTS.md` 写名称开头的作用注释，长方法解释关键状态和并发边界。保留外部输入、资源、权限、身份和容量的必要检查，不加内部不可能状态的兜底、静默降级或无用抽象。

所有 Git 写操作必须有用户明确授权；不得自行暂存、提交、创建分支/worktree、合并或回退。不要修改其他 worktree、启动其他实现 Agent、调用 Claude 或读取真实凭证。正常使用环境权限机制，遇到限制按机制申请，不绕过沙箱。
