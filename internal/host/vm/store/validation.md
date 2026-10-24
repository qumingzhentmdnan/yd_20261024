# SQLite 持久化验证记录

2026-10-24，在 qemu-backend-agent worktree 执行。

## 实现与边界

Manager 统一保存配置、操作意图、epoch 和退出结果；Driver 负责私有资源、进程和独占资源锁。SQLite 驱动为 modernc.org/sqlite v1.39.1。新增 Driver 内部执行边界及 ErrExitUnavailable/ErrClosed；对外 VMBackend 六个方法签名不变，fake 和共享 suite 未修改。

恢复支持 Created/Stopped 和已持久化历史，未完成运行转 Unknown。没有实现存活进程接管、未知运行的自动核查、QMP 或跨平台支持。Unknown 阻止启动和删除，不伪造退出结果。

## 检查

- go test ./...：通过，默认不启动真实 VM。
- go vet ./...：通过。
- CGO_ENABLED=1 go test -race ./...：通过。
- SAFEAGENT_QEMU_INTEGRATION=1 CGO_ENABLED=1 go test -race ./... -count=1 -timeout=3m：通过。
- git diff --check、具名函数中文注释检查：通过。
- /proc 按真实 argv[0] 检查测试结束后的 QEMU 进程：无残留。

Manager 非真实 Driver 测试覆盖共享契约、重开恢复、Unknown、清理重试、异常退出、第二写者，以及 SQLite 触发器注入退出、Running 和 Launch 失败状态提交错误。真实 TCG 测试覆盖共享契约、重开沿用原 overlay、真实退出结果恢复、启动失败、基础盘 SHA-256 不变、配置发布中断恢复。存储测试覆盖固定结果不可覆盖、外键删除、数据库路径别名及绑定冲突。

真实验证仅 Linux/amd64/TCG，不是 KVM、Guest 就绪或跨平台证明。

## Review

codexx 在同一 worktree、同一 session 上进行五轮只读评审，显式使用 xapi，模型参数 gpt-6.1-sol / medium。四轮指出的问题已修复并加针对性验证；第五轮结论为“无有效问题”。Reviewer 自身未执行测试，上述测试由主 Agent 执行。

Claude CLI 恢复同一 worktree 已有 session，配置模型 deepseek-flash[1m]；本轮超过十分钟没有最终 JSON，stderr 提示 unrecognized_model，终止本轮进程，不能记为通过或声称取得实际 modelUsage。原始 session、提示词、各轮输出和失败记录保留在 Git 忽略的 artifacts/review-sessions/，不提交运行记录。
