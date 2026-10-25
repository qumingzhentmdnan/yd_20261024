# Guest 通道设备实验

用途：在真实 QEMU 中验证独立物理端口的背压、关闭、重新连接、PTY Resize 和子进程退出行为。这里的文本命令只用于实验，不是正式 RPC，不允许生产代码依赖本目录。

本机运行：

```sh
source artifacts/toolchain/activate.sh
python3 scripts/spikes/guest-channel/probe.py \
  --assets artifacts/validation \
  --output artifacts/guest-channel-spike/local-run
```

输出目录必须尚不存在，每轮保留独立结果。需要 PATH 上的 `qemu-system-x86_64`、`qemu-img`、`bash`、`cpio`、`gzip`、`find` 和 Python 3.11+。输入素材为此前手工准备的 BusyBox/glibc/socat rootfs、`workspace-base.raw` 和 `guest-kernel/boot/vmlinuz-6.8.0-147-generic`；素材被 Git 忽略，全新检出不能直接运行。准备过程及版本见 `docs/validation-2026-10-24.md`。

脚本复制 rootfs，覆盖实验 init，制作新 initramfs 和 qcow2 overlay；使用权限 0700 的独立 `/tmp/sa-channel-*` 目录建立 socket。无通用网卡、无宿主目录共享、无凭据、显式 TCG。结束时只关闭自己的 QEMU、关闭 socket 并删除本轮端点目录；磁盘、控制台日志、实际 QEMU argv 和 `results.json` 保存在指定输出目录。

实验使用固定 shell，通过 UID 1000 验证 PTY。控制端对唯一 `/dev/pts/0` 调用 `stty` 调整窗口；这不是正式 Resize RPC 的验证。终端和代理分别输出 32 MiB 零字节，暂停读取期间做 20 次控制 ping，再读取并验证总字节数。测试 shell `exit 7` 后分别记录 socat 退出码和直接子进程退出码。

控制端重连故意不做 Hello，用于证明设备重连不能替代业务握手。没有实现身份认证、operation 去重、流 framing、Cancel、正式 StopSession、实际 Agent 或模型调用。`results.json` 的 `passed` 仅表示上述设备检查通过，不能作为协议冻结依据。
