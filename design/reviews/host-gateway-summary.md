# Host Gateway 设计与实现评审

日期：2026-10-24。目录：/home/yedong/workspace/sandbox。只使用 codexx，xapi/gpt-6.1-sol、medium。

Gateway 与 Workspace 是独立任务，建立专用 session 01a16d5c-7f47-7951-8b8b-c6b4d784eba6，设计、修正及代码复审复用该 ID；Workspace 原 session 保留。本任务不调用 Claude、不创建子 Agent、不执行 Git 写操作。

## 编码前验证与设计

范围为 design/host-gateway.md、scripts/spikes/host-gateway 和 docs/host-gateway-spike-2026-10-24.md。先完成最小设计和本机 HTTP/TLS 探针，再进入生产模块。

设计首轮审查发现两项有效问题：

1. P1：HTTP 服务端 r.Body.Close 无法保证解除正在阻塞的 Read，原型未验证部分上传停滞。
2. P2：仅 IP URL 的 TLS 测试未验证原域名 SNI 与按 IP 拨号的组合。

修正为 HTTP 适配器设置读取 deadline 后关闭输入，并等待取消回调退出；新增原始 TCP 部分上传的总超时及撤销、域名证书与 SNI、错误主机名/不可信证书拒绝和停滞响应总超时。完整探针 race 通过。原 session 设计复审确认无剩余阻塞问题，可以实现正式核心。

原型历史失败、平台与观测数据见实测文档；不将原型状态机或网络结果冒充正式授权表与配额验收。

## 正式实现与代码评审

实现 internal/host/gateway：Host 内存绑定、替换/撤销、固定 POST 路由、Host 认证、有界请求与流式响应、全局在途配额、总/空闲超时、DNS/IP/TLS 策略及资源释放。internal/testkit/fakecredential 只提供显式假值和未知引用错误，不读取环境或 Vault。

第一轮代码评审的有效问题：空闲判定与提交终止分离，旧回调可与新进展竞争。已将两者放进同一锁内，旧回调在终止前复核 lastProgress；正式流进展测试补充显式旧回调模拟。

另两项建议经主 Agent 判断不采纳：

- crypto/rand.Read 错误分支：Go 1.27.3 实际源码保证永不返回错误，随机源底层失败不可恢复终止进程；不添加不可达分支。
- nil 依赖兜底：CredentialStore/Resolver 是 Host 可信代码的非 nil 装配前提，不由 Guest 或配置文件创建。已在 New 和设计中明确契约，不添加反射检查、自动默认值或替代实现。

原 session 第二轮代码复审通过：有效 Idle 问题已修复；reviewer 核查 Go 1.27.3 实际源码和 Host 装配契约后确认其余两项无需增加防御分支，首轮三项意见均关闭。未发现新的具体安全或正确性问题，也未提出应继续删除的冗余防御。

## 验证与范围

主 Agent 在本机允许 TCP socket 的环境执行以下命令，均通过：

```sh
source artifacts/toolchain/activate.sh
go test -timeout 60s ./...
go vet ./...
CGO_ENABLED=1 go test -race -timeout 60s ./...
```

正式测试覆盖身份替换与拒绝、授权注册/撤销竞态、完整响应生命周期占额、请求/响应精确上限、阻塞输入、总/空闲计时、旧回调、资源唯一释放、凭证/头隔离、429/Retry-After、域名 SNI、DNS 混合未授权地址、TLS 证书拒绝与重定向拒绝。

独立探针模块另运行 race，输出在 artifacts/host-gateway-spike/race-2.txt。Reviewer 只读审查，不将主 Agent 执行的测试记为 reviewer 自行验证。

尚未接入 proxy 线协议、Guest-local Bridge、生命周期调度或真实 Vault；未访问真实模型/MCP，未验证 VM 端到端或其他平台。生产依赖必须遵守可取消和错误不包含秘密的契约，设备输入 Close 解除阻塞的行为仍需接入时实测。
