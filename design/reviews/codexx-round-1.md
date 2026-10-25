结论：**分层方向适合两天 MVP，可以开始模块内部开发；目前还不适合让 Host、Guest、Fake 各自独立实现协议。** 第 7 节冻结门槛已有可执行骨架，但还需收敛生命周期语义，并补齐慢读、取消和重启竞态的验收。

三份文档职责基本清楚：`design.md` 是总体设计，`interfaces-and-fakes.md` 是接口提案，`sandbox-interface-review.md` 是尚未采纳的修订建议。以下按设计评审处理，不把草案缺项当作实现缺陷。全程只读，未读取凭据或无关配置，未启动子 Agent。

1. **P1｜分工前必须解决｜接口裁决入口尚未唯一。**
   接口提案仍使用 `Boot/Prepare/Start/Terminal/GatewayBindings`，评审建议改成 `Start/PrepareAgent/StartSession/TerminalConnection/GatewayAccess`。这是**明确的修订建议，不是真正矛盾**；但开发者需要知道哪些已采纳。另外，总体设计把 HTTP `/start` 描述为“启动 VM / Guest Agent”，接口提案则明确到 `GUEST_READY`，由 `/sessions` 启动 Agent，存在实际表述歧义。
   **最小补充：**指定接口提案为当前契约入口，列出建议采纳结果，统一 `/start` 的完成条件。纯命名优化可以后置。
   依据：[接口提案:45](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:45)、[HTTP 语义:83](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:83)、[总体 API:352](/home/yedong/workspace/sandbox/design/design.md:352)、[评审状态:3](/home/yedong/workspace/sandbox/design/sandbox-interface-review.md:3)。

2. **P1｜VM 契约冻结前必须解决｜运行实例与资源身份需要进入接口。**
   当前 `Stop(VMRef)` 无法表达“只停止旧 epoch”，`VMConnection` 又暂代启动结果和连接装配。正文已要求区分断联与 VMM 退出，因此这是**已识别待决项**。建议采纳评审中的 `VMInstance{VMRef, epoch}` 和独立 `Wait`；否则迟到 Stop、重启和多个等待者会由各实现自行解释。
   同时固定：启动成功后取消调用 context 不杀 VM；Stop 成功表示 VMM 已退出；Wait 取消只结束等待。MVP 无需额外抽象工厂。
   依据：[接口说明:74](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:74)、[实例与生命周期建议:36](/home/yedong/workspace/sandbox/design/sandbox-interface-review.md:36)。

3. **P1｜协议冻结前必须解决｜去重淘汰必须保持“不重复执行”。**
   文档要求 Guest 去重至少覆盖一次 VM 启动，又把容量、保存时限和淘汰留到冻结时决定。两者可以兼容，但必须明确：**记录淘汰后收到旧 Start，不能当作新操作执行。**
   最小方案可只保存当前 session 和已完成启动记录，并在容量不足时拒绝新操作；固定 scope、参数摘要、结果查询及 Guest 重启是否必然更换 epoch。取消与完成竞态也须规定为“调用结果不确定时先查询”，不能换 operationID 重试。
   依据：[去重原则:37](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:37)、[取消门槛:149](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:149)、[去重门槛:152](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:152)。

4. **P1｜终端契约冻结前必须解决｜PTY 背压需要区分连接层与进程层。**
   “慢读有界背压”没有确定无人 Attach、网页断联或长期不读时怎么办。若直接停止读取 PTY，Agent 可能堵在输出上，连带阻塞退出流程。评审建议的“持续排空到有界缓冲或丢弃”合理，但尚未成为正式契约。
   **最小补充：**固定无 attachment 和慢 attachment 的溢出策略；规定 Close/Attach context 取消解除阻塞，只关闭 attachment；SessionExited 后允许有界排空尾部输出。增加慢读期间 Stop/控制 RPC 仍可完成的测试。
   依据：[终端草案:80](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:80)、[终端修订建议:82](/home/yedong/workspace/sandbox/design/sandbox-interface-review.md:82)。

