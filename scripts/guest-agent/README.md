# 真实 Guest 固定工作负载验证

此入口构建当前生产 `cmd/guest-agent`，通过生产 QEMU Driver、Host RPC Client、公共 codec 和 Linux Runner 验证固定登记工作负载。没有实现产品 Manifest 解析、Import、Gateway 或任意 Agent 执行。

从 worktree 根目录执行，素材使用已有 Linux/amd64 BusyBox/glibc rootfs、内核和不可变基础盘（本地忽略文件，不随检出交付）：

```sh
source /home/yedong/workspace/sandbox/artifacts/toolchain/activate.sh
python3 scripts/guest-agent/build-image.py \
  --assets /home/yedong/workspace/sandbox/artifacts/validation \
  --output artifacts/guest-agent/local-race --race
SAFEAGENT_GUEST_INTEGRATION=1 \
SAFEAGENT_GUEST_IMAGE=../../../../artifacts/guest-agent/local-race \
CGO_ENABLED=1 go test -race ./internal/host/guest/rpc \
  -run 'TestQEMU.*Integration|TestQEMUWireFailures|TestQEMUTerminalFaults' -v -count=1 -timeout=2m
```

`SAFEAGENT_GUEST_IMAGE` 由 Go 测试进程的包目录解析，也可直接传绝对路径。输出目录必须不存在。镜像中 UID/GID 固定为 1000；登记 Manifest 使用固定脚本的完整副本，配置及工作区为实际文件，入口对全部内容重新计算摘要。该简化登记只用于固定工作负载，不代表正式导入或配置系统完成。

真实测试创建独立 0700 临时资源根目录，由现有 VM Manager 创建/启动/停止/等待/销毁 QEMU。验证 Hello、GetState、Prepare、Start、Bind/Bound、UID 1000、初始窗口、字节回显、Resize、Detach/重绑、Host Fin、Stop、固定 Exit、Fin 前尾部和最终状态。Cleanup 使用独立 context，在 client 已关闭时仍可回收 VM，并核对基础盘 SHA-256 未变。`build.json` 记录构建命令及 initramfs 摘要，`content.json` 记录实际登记摘要。

当前装配不支持同 boot 控制重连：每实例只交付一次 control，断线后 Host 需回收 VM，以新 epoch 重启。Guest 以 boot ID、独占锁及不可重入标记拒绝丢状态后的同 boot 重建。控制断开只撤销授权，不通过 Client.Close 或 EOF 隐式 Stop 工作负载。终端单独 Detach/重绑已测试，但全部设备故障和容量门槛须在验收记录逐项报告。

宿主内存控制通道/Unix 终端 + 真实 PTY 的共享 `guestcontract`、Host wire 故障、容量和慢输出测试随普通 `go test ./...` 运行。Fake、宿主真实 PTY、真实 TCG、跨平台结果需分别记录，不相互替代。
