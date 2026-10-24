# Host Gateway 最小设计与验证门槛

状态：本地可行性探针、设计及正式核心代码均已通过 codexx 复审；全仓库 test/vet/race 通过。只读评审记录见 reviews/host-gateway-summary.md。

## 范围与协作边界

实现 Host 内部模型 HTTP Gateway，不修改 internal/protocol、Guest RPC、PTY Runner 或 VM 生命周期接口。不实现 proxy 线协议、Guest-local Bridge、真实 Vault、MCP 会话/工具授权、模型请求 JSON 转换。模型请求字节按固定路由转发，不声称已兼容真实供应商服务。

输入身份由未来 Host 通道适配器从已 Hello 的 VMInstance 与连接建立，禁止从 Guest JSON、URL 或请求头建立身份。本轮测试用 Host 显式构造身份，不能算真实 VM 链路验收。

## 内部 API 与生命周期

新增 internal/host/gateway，使用标准库。ChannelIdentity 包含 vm.VMInstance 和不复用的 ConnectionID，类型可比较。BindingID 为 Host 随机标识。

- New(config, credentials, resolver) 创建 Gateway；服务配置及配额必须显式提供。credentials/resolver 是可信 Host 编译期装配的非 nil 依赖，不由 Guest 输入或配置文件创建，不增加反射检查、自动默认值或替代实现。
- Bind(ctx, identity, spec) 注册当前通道与允许的 ServiceRefs，返回 BindingID。每个 VMRef 只保留一个绑定，同一身份重复绑定报冲突。Host 注册新 epoch/连接时原子撤销该 VM 的旧绑定并取消在途请求。Bind 只供可信 Host 生命周期调用，调用顺序由其保证；不能从随机 epoch 推断新旧，不能开放给 Guest。
- Serve(ctx, identity, bindingID, request) 返回 status、过滤后 headers 与 io.ReadCloser body。身份及绑定必须严格匹配，旧连接、旧 epoch、已撤销绑定、未授权服务均拒绝。
- Revoke(ctx, bindingID) 原子撤销授权并发出在途取消，已不存在的绑定视为已撤销。返回表示取消已经发起，不承诺远端已退出；实际释放在有界请求生命周期中验证。
- Close() 撤销全部绑定、取消在途请求并关闭空闲连接；之后 Bind/Serve 返回已关闭。无持久化：进程重启必须重新握手绑定。

BindingSpec 只引用 Host 配置中已存在的服务。配置包含 MaxBindings、MaxConcurrent、MaxRequestBytes、MaxResponseBytes、MaxHeaderBytes、TotalTimeout、IdleTimeout，全部为正。并发限制为 Gateway 全局在途请求数，满额立即拒绝，不积累无界等待队列。配额从接纳请求直到流 EOF/Close/取消一直占用。

## 上游与凭证

每个 Service 配置 Ref、HTTPS Origin（不含 userinfo/path/query/fragment）、精确 method/path 路由、AllowedNetworks、CredentialRef 和认证类型（Bearer 或 X-API-Key）。请求仅提供 ServiceRef、Method、Path、headers 和 body，不含上游地址；本轮拒绝查询串、编码路径、路径清理变形及未精确登记路由，只支持 POST。

Dial 时由显式 Resolver.LookupNetIP 解析服务域名，逐个检查解析 IP 是否落在该服务配置的 AllowedNetworks，再按实际 IP 拨号；混入未授权 IP 的结果整组拒绝。没有环境代理，没有自动扩大的网络白名单，没有重定向跟随。AllowedNetworks 为 Host 的显式出站授权，测试仅允许 loopback /32；未来真实服务配置需另行验证，不把测试授权当成默认生产策略。

使用 HTTPS 正常证书与主机名校验，最低 TLS 1.2；测试通过显式 RootCAs 信任本地测试证书，禁止 InsecureSkipVerify。Resolver 是生产与 DNS 改变测试实际使用的最小依赖，不引入通用网络工厂。

