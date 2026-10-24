# Guest 合入后的基线验证

日期：2026-10-24。验证提交：44f3a1bf49510d514d3e6754f4b09bdf7d7c8a96（合并真实 Guest 控制与终端实现）。环境：Linux/amd64 WSL、Go 1.27.3；无 /dev/kvm，明确使用 QEMU microvm/TCG。

本轮只执行合并后的基线验证并记录结果，不修改生产代码，不实现 proxy/Bridge，不进行 Git 写操作。

## 命令与结果

先加载 artifacts/toolchain/activate.sh。以下检查全部退出 0，普通测试与 race 均使用 -count=1，不复用测试结果缓存。

| 检查 | 命令或内容 | 耗时 |
|---|---|---:|
| 协议 | python3 scripts/validate-guest-protocol.py；Schema、45 个负载正反例、2 个指纹向量 | 0.264 秒 |
| 静态检查 | go vet ./... | 0.368 秒 |
| 全仓测试 | go test -count=1 -timeout=3m ./... | 6.377 秒 |
| 全仓 race | CGO_ENABLED=1 go test -race -count=1 -timeout=3m ./... | 17.099 秒 |
| 重建 Guest | build-image.py --assets artifacts/validation --output 本次独立 guest-race 目录 --race | 1.820 秒 |
| 真实 TCG | 下述显式集成命令 | 72.515 秒 |

本次输出根目录为 artifacts/merge-validation/44f3a1b-20261024T103109Z。Guest 使用该目录中新构建的 initramfs，不沿用原 Agent 的镜像。initramfs SHA-256 为 6ade090f9eb640b822455807b7c720252d9d965e4810395b308a9497ac59df22，构建含 -race。

真实集成命令：

```sh
source artifacts/toolchain/activate.sh
SAFEAGENT_GUEST_INTEGRATION=1 \
SAFEAGENT_GUEST_IMAGE="$PWD/artifacts/merge-validation/44f3a1b-20261024T103109Z/guest-race" \
CGO_ENABLED=1 go test -race ./internal/host/guest/rpc \
  -run 'TestQEMU.*Integration|TestQEMUWireFailures|TestQEMUTerminalFaults' \
  -v -count=1 -timeout=4m
```

重建时使用 scripts/guest-agent/build-image.py --race，输出必须选择新的独立目录，不能覆盖本次证据。

## 真实 VM 验证

共 13 个独立 VM 场景全部通过：

- 正常闭环：Hello/GetState、Prepare/Start、Bind/Bound、UID 1000、终端字节 I/O、初始窗口/Resize、Detach/重绑、Host Fin、Stop、Exit、Guest Fin 及退出尾部。
- 六项共享契约：握手与预取消启动、并发启动/冲突/稳定重放、等待取消/固定退出、终端解绑/半关闭/停止、断连唤醒等待者、首次 CloseWrite 取消结果稳定。
- 三项控制故障：旧身份、半帧、超长帧。
- 三项终端故障：独立断线、旧 Bind、慢读 flood 与输出排空。

日志中 13 次独立清理断言均确认 Stop/Wait/Destroy 完成；额外按本次唯一镜像路径检查宿主进程，未发现残留自有 QEMU。

基础盘前后 SHA-256 均为 4e16706e345f0bacbae79893a423ef6c8c1b522f6f078caceaee30f8b07216fc。验证结束时 HEAD 未变化。

## 证据和边界

本次目录中保存 environment.json、各检查的命令/退出码/耗时 JSON、原始 .log、Guest 构建和内容登记，以及最终 summary.json。运行记录和镜像位于 Git 忽略目录，不作为源代码提交。

全仓测试包含 Fake 与宿主真实 Handler/PTY，但不等同于真实 VM；真实 VM 的结果以上述显式测试为准。没有验证 KVM、其他平台、同 boot 控制重连、真实产品 Agent、Workspace 导入或模型 Gateway 的 VM 链路。旧身份和控制损坏场景仍使用现有有界查询/关闭/回收策略，不能据此声称 virtio 关闭会自动产生 Host EOF。

结论：本次覆盖的合并基线检查全部通过，可以进入 Guest Bridge/proxy 通道的最小设计与可行性验证；尚未宣称完整产品链路打通。
