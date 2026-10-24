# Guest 通道补充验证

状态：设备实验通过；Guest 线协议未冻结。

后续独立 Host/Guest 固定工作负载原型及修复验证见 `guest-rpc-spike-2026-10-24.md`。本文保留设备实验的原始范围；原型通过不代表生产协议冻结。

环境沿用 `docs/validation-2026-10-24.md`：Linux/WSL2、amd64、QEMU microvm、显式 TCG，Guest 为 BusyBox/initramfs。没有 KVM、其他平台或正式 Agent 镜像验证。

## 实测结果

复现入口为 `scripts/spikes/guest-channel/README.md`；最终版本成功一轮输出位于本地 `artifacts/guest-channel-spike/run-5/`。每轮创建独立 overlay 与端点，未修改已有素材。

| 项目 | 结果 | 对实现的约束 |
| --- | --- | --- |
| 控制双向通信 | 通过 | 控制端口可用于独立请求响应 |
| 非 root PTY | UID/GID 1000，`/dev/pts/0` | 正式 Runner 单独负责 PTY 初始化与降权 |
| Host 控制窗口 | 控制端执行调整后，终端查询为 40×120 | 设备能力可用，尚未验证正式 Resize RPC |
| 双慢读者 | terminal 和 proxy 各输出 32 MiB，Host 暂停读取；控制 20 次 ping 均成功 | 保留独立端口和独立调度，不能把数据写操作放在控制处理循环 |
| 控制响应耗时 | 本轮 20 次约 0.22–0.81 ms | 单轮探测观测值，不是性能保证或延迟分位数 |
| 背压恢复 | 恢复读取后，两路均收到恰好 33554432 个零字节 | 本轮字节未丢失；不代表业务层缓冲上限已验证 |
| 进程退出状态 | shell `exit 7`；socat 返回 1；直接子进程记录为 7 | `SessionExit` 必须来自 Runner 的进程等待，不能使用桥接器返回码 |
| 终端退出后控制 | ping 继续成功 | 终端进程、端口连接、VMM 是三个不同生命周期 |
| 控制断开与重连 | Guest 观察到 EOF，重开设备后原 QEMU socket 可再次通信 | 必须让旧连接授权失效；不能靠端口物理存在跳过 Hello |
| VMM 退出 | QMP quit 后 QEMU 返回 0，三个 Host 端口读取均 EOF | Host 关闭连接时仍须主动唤醒 RPC 等待者，不依赖额外进程退出事件 |
| 回收和基础盘 | 自有进程已回收，基础盘 SHA-256 不变 | 后续验证继续使用独立资源 |

首次实验暴露 socat 的 EXEC/PTY 组合不支持 `link` 选项，已删除该用法；未将失败实验记为通过。第二轮证明 socat 返回码不能充当 Agent 退出码，第三轮增加直接子进程状态采集后通过。

源码核对也支持将设备状态与业务状态分开：Linux 的 [virtio_console.c（v6.8）](https://github.com/torvalds/linux/blob/v6.8/drivers/char/virtio_console.c) 在 Host 断开且无缓存数据时读取返回 0；QEMU 的 [virtio-console.c（v8.2.2）](https://github.com/qemu/qemu/blob/v8.2.2/hw/char/virtio-console.c) 对通用 serial port 实施端口背压，并处理 chardev 开闭通知。源码说明不替代当前内核软件包上的实测，也不保证重连时业务帧不串代。

## 推荐拓扑与边界

建议以同一个 virtio-serial controller 下的独立 `safeagent.control`、`safeagent.terminal`、`safeagent.proxy` 端口作为当前 Linux/amd64 候选。QEMU 参数与此前设备实验一致，实际完整 argv 随每轮输出保存。

v0.1 先使用 control 和 terminal；proxy 仅用于技术验证，业务 framing 不随本次冻结。不能让文件或代理复用控制 JSON 传输 body。暂不引入一个端口内的多业务 multiplex，也不因当前设备可重连就承诺业务透明恢复。

真实 QEMU 后端 Agent 继续只实现现有 VM 生命周期接口。本次没有改动 `internal/host/vm` 或要求其提前提供业务通道接口。连接端点如何交给 Host 的适配器，应在最小协议原型通过后集中确定。

## 尚未通过的协议门槛

设备实验没有实现正式 Hello、epoch/connection 身份绑定、Prepare/Start operation 去重、业务进程组停止、取消与半关闭、畸形帧处理和共享黄金 fixture。最小协议候选见 `design/guest-protocol-v0.1.md`；只能据此继续做原型验证，不能让 Host/Guest Agent 各自开始生产实现。