CredentialStore 仅提供 Resolve(ctx, credentialRef)，返回本次请求使用的 Host 凭证。测试用显式假值与未知引用错误，不读取环境变量或真实 Vault。Guest 请求头按固定允许列表重建，仅转发 Content-Type、Accept、Anthropic-Version；认证头与 Host/Forwarded/Connection/Cookie 等不会复制。Host 最后注入认证。响应只允许 Content-Type、Retry-After、X-Request-Id，不透传 Location、Set-Cookie 或认证头。错误及日志不输出凭证、上游响应 body 或请求 body。

## 数据流与资源

请求 body 仍以 io.ReadCloser 输入；本轮模型 JSON 请求先有界读取至 MaxRequestBytes+1，超限时不发送上游，最大内存由并发数乘单请求上限约束，不使用无界 ReadAll。请求 body 的 Close 必须可与 Read 并发并解除阻塞，这由未来通道适配器保证。先接纳配额再读取 body/凭证，拒绝请求也释放输入 body。

响应直接流式读取，不整包缓存；读到 MaxResponseBytes 后再读取一个字节区分精确 EOF 与超限，额外字节不得交付。Content-Length 已知超限时在交付响应前拒绝。响应 EOF、Close、取消、撤销、总超时或空闲超时均取消请求，关闭输入与上游 body，释放配额；释放只执行一次。注册、返回响应与撤销的竞态必须有测试。

TotalTimeout 覆盖请求读取、凭证解析、拨号、响应头以及整个流。IdleTimeout 从请求接纳开始计时，每次请求/响应有实际字节进展时重置；下游停止读取也会超时，不能只依赖上游发包。使用可重置定时器与 context 取消，不为每次 Read 创建 goroutine。

Transport 显式设置响应头上限、连接及 TLS 超时，不使用环境代理；本轮禁用连接复用，避免复用失败引发透明重发。上游 4xx/5xx/3xx 状态按原状态返回，3xx 不跟随且不转发 Location；流已经开始后发生错误直接返回读取错误，不追加伪造 JSON/SSE 正常结束标志。

核心 Serve 不拥有 HTTP ResponseWriter，未来桥接适配器必须边读边写并及时 Flush，写入阻塞需有写超时/取消，失败须 Close 核心响应 body。Go HTTP 服务端的原始 r.Body.Close 不能保证打断阻塞 Read；HTTP 适配器必须先设置读取 deadline，再关闭 body，并等待取消回调退出后才能结束 handler。设备适配器也须实际验证自己的 Close 解除 Read 契约。探针用本地 HTTP 适配器验证该要求，不把它作为对外可访问的生产 API。

## 编码前可行性验证

探针放 scripts/spikes/host-gateway 独立 Go module。所有请求只访问本机测试服务，凭证为明确假值，测试同步用闸门；真实网络超时使用有界 deadline。

| 风险 | 验收 |
|---|---|
| SSE 被缓存 | 上游等闸门尚未写完时，下游已收到首块 |
| 取消与关闭 | 客户端断开、撤销及总超时后阻塞读取退出，上游观察到取消；空闲计时的状态逻辑由正式模块测试验证 |
| 慢读背压 | 64 MiB 上游不被整包消费；观察窗口内生产量有界，堆增量记录，无无界 body 队列 |
| 凭证与头隔离 | Guest 假认证被丢弃，仅收到 Host 假认证；Host 路由不被覆盖 |
| 路由与重定向 | 未登记路径/地址拒绝，重定向目标命中数为零 |
| 地址策略 | 未授权 IP、DNS 结果从允许变为禁止时，在实际拨号前拒绝 |
| 身份与限额（正式模块验收） | 旧 epoch/连接/撤销拒绝，在途响应持续占额，读写容量精确边界明确；不复制一份业务状态机到可丢弃探针 |

可行性探针仅验证标准库与设计机制，生产模块必须独立测试，不能把原型测试记为生产实现通过。设计、探针结果经 codexx 审查，解决阻塞问题后实现正式代码并再次评审；只使用 codexx，相关轮次复用本工作目录 Gateway 专用 session。
