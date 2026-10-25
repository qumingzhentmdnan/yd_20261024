> 最终整合状态（2026-10-25）：本文保留各轮实施过程。原session的round4及主工作区整合复审均通过；新增回归普通/race/vet通过。真实TCG生产PTY两轮及独立副本Host假TLS链路、两种CLI经七牛自定义gpt-6.1-sol真实对话均通过，详见 [宿主入口](real-cli-host-implementation.md)。KVM、跨平台和任意供应商兼容性未据此验收。

# 真实 CLI 传输实施记录

2026-10-25：已实现长会话控制账本、proxy.Run 正常取消及已捕获固定CLI的必要HTTP兼容。导出profile可供Host入口接入，完整VM/CLI集成由主Agent验证。设计见 design/real-cli-transport.md，实时交接见 artifacts/implementation-session/status.md。

## 修改

Host RPC 将序号、非只读历史容量和未完成只读容量分开。非 GetState 请求保留全部历史记录，上限4096；GetState 使用独立16个槽位，包含调用方已经取消但尚未收到完整合法响应的请求。等待调用/写资格前先原子预留，因此16个活动调用也能让第17个查询立即返回 RESOURCE_EXHAUSTED。尚未登记发送就取消/失败时释放预留；登记后仅完整验证成功或错误响应才释放，连接终止统一释放。GetState 响应删除后，重复响应按未知ID失效连接，迟到首个响应仍严格验证身份/方法/结果。uint64序号耗尽明确拒绝，不复用序号。Probe失败即断开不变。

Guest connection.go 已经主 Agent 追加授权，将 GetState 排除于4096历史计数，保留严格递增last、MaxPending业务任务和原有32项有界响应队列。未改 prepare/runner、Boot、公共protocol Go DTO或operation实现。长会话契约对应更新，旧Guest镜像必须重新构建，不能假定与新Host自动兼容。

proxy.Run 保持唯一撤销/清理所有权。调用方主动cancel时，逐项剔除传输的 context.Canceled、net.ErrClosed、io.ErrClosedPipe、io.EOF；保留组合错误中的其他错误。deadline到期不作为正常结束。cleanup使用独立10秒预算，错误在归一化后合并，任何cleanup错误都保留。正常主动取消且清理成功返回nil，调用方必须报告其他非nil结果，不能按errors.Is(context.Canceled)整包忽略。真实VM启动取消测试的预期同步更新，但本轮未运行真实VM。

## CLI兼容与来源

