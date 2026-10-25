# 交给实现 Agent 的提示词

请在用户准备的新 worktree 中，阅读当前仓库的 `AGENTS.md` 和 `docs/tasks/guest-agent.md`，按照任务文档完成真实 Host RPC Client、Guest Handler 和受控 PTY Runner 的实现、验证及评审。请完成代码与验证，不只输出方案。

开始前确认当前 worktree 已包含 `docs/guest-foundation.md` 所列公共协议与 Fake 基础代码。新 worktree 不会自动继承其他工作区的未提交文件；若基线缺失，请明确报告缺失项。

复用现有 `internal/protocol`、`internal/host/guest`、`internal/testkit/guestcontract` 和 `internal/host/vm` 生命周期契约，不另起协议或接口。具体文件范围、实现顺序和验收条件以 `docs/tasks/guest-agent.md` 为准。验证脚本和旧 spike 可用于复现设备行为，生产实现复用公共编解码与 fixture。

先实现固定受控工作负载的真实通信闭环，再验证进程组、PTY、断连、取消、背压和输出排空等边界。分别报告 Fake、真实 VM 和跨平台验证结果；未运行的项目明确标注，不将旧原型结果当作新增实现的验收结果。

完成后调用 codexx 做只读 review，首次和恢复均显式指定 `-c 'model_provider="xapi"'`，使用模型 `gpt-6.1-sol`、推理强度 `medium`。在本 worktree 的 `artifacts/review-sessions/` 记录 session、绝对路径、任务主题和模型；同一任务复用本 worktree 原 session，不跨 worktree 复用。修复有效问题、重跑相关检查后，在原 session 复审并记录结论。

遵守当前仓库的中文注释和实现约定。所有 Git 写操作须有用户明确授权；不要自行暂存、提交、创建分支或 worktree。
