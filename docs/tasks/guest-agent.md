# 新 worktree：真实 Guest 控制与终端实现

## 目标与基线

在已包含 `docs/guest-foundation.md` 所列代码的基线提交上，实现 Host Client、Guest Handler 和固定受控工作负载 Runner，打通真实 QEMU control/terminal 通道。先完成固定工作负载闭环；Manifest/Import/Gateway 不以假数据声称生产完成。

开始前读取 AGENTS.md、`design/guest-protocol-v0.1.md`、`internal/protocol/README.md`、`internal/host/guest/client.go` 和现有 VMBackend/QEMU 实现。共享协议候选已集中定义；发现不满足实现需要时先说明具体差距，集中修改契约，不在 worktree 中另起接口。

## 文件范围与对接

- 复用 `internal/protocol` 全部 DTO/codec/fixture；Request/Response 走专用 Encode/Decode API。
- 实现 Host `guest.Client` / `guest.Terminal`，建议新增 `internal/host/guest/rpc/`。
- 建议在 `internal/guest/` 下实现 Handler 与 Runner，入口使用 `cmd/guest-agent/`。不要直接搬入 spike 共用参数大结构体或较弱 token。
- 通道装配复用现有 VMInstance 及 QEMU 私有端点；VMBackend 不返回业务连接，不扩展为 Guest RPC。若端点交付机制不足，明确列出与 QEMU 任务的共享改动后再实现。
- `internal/testkit/guestcontract.Run` 的 Factory 为每个场景返回独立、尚未 Hello 的 client 以及已登记固定内容的合法 Prepare/Start 参数；测试工作负载应持续运行直到显式停止。suite 会用真实准备结果填充 preparedID；Factory 用独立 context 清理自有进程与 VM，不能依赖已关闭 Client。

## 建议实现顺序

1. 内存通道上接通真实 Handler/Client，严格分离控制/终端端口，所有写入串行；测试使用现有帧黄金字节与负载正反例。
2. 实现可信 instance 绑定、Hello、连接代次及 Bind/Bound；旧 epoch/connection/token、重复/未知/迟到响应按候选契约处理。
3. 实现 boot 操作记录、原子预留与提交、固定成功/失败、并发重试。用故障闸门验证取消、断连、进程已创建但响应丢失；guestd 丢状态禁止同 boot 静默重建空表。
4. 实现非 root PTY Runner：固定允许的工作负载、会话领头进程和进程组、初始尺寸、Resize、唯一 Wait、TERM/宽限期/显式 KILL。主进程退出、组清空和输出 Fin 分开。
5. 实现唯一控制读循环、WaitSession 注册后查询及事件合并、pending/request 容量、首字节起算帧预算、绑定超时、退出后有界尾部排空、Close/CloseWrite 的固定结果。终端背压不能占满控制面；输出中止返回 ErrOutputIncomplete。
6. 接入真实 QEMU 设备，复用共享行为 suite，再补 wire 状态机/故障测试。旧 spike 仅作设备操作参考，不是当前生产协议实现。

## 验收

```sh
source artifacts/toolchain/activate.sh
python3 scripts/validate-guest-protocol.py
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...
```

另外记录真实 VM 命令、素材前提、平台、逐项断言、基础盘摘要未变及自有 QEMU 回收；至少完成 Hello → GetState → Prepare → Start → Bind/Bound → 原始字节 I/O → Resize → Stop → Exit/Fin/GetState。补充旧身份、分别断线、半帧/超长帧、取消/提交竞态、慢读时 Stop、输出排空超时和容量边界。没有环境的项明确写未运行，Fake 与真实 VM 结果分开。

运行 AGENTS.md 规定的 codexx 只读评审，显式 `-c 'model_provider="xapi"'`；在该 worktree 自己的 artifacts/review-sessions 记录 session/路径/模型。同任务复用该 worktree 原 session，不能沿用本基础 worktree 的 ID。处理有效问题后原 session 复审；所有 Git 写操作按授权执行。
