# Guest RPC 原型验证

日期：2026-10-24。范围：独立 Python Host、Go Guest 和镜像内固定 shell 工作负载，在 Linux/amd64、QEMU microvm、显式 TCG 下验证候选控制与终端协议。没有修改 `internal/host/vm` 共享契约或新增生产依赖，没有执行 Git 写操作。

## 实现与修复

源码和复现命令见 `scripts/spikes/guest-rpc/README.md`。Host/Guest 使用不同语言独立编解码，共用黄金字节 fixture；这不是正式 SDK 或生产 guestd。

- Detach 等待已绑定端口的输入输出循环退出、设备关闭，再释放授权，避免旧读取者消费新 Bind；已取消授权不能再绑定。
- PTY 输出结束后再次排空缓冲才发送 Fin，Host 检查退出尾部 `EXIT_TAIL_READY` 必须出现在 Fin 前。
- 固定 stubbornchild 工作负载在 READY 前忽略 TERM/HUP，避免 session leader 退出引发的 HUP 使测试子进程提前退出。
- 控制重连等待 `CONTROL_CLOSED:<connectionID>`，确认旧代次任务退出、设备关闭；不再使用固定睡眠推测完成。控制台诊断只服务实验，不是生产恢复协议。
- 保留 Stop 错误诊断，后续失败可区分取消、宽限期和资源错误；未因历史偶发失败放宽业务断言。

## 验证结果

最终代码普通探针 `artifacts/guest-rpc-spike/final-plain-2/results.json` 与 race Guest 探针 `artifacts/guest-rpc-spike/final-race-1/results.json` 均 `passed=true`。后者 `race_guest=true`，Guest 日志无 `WARNING: DATA RACE`。两轮所有检查均通过，基础盘摘要不变、自有 QEMU 已回收。

同代码又运行普通探针 `final-plain-3`，28 项检查再次全部通过，基础盘未变、进程已回收。

| 检查 | 结果 |
| --- | --- |
| Python/Go 黄金帧、分片 Hello、握手前请求 | 通过 |
| 重复、未知、迟到响应及断线唤醒全部等待者 | 通过 |
| 同 operation 并发 Start 只创建一个进程、参数冲突 | 通过 |
| UID 1000 PTY、输入输出、初始窗口和 Resize RPC | 通过 |
| Detach 立即重绑、旧 token/未知 stream 拒绝 | 通过 |
| 实际 exit 7、退出事件和 Fin 前尾部字节 | 通过 |
| Cancel Stop 不回滚进程、显式强停、残留进程组成员 | 通过 |
| 慢终端有界缓冲、控制继续推进、Stop 不依赖终端读取 | 通过 |
| 控制重连后 operation/退出状态保留 | 通过 |
| 畸形头、超限、半帧、重复 JSON key/requestID、旧 epoch/connection | 通过 |
| 协议错误关闭唤醒请求，不自动停止工作负载 | 通过 |

根仓库与独立 spike module 分别运行 `go test ./...`、`go vet ./...`、`CGO_ENABLED=1 go test -race ./...`，均通过。Python 编译和两个 shell 脚本的 `sh -n` 通过。根仓库 Fake/契约测试与真实 VM 结果分别记录，不能互相替代。

历史失败不改记为成功：旧 run-4 失败，run-3 虽成功但未覆盖后来新增的 stubbornchild；本轮 fix-1 强停失败且历史证据不足以确定根因，fix-2 最终控制重连超时。final-plain-1 在 Guest 内核启动阶段超过 30 秒，未进入业务检查；同代码 final-plain-2 成功。所有轮次原始日志均保留。

## 评审

codexx 使用 xapi、`gpt-6.1-sol`、medium；按用户要求创建的 session `01a16c9a-7265-7471-a521-c17617f5de1e` 完成发现问题、修复复审。首次三项问题及后续重连等待问题已处理，最终 `codexx-fix-2.txt` 未发现剩余具体有效问题，并核对最终两轮真实探针结果。

Claude CLI 复用 `876c2b2c-636b-4103-9bc1-b1e978f65689`；第一轮恢复等待近 8 分钟无结果后终止，退出码 143；再以同一 session 提供聚焦的当前源码复审，180 秒无结果超时，退出码 124。两次均未返回结果、session_id 或 modelUsage，不能记为评审通过。启动诊断显示 `deepseek-flash[1m]`，不能据此声称 Anthropic Claude 已评审。最终状态另记本地 `artifacts/review-sessions/guest-channel/validation-status.json`。所有原始输出、模型及 session 记录均保存在本地忽略目录，不提交。

## 未验证范围

没有验证 KVM/WHPX/HVF、Windows/macOS/arm64、实际 Claude/Codex Agent、生产 Gateway/Vault、导入导出、所有协议容量上限或产品安全边界。固定工作负载成功不代表生产协议已冻结，也不能据本轮通过断言历史强停问题根因已经确定。
