# 真实 Guest 实现与验收记录

当前工作在 `feat/guest-agent` worktree；实现复用公共 protocol、guest.Client/Terminal、VM Manager 生命周期及 guestcontract。尚未提交。

## 实现范围

- `internal/host/guest/rpc`：真实控制请求/响应关联、取消与迟到账本、唯一读者、Hello 语义校验和 GetState、独立退出等待、终端授权及有界输出。
- `internal/guest`：boot 内操作结果与准备记录、唯一启动资格预留、固定失败、历史状态、Handler、绑定/解绑及 PTY 有界输出。
- `internal/guest/runner`：Linux 非 root PTY、会话领头进程、唯一退出观察、WNOWAIT 保留 PGID 至组空、显式 TERM/KILL、输出和进程生命周期分离。
- `internal/guest/device`、`cmd/guest-agent`：可关闭的非阻塞设备连接、固定镜像登记校验、同 boot 不可重入标记。不支持丢状态后的服务重启恢复。
- QEMU 具体驱动增加独立 virtserialport 及可信端点交付，绑定真实 VMInstance；未修改 VMBackend、Driver、Process 或 guest.Client 契约。`protocol.DecodeRequestEnvelope` 集中补足握手/身份先于方法参数检查的需求。`x/sys/unix` 是已有依赖，改为直接引用，没有引入新包版本。

## 已运行

- 协议 Schema、45 个负载正反例和 2 个指纹向量通过。
- Linux 本机 `go test ./...`、`go vet ./...`、`CGO_ENABLED=1 go test -race ./...` 通过，Fake 仍单独通过。
- 宿主真实 RPC/Handler/PTY 跑共享 guestcontract；补充 challenge、迟到/重复/未知响应、4096 请求账本、256 操作容量、固定 exec 失败、慢输出 Stop、排空超时和残留进程组测试。
- 新 Guest 镜像真实 Linux/amd64 microvm/TCG 闭环普通版和 race Guest 均通过；PGID 加强及 review 修复后的 race 镜像又通过闭环；真实 QEMU 工厂已通过全部 6 个共享 guestcontract 场景（每场景独立 VM）。端点短路径修复后既有真实 VM 生命周期 suite 也通过。命令见 `scripts/guest-agent/README.md`，最终重建后完整重跑证据为 `artifacts/guest-agent/final/results.txt`（57.874秒，全部通过）；此前普通和race镜像证据在 `artifacts/guest-agent/plain`、`race`、`race-final`、`review-final`。基础盘 SHA-256 为 `4e16706e345f0bacbae79893a423ef6c8c1b522f6f078caceaee30f8b07216fc`，前后不变，自有 QEMU 已回收。
- darwin/amd64 Host RPC 测试交叉编译成功；未在 macOS 运行。Windows 交叉编译失败于既有 `internal/host/vm/store/sqlite.go` 的 Unix syscall 依赖，未算通过。

## 仍需完成的验收

review 前三轮共提出 12 项有效问题，已修复并补回归：端点退出取消、Attach初始化、PTY取消回调、停止原因不降级、CloseWrite取消与部分Fin、序号/计数分离、Close总预算、可取消Resize及授权原子性、Stop等待退出提交、历史端点映射回收。第四轮复审未发现剩余具体有效 bug；记录见本 worktree `artifacts/review-sessions/codexx-implementation-4.txt` 及第五轮增量复审；最终全仓协议校验、test、vet、race全部通过。

真实旧身份、半帧和超长帧探针已运行：首次错误假设设备关闭会立即产生Host socket EOF，三项均超时失败，原始日志保留。virtio关闭不保证Host EOF，采用Host查询超时后显式关闭逻辑连接、回收VM的策略后，三项通过；不能把这解释为Host自动识别Guest设备关闭。补充真实终端单独断线、Bound后无效Bind、持续flood慢读Stop及排空不完整三项已通过（`artifacts/guest-agent/terminal-faults/results.txt`）。真实取消提交竞态及全部容量仍未逐项完成；同 boot 控制重连明确不支持，需新 VM epoch。宿主增加64 session、32FIFO入队预算、创建前/创建后取消闸门、Stop退出提交闸门、旧token拒绝和Bind超时释放。入口同boot重启拒绝已在独立私有目录通过；真实guestd异常重启、16请求并发上限、全部资源回收边界和上述场景的真实设备版本仍需专项覆盖。旧 spike 不替代新增实现验证。KVM、其他平台运行、真实产品 Agent、Manifest/Import/Gateway 均未验证。

实现评审复用本 worktree 原方案 codexx session，模型 xapi/gpt-6.1-sol/medium；原始记录位于 `artifacts/review-sessions/`，五轮实现评审完成，首三轮12项有效问题全部修复，第四轮及第五轮增量复审均无剩余具体有效bug。方案与实现沿用同一session；review只读，没有Git写操作。

## 冗余与可读性专项整理

复用原 codexx session 完成三轮只读专项 review：首轮7项、第二轮2项建议均处理，第三轮未发现剩余值得处理的具体冗余、防御性分支或难读问题，也未发现简化回归。原始证据位于 `artifacts/review-sessions/codexx-cleanup-{1,2,3}.txt`。

- 删除 context value 隐藏清理策略，私有调用显式选择取消行为；删除 Terminal.closeDone 重复完成通道、channelRun.instance 未使用副本、process 构造已保证的非空兜底和 Accept 交付前无意义加锁。
- 请求发送、PTY单帧输入各自统一释放资格；Start创建与登记、内核退出读取、Fin发送分成具体步骤，降低嵌套；Runner启动失败统一关闭PTY，移除入口无依据的100ms退出延时，输出容量复用公共常量。
- 保留身份/epoch、资源边界、取消回调完成等待、Exit提交、PGID保护及部分Fin失败的必要检查。

第一轮真实重跑中，正常闭环、共享契约和终端专项通过，旧身份场景在Hello阶段超时（尚未故障注入）；失败证据保留在 `artifacts/guest-agent/cleanup/results.txt`，不记为通过。最终重建race Guest后，真实TCG正常闭环、全部6项共享契约、3项control故障和3项terminal故障全部通过（75.906秒），证据位于 `artifacts/guest-agent/cleanup-final/results.txt`；每台自有VM已回收且基础盘摘要未变。全仓协议校验、test、vet、race及真实VM生命周期race通过。本轮未重跑跨平台编译，沿用前述边界。
