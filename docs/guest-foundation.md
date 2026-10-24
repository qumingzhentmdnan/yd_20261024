# Guest 公共协议与 Fake 实现交付

本轮交付是可供后续实现复用的代码基础，协议仍为 v0.1 候选。没有新增 Go 依赖，没有修改 VM 生命周期契约。

## 文件与职责

| 路径 | 已实现 |
| --- | --- |
| `internal/protocol` | 强类型 DTO、严格 JSON、方法参数/结果关联、SAGP framing、UUID/nonce、操作指纹 |
| `internal/host/guest/client.go` | Host 消费方 `Client`、`Terminal` 和本地错误 |
| `internal/testkit/fakeguest` | boot/连接/session/组状态、固定去重结果、可控故障、独立退出等待、有界终端 |
| `internal/testkit/guestcontract` | Fake 与后续真实 Client 共用的外部行为测试 `Run(t, Factory)` |
| `design/schemas`、`design/fixtures` | 精确负载 Schema、45 个正反例、2 个操作指纹向量 |
| `internal/protocol/testdata/frames.json` | 公共编解码与 Go/Python spike 共用黄金字节 |

依赖方向为 Fake → Host 消费方接口 → protocol，Fake 另外复用 vm.VMInstance。protocol 不依赖 Host/Guest，真实两端不得另起同名 DTO 或复制 fixture。Fake 的登记内容是显式测试数据，不代表 Manifest 或 Workspace 导入实现。

## 保证与边界

共享行为覆盖握手前拒绝、重复 Hello、调用前取消无副作用、同 operation 并发启动/冲突优先、停止后固定重试结果、独立等待者、退出快照不可变、终端独占/Resize/Detach/Fin、断连唤醒。Fake 专属测试补充提交前取消、提交后响应丢失与重连重试、容量、输出背压、环形缓冲以及残留进程组。

生产 Host 连接装配、RPC Client/Handler、请求追踪、真实 PTY/进程组 Runner、Manifest/Import、Gateway/Bridge 和 guestd 崩溃恢复尚未实现。协议编解码不负责设备超时；高层 Fake 不负责模拟 wire pending/request 限额或 Bind token。真实实现须在这些边界继续验证。

当前新增实现通过 Linux 本机 Go 测试、vet 和 race；具体最终结果与 review 记录见 [验收记录](../design/reviews/guest-foundation-summary.md)。既有 spike 的真实 VM 结果只证明旧固定工作负载原型，本轮未把新公共 DTO/Client 接入真实 VM。跨平台尚未验证。

## 后续工作

在新 worktree 中按照 [Guest 实现任务](tasks/guest-agent.md) 开发真实传输与执行层，可直接使用 [Agent 提示词](tasks/guest-agent-prompt.md)。新 worktree 从提交创建，不自动继承其他工作区的未提交/未跟踪文件；创建时应选择已包含本基础代码的提交。本基础按用户授权提交，未创建新分支或 worktree。
