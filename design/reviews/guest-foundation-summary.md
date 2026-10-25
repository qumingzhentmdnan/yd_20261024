# Guest 公共协议与 Fake 验收记录

日期：2026-10-24。范围：本工作区新增公共协议、Host 消费方接口、有状态 Fake、共享契约测试与实现交接文档；不包含真实 RPC/Runner。

## 自动验证

| 层次 | 结果与范围 |
| --- | --- |
| Schema / 指纹 | `python3 scripts/validate-guest-protocol.py`：45 个负载正反例、2 个指纹向量通过 |
| 公共帧编解码 | 根模块测试：共用黄金字节、逐字节内存通道分片、方向/大小/半帧/EOF、严格 JSON、方法编码边界通过 |
| Fake 与共享行为 | `go test ./...` 通过；覆盖去重、提交竞态、断连重试、快照、终端、残留组及容量 |
| 静态与并发 | `go vet ./...`、`CGO_ENABLED=1 go test -race ./...` 通过 |
| 旧 spike 兼容 | 独立 Go module 的 test/vet/race 通过；Python `check_fixtures()` 读取迁移后的唯一帧文件通过 |
| 新公共实现真实 VM 集成 | 未运行；生产 Client/Handler/Runner 尚未实现 |
| 跨平台 | 未验证；本轮在 Linux 本机执行 |

既有真实 VM spike 结果见 `../../docs/guest-rpc-spike-2026-10-24.md`，不能替代本次新增实现的真实集成验收。

## 只读评审

codexx 使用本 worktree 的原 Guest 协议 session，显式指定 `model_provider="xapi"`、`gpt-6.1-sol`、`medium`。具体 session ID、绝对路径和逐轮输出保存在被忽略的 `artifacts/review-sessions/guest-channel/`，不提交运行记录。

首轮提出 3 个有效问题，均已修复：

1. 成功响应接受额外 `error:null`：严格字段检查现在拒绝可省略字段显式 null，新增 Schema/Go 共用反例。
2. CloseWrite 重复调用改写首次结果：Fake 保存首次尝试完成通知及结果，补共享重复/并发/调用前取消测试和输入锁等待超时回归测试；明确调用前取消也算首次尝试。
3. method 长度按字节限制：改按 Unicode 字符数校验，新增共享 64 字符正例和 65 字符反例，并验证未知 Unicode 方法交给 Handler。

原 session 第二轮只读复审通过，退出码 0，收到 turn.completed；未发现剩余有效问题或修复引入的死锁、并发发布和契约矛盾。事件中有长线程多次压缩可能影响准确性的提示，无调用失败；按会话复用约定继续使用原 session。Reviewer 独立运行了 Schema 及限定范围 diff 检查，Go test/vet/race 与 spike 结果采用主 Agent 的实际运行记录，未声称独立重跑。生产传输状态机、真实设备恢复、Runner/PTY 权限和跨平台仍需后续实现分别验证。

按会话最初约定另外尝试恢复原 Claude CLI session，限制为 Read/Grep/Glob。该调用报 `unrecognized_model`（诊断模型名 `deepseek-flash[1m]`），90 秒后超时退出 124，无 result 或 modelUsage；未获得评审结论，不能记为通过，也不能声称实际使用了 Anthropic Claude。
