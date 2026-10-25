# PTY 进程组 proc 读取竞态修复

## 根因与初始证据

基线：`82a97d4d87c1fa79c3e179bf731d4cda03d7bc87`，工作目录 `/tmp/safeagent-realcli-runner`。
指定 transport-test.log 只有 RPC INTERNAL，不能直接证明底层 errno；指定 lifecycle-test.log 不存在，lifecycle-final-test.log 显示随后测试通过。

原版 groupLocked 在枚举 /proc 后逐项 os.ReadFile，只忽略 ENOENT。新增本地进程 churn 诊断：8 个并发 worker，每个执行 500 个 /bin/true，同时查询一个不可能存在的 PGID，迫使查询遍历所有进程，避免提前找到存活成员。原版连续 10 轮均捕获 `*fs.PathError: read /proc/<pid>/stat: no such process`，日志保存在 `artifacts/implementation-session/baseline-churn.log`。最初对存活组进行扫描的诊断没有复现，因为可能提前找到成员，此记录保存在 initial-churn.log。

进一步使用 stdin 管道阻塞 shell，先打开其 /proc/PID/stat，再关闭管道并 Wait 回收，读取原 inode 稳定返回 ESRCH。最终回归通过 /proc/self/fd/N 引用同一 inode，先验证 os.ReadFile 确实返回 ESRCH，再调用生产读取函数验证识别结果。没有 sleep、重试、全局测试钩子或日志泄露。

这证明组扫描存在真实的 open/read 资源边界竞态；缺少原失败时的 errno，不能声称追溯证明 transport 那一次失败的唯一原因。当前复现与其 INTERNAL 路径一致。未发现空/短 stat 是退出竞态的证据，因此仍明确返回解析错误。

## 最小实现

仅修改 runner：将原有单个 stat 的读取与成员判断提取为 processInGroup，新增识别 ESRCH，沿用 ENOENT 处理。权限、目录读取、空/短 stat 等其他错误继续传回 groupLocked，保持 GroupStateUnknown；不把无法确认的组视为空。未改变 Start、信号、唯一 Wait、leader 僵尸保留和 PGID 防复用机制，无共享接口和依赖变更。

新增三个测试：全量 proc 扫描与短命进程并发、已回收 proc inode 的确定性 ESRCH、格式错误和真实资源读取错误传播。压力测试使用每轮新 Process 观察不存在的 PGID，保证不命中 groupEmpty 缓存。未修改 RPC 用例，未给其增加 sleep 或重试。

## 验证与限制

使用指定 activate.sh，GOCACHE=/tmp/safeagent-runner-proc-cache。所有验证只使用本地 Linux 进程；非 fake VM 测试，未运行真实 VM、跨平台测试、凭据或外部 API。详细结果与 reviewer 状态见 `artifacts/implementation-session/status.md`。运行日志不提交。
