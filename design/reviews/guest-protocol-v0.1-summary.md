# Guest v0.1 候选接口与线协议评审

日期：2026-10-24。范围：`design/guest-protocol-v0.1.md`、`design/schemas/guest-protocol-v0.1.schema.json`、两组 fixture、`design/interfaces-and-fakes.md` 与校验脚本。没有修改 `internal/host/vm` 实际契约或生产实现，没有执行 Git 写操作。

## 结果

定义了 Host 消费方 `GuestClient`/`Terminal` 候选方法、控制/终端帧及方向、8 个 RPC 的精确输入输出、事件与错误、状态快照、终端关闭/Fin、超时与有界队列、operation 去重指纹和提交点。`VMBackend` 直接引用实际接口；工作区导入、Gateway 和真实 Agent 不在此候选协议的已验收范围。

`python3 scripts/validate-guest-protocol.py` 验证 Draft 2020-12 Schema、42 个负载正反例和 2 个参数指纹向量，通过；`git diff --check` 与 Python 编译检查通过。文档与 fixture 变更没有再次运行 QEMU，历史原型普通/race 结果仍只适用于固定工作负载和原型字段，不能代替新 Schema 真实通道验证。

## 评审

codexx 使用 `xapi`、`gpt-6.1-sol`、medium，复用 session `01a16c9a-7265-7471-a521-c17617f5de1e`。首轮指出两项有效问题：按方法校验的 Response 不接受合法 ErrorResponse，以及先查引用再查已有 operation 导致改参重试错误返回 NOT_FOUND。已将每种 Response 改为 SuccessResponse/ErrorResponse 互斥联合，明确“结构与范围 → 已有 operation → 新操作引用与容量 → 原子预留”，补充业务错误与冲突用例；同 session 复审未发现剩余具体问题。原始结果见本地 `artifacts/review-sessions/guest-channel/codexx-contract-{1,2}.txt`。

Claude CLI 复用原 session `876c2b2c-636b-4103-9bc1-b1e978f65689` 只读评审，180 秒超时，退出码 124、JSON 输出为空；没有 `modelUsage`，不能声称取得 Claude 或其他实际模型的评审结论，也不能把双评审记为完成。

## 冻结状态

**候选已定义，尚未冻结。** 下一步须让生产 Host/Guest/Fake 共用 Schema 与 fixture，完成新增字段、完整容量边界、guestd 状态丢失、连接代次恢复和真实目标平台/Agent 验证。各项未测内容见协议第 9 节；本轮 codexx 文档复审不能替代它们。
