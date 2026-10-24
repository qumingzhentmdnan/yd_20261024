# 仓库开发约定

## 注释

- 所有新增和修改的代码注释使用中文，标识符保持代码中的原名。
- 每个具名函数和方法的声明上方必须有 `// 函数名/方法名 作用说明`；接口方法、私有方法、构造函数和测试辅助函数也遵守此规则。
- 每个结构体字段上方必须有 `// 字段名 作用说明`，包括私有字段；说明必要的单位、生命周期和零值语义。
- 较长的方法在关键步骤、状态转换和并发边界前使用 `//` 解释意图；不逐行翻译代码，不用注释掩盖可以直接简化的逻辑。

## 实现风格

- 不写防御性代码：不为已确定不会发生的内部状态增加兜底，不重复校验已经保证的条件，不添加静默降级、无依据的默认值或吞错分支。
- 外部输入、文件与进程资源边界的必要校验，以及契约要求的状态、权限、epoch 检查必须保留；每项检查须有明确的来源或调用场景。
- 错误在实际发生的位置明确返回，不用通用封装掩盖错误，不因“以后可能需要”增加兼容层、抽象工厂或扩展接口。
- 可以使用 `github.com/samber/lo` 简化确有收益的集合操作；以可读性为准，不强制引入依赖，也不以链式调用替代更直观的简单代码。

## Git 操作

- 暂存、提交、创建分支或 worktree、回退和恢复暂存区等 Git 写操作，须先获得用户明确授权；授权范围内的必要步骤无需重复确认。
- 提交信息必须使用中文，准确概括本次最终变更。

## 协作与验证

- VM 生命周期的当前契约位于 `internal/host/vm`，后续真实后端复用该定义及 `internal/testkit/vmcontract`，不在各 worktree 中另起接口。
- 只修改当前任务拥有的文件；变更共享接口或新增依赖时说明具体原因。
- 按变更运行相关测试。并发代码运行 race 检查，缺少环境时明确标注，不能将未运行记为通过。
- 完成代码后按下方流程调用 `codexx` 做只读 review，处理有效问题，并在原 session 中复审；记录结论和未解决问题。
- Fake 测试、真实 VM 验证和跨平台验证分别报告，不相互替代。

## Reviewer 调用与 Session 复用

- 在当前 worktree 根目录执行命令。每个 worktree 单独维护 codexx 的 session，不跨 worktree 混用。
- 将 session ID、worktree 绝对路径、任务主题和实际模型记录在当前 worktree 的 `artifacts/review-sessions/`，不提交运行记录。下方示例在该目录保存 `codexx.id` 和评审输出。
- 同一任务的设计讨论、实现评审、修复和复审尽量复用原 session。首次无 session 时创建；已有 session 时，仅在上下文无关时才允许创建新 session，并记录原因及旧 ID。不要因一次评审结束、超时或上下文变长就另开 session。
- session 无法恢复时先检查 ID、工具错误和登录状态，明确报告问题，不静默换新。对同一个 session 的调用必须串行。
- 恢复时传入明确 ID，不使用 `--last` 或不带 ID 的 `--continue`。不要使用 `--ephemeral`。
- 评审提示词应包含任务目标、文件范围（包含相关未跟踪文件）、当前改动和测试结果；复审时说明修复点，要求重新读取当前文件。要求 reviewer 只读，禁止修改文件、Git 写操作、读取凭据和递归调用其他 reviewer 或子 Agent。主 Agent 判断问题是否有效，修复后重新运行相关检查。
- codexx 调用时显式传入 `-c 'model_provider="xapi"'`，首次调用和恢复均适用；尤其恢复时须放在 `exec resume` 后，避免沿用 session 中保存的其他供应商。
- codexx 默认使用本会话约定的 `gpt-6.1-sol`、`medium`；用户明确指定时覆盖。
- 使用正常沙箱和权限机制；遇到限制按环境要求申请权限，不使用绕过审批或沙箱的参数。

### codexx

首次调用前，将具体评审要求写入 `prompt.txt`。以下命令仅在没有对应任务 session 时执行：

```sh
mkdir -p artifacts/review-sessions
review_dir="$PWD/artifacts/review-sessions"
codexx -a never -s read-only exec \
  -c 'model_provider="xapi"' \
  -m gpt-6.1-sol -c 'model_reasoning_effort="medium"' \
  --json -o "$review_dir/codexx-reply.txt" - \
  < "$review_dir/prompt.txt" > "$review_dir/codexx-events.jsonl"
```

从 JSONL 的 `thread.started` 事件提取 `thread_id`，保存为 `codexx.id`；同时检查命令退出码和错误事件，不能把生成 ID 当作评审成功。后续调用复用该 ID：

```sh
review_dir="$PWD/artifacts/review-sessions"
codexx -a never -s read-only exec resume \
  -c 'model_provider="xapi"' \
  -m gpt-6.1-sol -c 'model_reasoning_effort="medium"' \
  --json -o "$review_dir/codexx-reply.txt" \
  "$(cat "$review_dir/codexx.id")" "$(cat "$review_dir/prompt.txt")" \
  > "$review_dir/codexx-events.jsonl"
```

每轮输出在下一轮前归档，避免覆盖评审证据。
