# Reviewer Session 复用验证

验证日期：2026-10-24。工作目录：`/home/yedong/workspace/sandbox`。

先完成实际调用验证，再更新 `AGENTS.md`。没有创建其他 worktree，没有执行 Git 写操作。

## 方法与结果

对每个 CLI 分别生成随机标记，第一轮要求记住标记并仅回复 READY；第二轮通过明确的原 session ID 恢复，仅要求返回上一轮标记，不再次传入标记。第二轮禁止调用工具；Claude 另使用 `--tools ''` 禁用工具。

| 工具 | Session ID | 恢复 ID 一致 | 标记准确返回 |
| --- | --- | --- | --- |
| codexx | `01a16c42-e5aa-75a2-beaf-b332595fce52` | 通过 | 通过 |
| Claude CLI | `6dc86a5d-9cc7-4396-9f77-d69ec5ee6aca` | 通过 | 通过 |

两个恢复命令均退出 0；codexx 第二轮事件只有消息输出，没有工具调用；Claude 返回 `is_error: false`。原始输入、输出和断言结果保存在本地忽略目录 `artifacts/review-sessions/session-reuse-check/`。

codexx 使用 `gpt-6.1-sol` 和 `medium`。Claude CLI 返回的实际模型为 `deepseek-flash[1m]`，本次证明的是该 CLI 的会话持久化与恢复能力，不代表验证了 Anthropic Claude 模型。

## 命令依据与边界

已查看本机 `codexx exec --help`、`codexx exec resume --help` 和 `claude --help`。实际验证了 `codexx -a never -s read-only exec resume ... SESSION_ID PROMPT` 与 `claude -p --resume SESSION_ID ... PROMPT`；首次创建使用普通持久化 exec 和 Claude 的 `--session-id UUID`。

Codex 的非交互模式及按 ID 恢复方式也见 [OpenAI 官方说明](https://learn.chatgpt.com/docs/non-interactive-mode)。

本次在当前 worktree 完成两轮上下文验证，没有验证跨 worktree 共享会话，也不应共享。验证 session 的主题仅为连通性检查，后续业务 review 属于无关上下文，应独立建立并记录业务 session。只读评审的完整命令和保存规则以根目录 `AGENTS.md` 为准。