5. **P1｜拓扑相关实现分工前必须解决｜冻结门槛可执行，但范围和时间预算不匹配。**
   第 7 节要求真实设备链路、重试去重、畸形帧、断线与共同 fixture，交付标准具体；不能只完成 Hello 就宣布冻结。不过总体计划仅分配最初两小时，而且最小链路没有覆盖慢读、cancel/完成竞态及多等待者退出。
   **最小补充：**选定一个真实主平台，冻结各端口如何绑定同一 epoch、任一关键连接断开时如何撤销授权；为关闭等待、Stop、drain 和缓冲填入可测试数值，并补上述故障用例。若超时，先并行技术验证，不提前宣布冻结。多端口与单设备复用本身都不是阻塞，**未选定且各自实现才是阻塞**。
   依据：[拓扑门槛:146](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:146)、[spike 验收:162](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:162)、[两小时计划:414](/home/yedong/workspace/sandbox/design/design.md:414)。

6. **P1｜Gateway 生命周期联调前必须解决｜授权终止事件还需一张明确表。**
   Hello 不作认证、身份来自 Host 端点、旧 epoch 拒绝、重连不复活授权，这些原则已经正确表达。剩余问题是：总体设计说“退出后撤销沙箱级代理能力”，编排文档重点列停止、崩溃和断联，尚未明确 **Agent 自然退出时何时撤销，以及重新 StartSession 是否重新授权**。这是范围歧义，不足以认定架构矛盾。
   最小补充自然退出、断联、lease 到期、VM 退出四种事件的撤销动作；稳定 workload 身份可与易断开的连接身份分开，但重连仍需显式重新授权。
   依据：[撤销流程:97](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:97)、[退出撤销:376](/home/yedong/workspace/sandbox/design/design.md:376)、[授权建议:99](/home/yedong/workspace/sandbox/design/sandbox-interface-review.md:99)。

7. **P1｜导出流程实现前必须解决；归档业务字段可后置｜停止后的导出路径不完整。**
   不可变 Host 快照、审批绑定、冲突检查与部分失败说明已经充分。真正缺的是：停止/崩溃后保留 overlay，但 `GuestFiles` 只能通过活 Guest 取快照；保留磁盘并不自动意味着还能预览和导出。
   **最小补充：**正常路径在停止 VM 前取回并验证 Host 快照，审批可稍后进行；异常退出明确标记“尚未取得快照”，规定受控恢复流程，或明确本次不可导出。另需确定删除记录格式，以及 Windows 对可执行位的处理规则，才能让 fake 与真实实现共享预期。
   依据：[停止保留文件:97](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:97)、[GuestFiles:111](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:111)、[导出校验:118](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:118)、[工作副本保留:384](/home/yedong/workspace/sandbox/design/design.md:384)。

**开发建议：**

- **现在可开发：**Manifest 校验和配置渲染、独立 PTY supervisor、QEMU 启停与设备 spike、模型 Gateway 的本地假上游测试、工作区归档校验和 Host 快照逻辑。
- **补齐契约后可并行：**Manager 与高层 fake。先统一实例身份、context 生命周期、退出结果、授权撤销和导出顺序，无需等待文件/MCP 全部线协议。
- **通过第 7 节后再独立实现：**真实 Host/Guest RPC、终端流和传输适配器；随后立即跑真实 VM → Bridge → Gateway 流式请求。
- **fake 方案足以支撑并发开发，但尚未具备可交付的共同契约。**其状态模型、手动时钟、闸门、共享 suite 和真实编解码测试层方向正确；应将评审文档第 5 节的生命周期用例纳入首批 suite，并明确共同类型、fixture 与 suite 的维护人。依据：[fake 要求:174](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:174)、[所有权:215](/home/yedong/workspace/sandbox/design/interfaces-and-fakes.md:215)。

可继续接受主 Agent 的第二轮质询，重点可核对上述哪些应进入首批冻结，哪些只影响后续产品验收。
