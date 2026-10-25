# 有状态 Guest Fake

Fake 实现 `internal/host/guest.Client`，供 Host 编排和接口消费者开发；不启动 VM、进程、PTY、网络或后台任务，不接收任意命令。

使用顺序：

1. `New(Config)`：显式传入现有 `vm.VMInstance`、固定 `GuestBuild`、合法 `ImageDigest` 和唯一登记的 `protocol.Preparation`；没有默认测试内容。
2. `Boot.Connect(instance)`：同一 boot 只允许一个活动连接，instance 不匹配返回 STALE_INSTANCE。
3. `Hello` → `PrepareAgent` → `StartSession`。参数取自已登记内容，准备结果中的 preparedID 用于启动；每个新操作使用不同 UUIDv4 operationID。
4. `AttachTerminal` 返回已绑定终端；`StopSession` 后可 `WaitSession` 并读取到输出 Fin。关闭 Client 只断连接，不停止工作负载。
5. 旧 Client 关闭后可重新 Connect、Hello；boot 内操作结果、session 和输出保留。新 boot 必须重新 New，不能复用旧 boot 的去重表。

可直接参考 `client_test.go` 中 `config`、`setup`、`TestSharedContract`，无需复制实现。

## 测试控制

| API | 作用 |
| --- | --- |
| `NewGate`、`Inject` | 在 PrepareBeforeCommit、StartBeforeCommit、StartAfterCommit 确定性等待或注入单次错误；等待 Entered 后再取消/断连/Release |
| `Exit(exit, groupRunning)` | 固定主进程退出结果；显式保留残留组成员，不自动生成输出 Fin |
| `IgnoreTERM` | 模拟 TERM 无效，验证宽限期及显式强停 |
| `FeedOutput`、`EndOutput` | 发送原始输出及输出 Fin；绑定时 64 KiB 背压，未绑定时保留最近 64 KiB |
| `TakeInput` | 取出输入并释放背压 |
| `Window`、`DroppedBytes` | 检查模拟窗口和未绑定期间丢弃字节 |

CloseWrite 与 Write 串行，不注入 Ctrl+D、不关闭 PTY。只有正常输出结束产生 EOF；连接断开返回 ErrDisconnected。GetState 返回独立快照，主进程退出与组清空分开。已接纳操作的成功或失败在 boot 内固定；提交后丢响应不回滚创建。

Fake 替换的是高层 Client，不模拟 16 pending/4096 request、实际 requestID、Bind token、设备重开和线协议时限，也不验证真实 Manifest/Import/权限。共享帧测试与 Fake 行为测试分别运行，不能据此声称生产 RPC 或真实 VM 已通过。
