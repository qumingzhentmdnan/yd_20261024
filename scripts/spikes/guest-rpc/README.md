# Guest RPC 原型验证

独立 Python Host 与 Go Guest 在真实 virtio-serial 设备上运行候选协议，Guest 仅启动镜像内固定工作负载。本目录是独立 Go module，不属于生产 Host/Guest 实现。

从仓库根目录运行：

```sh
source artifacts/toolchain/activate.sh
(cd scripts/spikes/guest-rpc && go test ./... && go vet ./... && CGO_ENABLED=1 go test -race ./...)
python3 scripts/spikes/guest-rpc/probe.py --assets artifacts/validation --output artifacts/guest-rpc-spike/local-plain
python3 scripts/spikes/guest-rpc/probe.py --race-guest --assets artifacts/validation --output artifacts/guest-rpc-spike/local-race
```

输出目录必须尚不存在。输入素材、工具链与设备实验相同，见 `../guest-channel/README.md`；race Guest 还需要本机 C 编译工具链及素材中的 glibc。被忽略的本机素材不随仓库交付，全新检出不能直接运行。

逐项结果写入输出目录的 `results.json`，并保存 initramfs、工作负载副本、QEMU 参数及控制台日志。每轮使用独立 overlay、权限 0700 的临时 socket 目录，并只回收自己启动的 QEMU。基础盘摘要检查与进程回收分别记录。

覆盖共享黄金帧、分片 Hello、请求关联、重复/未知/迟到响应、启动 operation 去重与冲突、非 root PTY、初始尺寸与 Resize RPC、Detach/重绑、退出码与 Fin 尾部、Cancel、进程组 Stop 与显式强停、有界终端缓冲、跨控制连接去重、畸形帧和旧身份拒绝。控制重连等待含 connectionID 的 Guest 关闭诊断，不靠固定睡眠猜测代次切换；该控制台协同仅用于实验，不是产品恢复机制。

验证范围为 Linux/amd64、QEMU microvm、显式 TCG、固定 shell 工作负载。未验证 KVM、其他平台、实际 Claude/Codex Agent、Gateway/Vault、生产导入导出或全部协议容量边界；不能据此冻结生产协议或宣称安全验收完成。
