# 真实 CLI 传输兼容与长会话设计

状态：长会话、正常取消和固定CLI必要HTTP兼容均已实现并完成相关普通/race验证；实现复审待主Agent在原session代跑。固定版本为Codex 0.114.0/gpt-5.4、Claude Code 2.1.81/claude-sonnet-4-6。本worktree假TLS验证不等于真实CLI经VM/Gateway或真实供应商验收。

## 来源与授权范围

数据流沿用Guest回环HTTP → SAPX → Host Gateway → Host显式配置的HTTPS origin。Guest不提供上游地址或可信身份，Authorization、X-API-Key、Cookie、Host、Forwarded及连接级头不会复制给上游，认证仅由Host CredentialStore注入。没有新增依赖、Boot字段或公共protocol Go DTO，也不开放CONNECT、任意URL、重定向、WebSocket或任意查询。

2026-10-25实际读取的依据：

- `/tmp/safeagent-realcli-agents/docs/real-agent-packages-implementation.md` 的“向主 Agent 报告的 Host Gateway 依赖”章节；该Agent已固定两种CLI的生产包与配置。
- `/tmp/safeagent-realcli-agents/artifacts/acceptance-claude/console.log:289`：实际Claude Code 2.1.81发送 `POST /v1/messages?beta=true`，模型 `claude-sonnet-4-6`，`Anthropic-Version: 2023-06-01`，`Anthropic-Beta: claude-code-20250219,interleaved-thinking-2025-05-14,prompt-caching-scope-2026-01-05,effort-2025-11-24`。
- `/tmp/safeagent-realcli-agents/artifacts/acceptance-codex/console.log:289`：实际Codex 0.114.0发送 `POST /v1/responses`，模型 `gpt-5.4`。
- [Claude官方Gateway协议](https://code.claude.com/docs/en/llm-gateway-protocol)：明确Messages推理请求带 `?beta=true`；要求原样转发 `anthropic-version` 和 `anthropic-beta`，不要硬编码beta值集合；错误body保持不变，`x-should-retry`原样传递，`retry-after`控制重试；`count_tokens`是可选端点。
- [OpenAI Responses create](https://developers.openai.com/api/reference/resources/responses/methods/create)和[Anthropic Messages create](https://platform.claude.com/docs/en/api/messages/create)：基础POST及SSE。OpenAI页面最初抓取超时，未据此推断CLI额外头或路由；本次Responses目标由固定CLI捕获确认。

当前worktree在 `artifacts/implementation-session/cli-evidence/` 保存两份console摘录、官方HTML及来源记录，外部素材只读。CLI Agent的localhost假上游检查没有经过Host Gateway；本worktree测试使用正式Bridge/Gateway及假TLS上游，但没有运行真实CLI或VM，不能拼接成一次完整验收。

## Host profile API

```go
func gateway.CLIService(agent, origin, credentialRef string, allowedNetworks []netip.Prefix) (gateway.Service, error)
```

仅接受 `codex` 和 `claude`，未知agent返回 `gateway.ErrInvalidConfig`。Ref固定 `model`；Origin、CredentialRef、AllowedNetworks由可信Host显式传入，没有默认源站、凭证或全网授权。主入口可传 `session-key` 作为CredentialRef；这只是引用，不把secret写入profile或Guest。返回Service再交 `gateway.New`，复用既有HTTPS origin、凭证引用、网络和资源配置校验。

| agent | 固定POST目标 | Host认证 |
|---|---|---|
| codex | `/v1/responses` | Bearer |
| claude | `/v1/messages`、`/v1/messages?beta=true` | X-API-Key |

复用现有 `Service.Paths` 表达精确请求目标，不新增通用query配置。Host仅允许这一条含查询的字面目标 `/v1/messages?beta=true`；其余仍须是规范无查询路径。运行时先匹配授权map，成功后才将目标拆为URL.Path和RawQuery，避免把问号编码进路径。大小写、重复参数、额外参数、空查询、编码别名、片段、路径清理变体均不授权。只登记 `/v1/messages` 的旧服务不会自动取得beta查询权限。

保留原Content-Type、Accept、Anthropic-Version允许列表；仅对X-API-Key认证的Messages目标转发Anthropic-Beta，并保留多个值及原内容，不把当前版本beta集合硬编码为允许值。响应保留Content-Type、Retry-After、X-Request-Id，并仅对该Messages目标增加X-Should-Retry。原上游错误status/body直接交付，开始响应后的流错误不补成功EOF。

没有观察到 `count_tokens`，因此不支持；Responses compact、模型发现、账户、遥测、Bedrock/Vertex/AWS登录及账户计划用量显示也不在本轮范围，不开放对应路由或通用响应头前缀。新增路径、功能和CLI版本须先提供证据再评审。

## 控制长会话

旧Host用 `next >= MaxRequests` 限制全部请求，Guest同时累计全部控制请求，周期GetState Probe会耗尽4096容量。主Agent已追加授权 `internal/guest/connection.go` 及专用测试和长会话契约，不修改prepare/runner或Boot。

Host将序号与容量分离：

- 非GetState请求保留全部历史，上限4096；mutating operation的operationID、最终结果及256操作限制完全不动。
- GetState有独立 `protocol.MaxPending=16` 占额，含取消后尚未完整验证响应者。调用/写资格前共锁预留，满额立即RESOURCE_EXHAUSTED且不分配序号。未登记发送便取消或失败时回收预留；登记发送后只有合法成功/错误响应完整验证或连接终止才释放。现有调用slots仍在调用返回时释放，不能代替独立账本上限。
- GetState完整验证后删除pending；后续重复响应因未知ID失效连接。首个迟到响应仍严格校验epoch、connectionID、方法及结果，不能在调用方取消时删除。
- uint64 RequestID严格递增，不复用，耗尽明确拒绝。并发响应可乱序完成，不增加错误的全局响应序号限制。Probe失败直接断开、不追加Cancel的策略保持。

Guest只将非GetState计入历史请求数，保留last的严格递增检查、最多16个业务任务和32项有界响应队列。旧Guest镜像仍有全部请求4096限制，必须重新构建并与新Host配套装配，不自动协商或重连。对应契约见guest-protocol-v0.1.md与guest-proxy.md。

## Run取消与资源

proxy.Run仍为绑定撤销、控制连接关闭与VM cleanup的唯一所有者。调用方主动取消（context.Canceled）时，仅剔除传输过程的context.Canceled、net.ErrClosed、io.ErrClosedPipe、io.EOF；组合错误逐项保留其他故障。截止时间到期不作为正常结束。cleanup使用独立10秒context，错误在取消归一化之后合并；cleanup返回的任何错误（包括context.Canceled/net.ErrClosed）均保留。正常主动取消且清理成功返回nil，调用方检查非nil结果，不按errors.Is(context.Canceled)整包忽略。

SSE及二进制保持原字节流，逐帧交付/Flush；保留上传、空闲、帧、写预算及有界内存。Web和命令行主入口不再设置单次请求总时限，Gateway在连续5分钟没有字节进展时结束请求，有数据进展时刷新空闲计时。上传、写入及首字节后的完整物理帧预算仍为2秒，Guest等待响应首字节的期限由Host的Gateway空闲判定管理。Gateway的TotalTimeout零值表示仅使用空闲超时，正值仍执行显式总时限。已完整上传请求发生上游失败或空闲超时，Host发送Abort后接纳下一请求；Guest在响应头之前返回502，流式响应中则中断当前HTTP响应，均保留代理通道供CLI重试。授权失效、撤销、协议错误和不完整物理帧仍终止通道；HTTP客户端中途取消也仍需关闭通道以隔离迟到帧，不自动重连。并发保持一个在途、额外请求立即503，不增加排队或透明重试。

## 验证与review

两种profile经真实Bridge/Host Gateway到本机假TLS：精确目标、请求body、认证剥离/Host注入、beta原样转发及供应商隔离、Guest响应无secret、首块先于尾块、二进制、429 status/body/Retry-After/X-Should-Retry、正常复用、取消。另测恶意query/方法/绝对URL/跨profile路由在凭证解析和拨号前拒绝；旧重定向/DNS/TLS及单并发503回归保留。

真实Host/Guest控制连接连续4128次Probe及Start/Prepare operation重放一致；取消占额/迟到错误响应回收/关闭回收、活动满额立即拒绝、发送前取消回收、序号耗尽、Guest历史满额与重复请求号均有专用测试。相关包普通、race、vet与中文函数/字段注释AST检查分别记录于本worktree artifacts。

设计review由主Agent代跑，原session为 `01a17116-acda-7a53-9a4d-18f4f657d317`，P1独立只读容量要求已采用。此前实现复审在本地因只读文件系统初始化失败；按本轮指令不重复调用受限review，更新同一session的prompt交主Agent代跑。尚未取得最终实现review结论。Fake TLS、真实VM、真实供应商、固定CLI端到端及跨平台分别报告，不能相互替代。

## round2生命周期错误修复

原session round2指出P2：Probe失败只cancel，控制连接非法响应只发布Done，随后外部cancel会剔除proxy关闭错误并误报成功。现已在具体rpc.Client增加并发安全的 `Failure() error`（健康nil、Done后首次原因固定），不改共享guest.Client。Run将可信端口打开保留在生产装配，内部run统一执行原生命周期，便于内存设备确定性验证。Probe错误由唯一探测任务保存；统一收尾等待该任务退出后读取，并在RPC主动Close前后取得首次失败，避免快照与Close间遗漏协议失败或将本地主动关闭当故障。合并主循环、父deadline、控制首次原因、Probe结果和独立cleanup错误。

仅父context主动取消时逐项剔除控制原因中的context.Canceled及Probe的取消/断线包装；协议、业务、deadline和其组合始终保留。cleanup在筛选后合并，不被筛选。确定性测试在Ready回调闸门暂停主任务，真实Probe或非法控制响应触发Done后才外部cancel再放行，覆盖RESOURCE_EXHAUSTED、协议、deadline、正常空闲/活动Probe取消、父deadline及三类cleanup错误；另有首次失败并发读取与含context.Canceled组合保真测试。真实VM/CLI验收仍由主Agent进行。

## round3 Probe同步写取消修复

round3唯一P2已修复：Probe在调用开始注册context.AfterFunc，取消时通过既有fail首次原因边界固定ctx.Err并立即关闭独占控制连接，打断sendRequest同步写；不再等待原两秒写deadline。Probe返回前停止或等待取消回调完成，Run仍等待探测任务退出后收集原因并执行唯一撤销/cleanup。已固定的真实协议、业务、deadline或网络timeout不会被后续取消覆盖；正常主动取消导致的传输退出仍按叶子筛选，含Canceled的组合不整包忽略，cleanup错误仍在筛选后合并。未修改共享guest.Client接口、预算或Probe失败断开策略。

新增确定性生命周期测试仅观察真实net.Pipe的Write入口，对端完全不读Probe请求：主动cancel后一秒内完成，cleanup成功返回nil且恰调用一次、绑定撤销；另一路等待真实写timeout触发Done后紧邻外部cancel，仍保留timeout。既有RESOURCE_EXHAUSTED/协议/deadline、非法控制响应紧邻cancel、正常活动/空闲取消与cleanup错误回归保留。

本轮相关六包普通与race全部通过，go vet、中文函数/字段注释AST和diff检查通过。证据为artifacts/implementation-session/probe-write-targeted.log、probe-write-test.log、probe-write-race.log、probe-write-vet.log、probe-write-comments.log。Bridge模型等待修复保留并已获round3认可。原session 01a17116-acda-7a53-9a4d-18f4f657d317不变，更新prompt交主Agent代跑复审；本轮未运行受限review、未提交，真实VM/CLI/供应商验收仍另行报告。
