# QEMU VM 后端

由 Host 调用方组装 Driver 和实现 `vm.VMBackend` 的持久化 Manager：

```go
driver, err := qemu.NewDriver(qemuPath, qemuImgPath, dataRoot)
if err != nil {
    return err
}
manager, err := vm.NewManager(ctx, databasePath, "qemu", driver)
// 检查 err 后调用 manager.Create/Start/Inspect/Stop/Wait/Destroy。
// 所有本进程 VM 停止、结果提交完成后调用 manager.Close()。
```

`qemuPath`、`qemuImgPath`、`dataRoot` 必须是绝对路径。资源根目录需归当前用户所有且权限为 0700。SQLite 数据库绝对路径由调用方通过 `databasePath` 指定，QEMU 包不创建 Manager 或决定数据库位置；每个 VM 的差分盘位于 `<dataRoot>/<ID>/overlay.qcow2`，基础盘不会被删除或覆盖。当前基础盘格式为 raw。

`vm.Manager` 负责配置幂等、资源级串行协调、epoch 分配、运行历史和持久化；`vm/store` 使用 SQLite，独立于 QEMU；`qemu.Driver` 负责磁盘和进程。其他后端实现 `vm.Driver` 后可复用 Manager。一个 Manager 对应一个后端和资源根目录；数据库绑定 Driver 的资源身份，不允许换根目录继续操作原记录。数据根目录由 Driver 独占锁保护；即使使用不同数据库，第二个 Manager 也不能同时管理同一根目录。

支持 Linux/amd64 microvm，TCG 或显式 KVM；KVM 不可用不回退。默认无通用网卡、无宿主目录共享。`Start` 只表示 VMM 进程创建成功，不表示 Guest 就绪。microvm 不承诺 ACPI 优雅关机，因此必须显式允许 `ForceAfterTimeout` 才能强停；没有 Guest RPC 或 QMP 实现。

## 持久化与恢复

Manager 在副作用前提交操作意图，结束后提交观察结果；不会持 SQLite 事务等待进程或执行资源删除。数据库主文件持有独占文件锁，第二个 Manager 打开同一数据库会报错。已完成的退出结果不可覆盖，Destroy 清理成功并删除数据库记录前保留历史。

重开后可以查询 Created/Stopped 配置和已保存的历史退出结果，并继续使用原 overlay。创建或删除中断保留操作阶段，调用 Create/Destroy 可继续处理。配置和 overlay 都通过临时文件重命名发布；未完成的临时输出不会被识别为完成资源。

原 Running/Starting/Stopping 记录恢复为 Unknown，禁止自动 Start/Destroy。不接管服务重启前仍在运行的 QEMU，也不依靠 PID 猜测进程身份；没有取得的真实退出结果通过 `vm.ErrExitUnavailable` 报告，不编造退出码。Unknown 运行当前需人工核查和后续恢复工具处理，不能直接删除数据库后重启。

退出结果提交失败时，真实结果保留在内存；后续 Wait 或针对当前实例的查询/控制会重试提交。未提交结果时 Close 拒绝成功。进程结束后若数据库持续无法写入且宿主服务崩溃，该结果仍可能无法恢复；SQLite 事务不能原子覆盖进程与文件副作用。

## 验证

默认 `go test ./...` 不运行 QEMU、不下载镜像。真实 TCG 集成测试需要本机 QEMU、qemu-img、内核、initramfs 与 raw 基础盘；当前测试默认使用原工作区的已验证 artifacts，可通过 `SAFEAGENT_QEMU_PATH`、`SAFEAGENT_QEMU_IMG_PATH` 覆盖工具路径。

```sh
source /home/yedong/workspace/sandbox/artifacts/toolchain/activate.sh
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...
SAFEAGENT_QEMU_INTEGRATION=1 CGO_ENABLED=1 go test -race ./internal/host/vm/qemu -count=1 -timeout=3m
```

真实测试复用 `vmcontract.Run`，另验证重开数据库、同一 overlay、真实退出结果恢复、启动失败和基础盘哈希不变。Manager 的非真实 Driver 测试独立覆盖契约、清理失败重试、异常退出、Unknown 恢复与 SQLite 触发器制造的提交失败。TCG 验证不代表 KVM、其他宿主平台、Guest 就绪或完整安全沙箱验证。
