# Host 内存凭证 Store

`internal/host/credential.New()` 创建宿主会话使用的内存 Store，实现 `gateway.CredentialStore`。不读取文件、环境变量或凭证服务，不落盘，不提供枚举或导出接口，无新增依赖。

宿主 CLI 负责从用户输入取得 secret 并调用 `Put(ctx, ref, secret)`，将此 Store 注入 Host Gateway；CLI 读取部分由主 Agent 实现。Host 服务配置使用同一 `CredentialRef`，Gateway 在转发时调用 `Resolve` 并注入真实认证。VM 仅得到代理地址和占位认证；真实密钥不得进入 VM、manifest、命令行或日志。本模块不实现 CLI、VM 配置或 Gateway 接线，接入时需由宿主会话持有 Store 并在结束时 Close。

引用是区分大小写的 Host 本地标识：首字符为 ASCII 字母或数字，其余仅允许 ASCII 字母、数字、`.`、`_`、`-`；不自动修剪或规范化。所有引用入口使用同一规则。secret 不得为空或含 CR、LF、NUL，不自动修改用户输入。无效输入返回固定错误，不包含输入内容。

`Put` 原子覆盖同一引用，供用户显式更换密钥。`Resolve` 只返回指定引用，未知引用返回 `ErrNotFound`，没有默认凭证或回退。`Delete` 幂等删除。`Close` 幂等关闭并释放存储所持的凭证表；后续有效调用返回 `ErrClosed`。已取消上下文优先返回上下文错误，无效输入优先于关闭状态返回输入错误。

所有状态访问由互斥锁串行化，操作前和获得锁后检查上下文。等待临界区的调用会在获得锁时观察取消；临界区仅有内存表访问，不进行 I/O。取消与提交并发时，以锁内取消检查为界，提交后的取消不撤回操作。关闭不撤回已经返回给 Gateway 的字符串，调用方负责结束在途请求及释放其持有引用。Go 字符串可能被运行时或调用方复制，本实现不承诺物理擦除。

Store 必须通过 New 构造；Store 值副本共享状态和关闭生命周期。`String` 和 `GoString` 隐藏内容，覆盖指针和值的常见 fmt 格式。模块不记录日志；调用方仍不得记录 Resolve 返回值、用户输入、或使用反射/unsafe 绕过封装。固定错误支持 `errors.Is`，不包装引用或密钥。

测试仅使用显式假密钥，覆盖生命周期、覆盖、删除、输入边界、取消、格式化和并发关闭；通过编译期断言验证 Gateway 接口。真实 VM、宿主 CLI 端到端接入和跨平台验证需单独执行，单元测试不替代这些验证。

## 本次验证记录

已 source 指定 Go 工具链。默认构建缓存因只读目录失败，改用可写的 `GOCACHE=/tmp/safeagent-credential-go-cache` 后，`go test ./internal/host/credential ./internal/host/gateway` 和对应 `go test -race` 均通过。未执行真实 VM、CLI 端到端或跨平台验证。

本 worktree 首次 codexx 因文件系统只读启动失败；主 Agent 已代跑 read-only、xapi/gpt-6.1-sol/medium 评审并将 session ID 保存到 `artifacts/review-sessions/codexx.id`。评审未发现实现缺陷，唯一 P3 是取消测试缺少首次上下文检查的同步信号。现已修复：测试上下文先读取并保留首次 Err 结果，再通知测试线程；测试线程收到信号后才取消并释放 Store 锁，确保取消由锁后检查观察。修复后普通和 race 测试均重新通过。等待主 Agent 复用原 session 复审，本次未新建或调用 reviewer。
