# Claude CLI 评审原文

实际返回的 modelUsage：deepseek-flash[1m]。这是 CLI 当前配置的模型，不代表 Anthropic Claude。

## 评审结论

已按要求只读检查 `AGENTS.md`、两个 `go.mod`、`Makefile`、`.gitignore`，以及 `internal/host/vm`、`internal/testkit/fakevm`、`internal/testkit/vmcontract` 下全部 9 个文件（含未跟踪新增文件）。未修改、未暂存、未调用任何 Agent/CLI/模型，未读取凭据；测试结果沿用主 Agent 已执行的 `go test`/`go vet`/`-race`，本次结论均为静态评审。

整体质量较高：并发入口统一经 `fakevm.Backend.lock` 串行化（`internal/testkit/fakevm/backend.go:112-135`），`r.exit` 在 `mu` 下写入、`close(done)` 前完成，`Wait` 收到信号后读取（`backend.go:306-310, 336-352`）不存在数据竞争；`Inspect` 对 `Exit` 做值拷贝（`backend.go:246-249`）；`Stop` 失败/取消回滚为 `Running`（`backend.go:286-295`）；`Destroy` 注入失败后资源与历史退出结果都保留、可重试（`backend.go:361-386`，测试 `backend_test.go:166-176`）；旧 epoch 的 `Stop`/`Exit` 均被 `item.instance != instance` 拦截（`backend.go:269-272, 320-322`）。以下是 5 个真实可操作问题。

### 1. bug：`Destroy` 对“从未启动”资源的契约与 fake/共用 suite 直接冲突

- `internal/host/vm/backend.go:27-29` 写的是“只删除**已停止**的私有资源”，`internal/host/vm/README.md:15` 同样表述。
- 但 `internal/testkit/fakevm/backend.go:367` 允许 `StateCreated`，并且 `internal/testkit/vmcontract/contract.go:307-314`（`DestroyUnstarted`）**要求**对未启动资源 `Destroy` 成功。
- 后果：下一个 worktree 按接口注释实现 QEMU 后端（`Created` 返回 `ErrInvalidState`）会直接挂在共用 suite 上；反之按 suite 实现又与文档不符。`backend.go:28` 的“资源不存在时成功”也没说明 `Created` 是否算可删。
- 可操作：三处统一为“`Created` 或 `Stopped` 可销毁，其余返回 `ErrInvalidState`”（建议改文档，因为入参校验/资源清理本来就该覆盖 `Created`），或把 suite 用例改为期望 `ErrInvalidState`。

### 2. bug（契约缺口）：已存在资源上的“未知 epoch”返回值未定义，fake 内部也不一致

- `backend.go:19-25` 只规定了“重启后停止**旧** epoch 返回 `ErrStaleInstance`”和 `Wait` 结果跨重启保留，没有规定“从未签发过的 epoch”作用于**存在**的 ref 时应返回什么；`contract.go:337-352` 只覆盖了 ref 不存在的情况。
- fake 对同一输入给出不同答案：`Stop` 返回 `ErrStaleInstance`（`fakevm/backend.go:269-272`），`Wait` 返回 `ErrNotFound`（`fakevm/backend.go:343-345`）。真实后端很可能一个选 `ErrStaleInstance`、一个选 `ErrNotFound`，而共用 suite 无法发现。
- 可操作：在 `backend.go` 明确“ref 存在但 epoch 不属于任何已记录运行”时的返回值（建议 `Stop`/`Wait` 统一为 `ErrStaleInstance`，`ErrNotFound` 只用于 ref 不存在），并在 `vmcontract.Run` 增加对应用例。

### 3. 覆盖建议：契约要求的 `GracePeriod > 0` 校验完全没有进入共用 suite

- `internal/host/vm/types.go:152-167` 定义并要求正数宽限期，fake 在 `Stop` 首行调用 `options.Validate()`（`fakevm/backend.go:255`）。
- 但 `vmcontract.Run` 全程只用合法的 `stopOptions()`（`contract.go:61-64`），`types_test.go:12` 也只测了 `VMConfig.Validate`。真实后端漏掉 `StopOptions.Validate` 仍能通过全部共用测试。
- 可操作：在 `Run` 增加一个子测试：对 `Running` 实例传 `GracePeriod: 0`，断言返回 `ErrInvalidConfig`、`Inspect` 仍为 `StateRunning`、随后用合法参数可正常停止（同时验证非法参数不产生副作用）。

### 4. 覆盖建议：`Stop` 进行中被取消、以及 `Exit` 的旧 epoch 分支缺少直接测试

- `fakevm/backend.go:283-295` 的“`StateStopping` → 取消/失败 → 恢复 `Running`”目前只由 `FailNext` 注入错误走到（`backend_test.go:145-183`）；`gate.wait` 的取消路径（`gate.go:33-40`）与 `before` 的 ctx 检查（`backend.go:147-149`）没有被 `gate + cancel` 直接触发。共用 suite 的取消用例只覆盖“调用前已取消”（`contract.go:316-335`）。
- 另外 `Exit` 的旧 epoch 拦截（`fakevm/backend.go:320-322`）无任何测试：重启后 `Exit(first)` 应返回 `ErrStaleInstance` 且不影响新运行。
- 可操作：在 `backend_test.go` 增加两个用例：(a) `SetGate(OpStop)` → 等待 `Entered()` → `cancel()` → 断言 `StateRunning`、`Stats().Stopped == 0`、`Wait` 仍阻塞，`Release()` 后重试停止成功；(b) 重启后 `Exit(first, ...)` 返回 `ErrStaleInstance`，`Inspect` 的 `Instance`/`State` 不被旧实例改写。

### 5. 风格/契约：`StateStarting`、`StateStopping` 的可观察性没有定义

- `types.go:130-137` 定义了这两个状态，fake 会在操作进行中对外暴露（`backend.go:216`、`backend.go:283`，测试 `backend_test.go:103-106` 明确断言 `StateStarting`），但 `backend.go:16-17` 的 `Inspect` 注释和 `README.md:12` 都没说明调用方是否可能观察到它们、观察到时应如何解释，`vmcontract` 也从未约束。
- 后果：真实 QEMU 后端即使把 `Created → Running`、`Running → Stopped` 做成不可观察的原子跳变也“符合”文档，与 Host 依据 fake 行为写出的判断逻辑产生偏差；反之要求可观察又缺少共用用例。
- 可操作：在 `Inspect` 注释中明确二者是允许的瞬时观察值（且不得据此推断 `Start`/`Stop` 已完成），并同步 `README`；若决定不暴露，则从 fake 中移除并在类型注释里写明只有 `Created/Running/Stopped` 可见。

补充：未发现其他并发或资源泄漏级问题；`locks` 中按 ID 保留的 channel 在 `Destroy` 后不清理（`fakevm/backend.go:55, 377-383`）属测试专属的小内存增长，真实后端实现时注意不要照搬为无界 map 即可。
