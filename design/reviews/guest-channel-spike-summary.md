# Guest 通道实验与协议候选评审

日期：2026-10-24。范围：`scripts/spikes/guest-channel/`、设备结果说明和 `design/guest-protocol-v0.1.md`。没有修改 VM 共享接口或生产实现，没有执行 Git 写操作。

## 实测

最终代码使用独立 QEMU/microvm/TCG 完成 `run-5`，结果保存在本地 `artifacts/guest-channel-spike/run-5/results.json`：

- 控制双向、非 root PTY 和 Host 控制窗口调整通过。
- terminal/proxy 各输出 33554432 个零字节，慢读期间 20 次控制 ping 均通过；恢复读取后字节计数正确。
- 子进程实际退出码为 7，socat 返回 1，分别记录。
- 控制重连的 Guest 设备代次从 1 增加至 3。
- QMP quit 后三端口均读到 EOF，基础盘摘要未变，自有 QEMU 已回收。
- 本地 socketpair 单独验证“先收到残留数据再 EOF”和 generation 响应分片；默认沙箱禁止 socket 创建，按正常权限机制执行后通过。
- 两个 shell 脚本语法检查、Python 编译检查与 `git diff --check` 通过。未改 Go 生产代码，不以 Go 测试替代真实设备实验。

## codexx

模型 `gpt-6.1-sol`，推理 `medium`。首评与复审均使用 session `01a16c53-2774-7052-a351-18b2a1d1c580`，JSONL 的 thread ID 一致，两轮退出码均为 0。

首轮提出四项有效问题，均已修复：

1. AttachTerminal 请求增加 Host 分配的 streamID，响应回显，统一分配约定。
2. EOF 检查改为在固定期限内排空残留数据，直到真正读到 EOF。
3. generation 读取完整数值并断言重连后严格增加。
4. 基础盘摘要读取成功后才创建临时端点目录，避免缺少素材时泄漏目录。

另外显式关闭 QMP makefile。复审重新读取当前文件及 run-5 结果，未发现剩余具体问题。

## Claude CLI

使用 session `876c2b2c-636b-4103-9bc1-b1e978f65689`。首轮 240 秒超时且没有返回结论，未记为通过；继续使用相同 ID 恢复评审，没有因超时另开 session。恢复调用直接提供当前文件内容并禁用工具，避免文件读取往返。

恢复调用在 180 秒后再次超时，没有返回结论，未记为通过。本轮只有 codexx 获得完整评审及复审结果。CLI 启动提示实际模型为 `deepseek-flash[1m]`，不能声称已获得 Anthropic Claude 模型评审。

## 结论边界

独立物理端口的设备实验通过，当前拓扑可作为 Linux/amd64 的候选。协议文档尚未冻结：Hello/epoch/connection、业务 operation 去重、Cancel、正式进程组停止、畸形帧与共享黄金 fixture 仍需最小原型验证。不将文本探测、socat 返回码或物理重连当作这些协议验收的证据。

本次新建评审 session 的原因是此前持久化 session 仅用于随机标记连通性测试，与本任务上下文无关；原因和旧 ID 已记录在本地 `artifacts/review-sessions/guest-channel/context.json`。