已读取CLI Agent实施记录的Host Gateway依赖章节及acceptance-claude/codex两份console.log:289。Claude2.1.81实际POST `/v1/messages?beta=true`、模型claude-sonnet-4-6、Anthropic-Version2023-06-01，beta集合为claude-code-20250219、interleaved-thinking-2025-05-14、prompt-caching-scope-2026-01-05、effort-2025-11-24；Codex0.114.0实际POST `/v1/responses`、模型gpt-5.4。证据摘录和实际抓取的[Claude官方Gateway协议](https://code.claude.com/docs/en/llm-gateway-protocol)在artifacts/implementation-session/cli-evidence/，完整来源见设计文档。

导出 `gateway.CLIService(agent, origin, credentialRef string, allowedNetworks []netip.Prefix) (gateway.Service, error)`。仅codex/claude，Ref固定model，CredentialRef由Host传入session-key等显式引用，Origin和网段无默认；未知agent返回ErrInvalidConfig，Service交New统一校验。路径/认证由profile固定：codex Responses+Bearer，claude Messages（含精确beta=true变体）+X-API-Key。原Service.Paths复用为精确请求目标，只有字面`/v1/messages?beta=true`允许带查询；请求授权后再拆URL.Path/RawQuery，不开放任意query。没有观察到count_tokens，不新增该路由。

Guest认证仍剥离，Host最后注入。仅APIKey认证的Messages目标增加Anthropic-Beta透传，保留多个原值，不硬编码beta集合；Content-Type/Accept/Anthropic-Version保持。官方协议还明确错误响应的X-Should-Retry和Retry-After语义，因此仅Messages增加前者，保留后者及原status/body；不开放账户相关通用响应头前缀。流、取消、单在途503、有界背压与全部预算保持。

## 验证

相关 rpc/guest/proxy/bridge/proxywire/gateway 普通测试通过，race通过；go vet和函数/字段中文注释AST检查通过。CLI新增回归证据为cli-regression-test.log、cli-regression-race.log及cli-final-targeted-{test,race}.log；静态检查为cli-vet.log、cli-comments.log。本地证据位于 artifacts/implementation-session/：transport-final-test.log、transport-final-race.log、transport-vet.log、comments.log；补测run-cancel-test.log、run-cancel-race.log、guest-capacity-test.log、guest-capacity-race.log。

新增CLI profile精确授权/未知agent/跨供应商隔离/配置拒绝查询变体测试；两种profile经真实Bridge/Host Gateway到假TLS验证Host认证、beta多值原样透传、SSE首块在尾块放行前到达、二进制、429错误status/body/Retry-After/X-Should-Retry、Guest响应无secret、正常复用与取消。旧DNS/TLS/重定向/单并发503回归保留。

长会话新增用例包括真实Host/Guest连续4128次Probe以及Start/Prepare operation重放一致；16次取消后容量满额、迟到合法错误响应释放、关闭回收；16个活动查询立即拒绝第17个、发送前取消回收；uint64序号耗尽；Guest长会话重复请求号拒绝和历史满额仍可查询；Run取消错误筛选和真实退出路径保留cleanup错误。既有非只读4096容量、迟到/重复/未知响应等回归仍通过。

首轮普通回归中旧 TestRemainingGroupAfterLeaderExit 在 boundary_test.go:142 的 StartSession 返回 INTERNAL。原始失败保留在 transport-test.log；第二轮普通、最终普通和race未复现。未查明偶发根因，不将其隐藏或记为从未失败。Fake TLS旧回归与内存设备属于宿主验证，不能当真实供应商、真实CLI或VM验收。真实VM、真实供应商、CLI端到端和跨平台均未运行。

## Review 与依赖

主Agent代跑设计review，session为01a17116-acda-7a53-9a4d-18f4f657d317，已从codexx-events-parent.jsonl提取到codexx.id。首轮指出的只读独立容量要求已采用；Guest范围已获追加授权。实现复审按原session、xapi/gpt-6.1-sol/medium执行，但本地codexx初始化因只读文件系统失败，退出码1，未进入review。最新prompt、失败日志及模型/目录信息保存在artifacts/review-sessions/，需主Agent代跑并在该session处理后续复审；不能记为实现review通过。

CLI固定Codex0.114.0/gpt-5.4、Claude2.1.81/claude-sonnet-4-6。捕获已到并据此实现必要兼容；两份日志各只有一个请求，没有证据要求改变单在途503。未新增CONNECT、任意URL、重定向、WebSocket、count_tokens或任意查询。按主Agent指令，本轮不再尝试受限review，更新同一codexx.id对应prompt交主Agent代跑；实现review未通过前不宣称已审完。未新增依赖、未读取凭据、未执行Git写操作。

## round2 P2修复

主Agent在原session代跑round2，仅发现生命周期真实失败原因丢失P2，已采纳。新增具体rpc.Client.Failure()并发安全返回首次原因，健康nil、Done后固定，Close不覆盖；共享guest.Client不变。Run统一收尾在等待Probe任务退出后合并Probe返回、RPC首次原因、父deadline和主循环结果，再合并独立cleanup。读取RPC原因同时覆盖快照与Close间的失效竞态；正常主动取消只剔除各分支的取消及Probe断线包装，真实控制EOF/协议/业务/deadline均保留，不整包忽略含context.Canceled组合。

生产Run仅把同实例检查/OpenProxy收进私有open闭包，实际生命周期位于私有run，未增加公开工厂/网络或VM接口。内存通道测试复用全部生命周期，通过Ready闸门确保Probe容量/协议/deadline或非法控制响应发生在外部cancel之前，退出后验证原错误保留、绑定撤销且cleanup恰一次；正常空闲/活动Probe取消nil和cleanup普通/context.Canceled/net.ErrClosed错误保留均覆盖。Failure并发读取和后续失败/Close不覆盖首次原因有独立race测试。

本轮首个普通回归旧TestRemainingGroupAfterLeaderExit再次偶发INTERNAL，单独复跑通过，原始lifecycle-regression-test.log保留。新增Probe阶段断言首次发现旧测试辅助仅Read一次导致半帧写超时，已改共享完整帧读取并允许阶段真实网络timeout；lifecycle-rpc-check.log通过。最终结果见lifecycle-final-{test,race}.log，vet/注释为lifecycle-vet.log和lifecycle-comments.log。不调用受限review，复用原codexx.id更新prompt交主Agent代跑，未提交。

## 模型静默等待预算修复

主Agent发现Bridge将响应metadata及后续响应帧的首字节等待误限两秒。本轮仅将这两处proxywire.Read的deadline从proxywire.Deadline(total)改为已有total。proxywire.Read收到首字节后仍将剩余完整帧限制为min(total, now+Budget2秒)；写预算、上传期限、请求60秒硬总上限、取消和Gateway自身idle/total均不放宽。主入口显式配置独占模型服务Gateway IdleTimeout=TotalTimeout=60秒，允许该硬上限内模型静默思考，与不完整物理帧不同。

新增真实Bridge/Host Gateway+假TLS闸门测试：上游收到请求后保持三秒不响应，首块成功交付后再静默三秒，完整流仍成功。另测首字节后只发一个物理字节仍在两秒超时，以及首字节等待遵守调用方硬截止。旧取消/慢下游/上传/总上限相关回归保持；真实60秒VM场景本轮未运行，主Agent的延迟三秒完整VM假TLS验收尚未宣称通过。证据model-wait-{test,race}.log与model-wait-vet.log。本项与Failure/Probe P2一并更新原session prompt，未运行受限review或提交。

## round3 Probe同步写取消修复

round3唯一P2已修复：Probe在调用开始注册context.AfterFunc，取消时通过既有fail首次原因边界固定ctx.Err并立即关闭独占控制连接，打断sendRequest同步写；不再等待原两秒写deadline。Probe返回前停止或等待取消回调完成，Run仍等待探测任务退出后收集原因并执行唯一撤销/cleanup。已固定的真实协议、业务、deadline或网络timeout不会被后续取消覆盖；正常主动取消导致的传输退出仍按叶子筛选，含Canceled的组合不整包忽略，cleanup错误仍在筛选后合并。未修改共享guest.Client接口、预算或Probe失败断开策略。

新增确定性生命周期测试仅观察真实net.Pipe的Write入口，对端完全不读Probe请求：主动cancel后一秒内完成，cleanup成功返回nil且恰调用一次、绑定撤销；另一路等待真实写timeout触发Done后紧邻外部cancel，仍保留timeout。既有RESOURCE_EXHAUSTED/协议/deadline、非法控制响应紧邻cancel、正常活动/空闲取消与cleanup错误回归保留。

本轮相关六包普通与race全部通过，go vet、中文函数/字段注释AST和diff检查通过。证据为artifacts/implementation-session/probe-write-targeted.log、probe-write-test.log、probe-write-race.log、probe-write-vet.log、probe-write-comments.log。Bridge模型等待修复保留并已获round3认可。原session 01a17116-acda-7a53-9a4d-18f4f657d317不变，更新prompt交主Agent代跑复审；本轮未运行受限review、未提交，真实VM/CLI/供应商验收仍另行报告。
