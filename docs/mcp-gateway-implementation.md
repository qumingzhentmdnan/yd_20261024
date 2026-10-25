# 远程 MCP Gateway

Host 使用 `github.com/modelcontextprotocol/go-sdk v1.3.1`，测试协议基线为 `2025-06-18`。新增依赖负责 MCP initialize、JSON-RPC 错误、HTTP 会话、SSE 响应和取消，避免重写协议栈。

用户在 Host 显式确认 HTTPS 精确端点、实际拨号允许网段、凭据引用和允许工具名称。Gateway 每次 DNS 查询检查完整地址集合，TLS 保留原域名校验；禁止代理环境、URL 用户信息、查询、片段和重定向。凭据由 Host 内存 Store 在实际出站时解析并注入 Bearer 认证；不会写入项目、Guest HOME 或 Manifest。Guest 只获得 `http://127.0.0.1:18080/mcp/<ref>`。

每个可信 VM 代次/连接绑定创建独立的下游 MCP 会话空间。远端发现和每次调用分别建立独立 SDK 会话，不向 Guest 透传远端 session ID。仅发布 Host 允许工具；未允许工具既不能被发现，也不能通过构造 tools/call 执行。不开放远端 resources、prompts、sampling 或 elicitation。允许工具的名称策略不能证明其业务无副作用；用户应连接可信的有限只读服务，远端同名工具的实现或 schema 变化也需要服务端信任。

上游接受 JSON 或 SSE 响应，每个 HTTP 响应在 SDK 解析前限制为 4 MiB，单次 HTTP 总时间 55 秒，并复用 Gateway 响应头预算。工具发现最多 16 页。下游使用 SDK JSONResponse，受 Gateway 响应预算及现有 proxy 总预算限制；可选独立 GET SSE 返回 405。原生客户端的 `/.well-known/` OAuth 元数据探测在 Guest 本地返回 404，不转发到模型服务。MVP 仅支持显式 Host Bearer token，不实现 OAuth 授权/刷新，不宣称任意 MCP 协议兼容。

撤销先在 Gateway 授权锁内使绑定失效并发布取消，之后在锁外关闭 SDK 会话；旧 session ID 无法跨绑定复用。桥接保留原有单在途限制，模型与 MCP 并发可能返回 503，由客户端处理。

Host 通过 `internal/host/mcp.NewService` 装配服务，并把返回的 `mcp:<ref>` 加入绑定。`GuestFiles` 生成 Codex TOML 与 Claude JSON 项目配置，支持显式自定义模型；生成文件由 Stage overlay 导入，不改用户源目录，不参加导出，同名用户文件冲突明确拒绝。

验证：`go test -race ./internal/host/mcp ./internal/host/gateway ./internal/host/proxy` 覆盖实际 TCP/TLS 上游 SDK、正式 Gateway/proxy/Guest Bridge 和下游 SDK 的 JSON/SSE 握手、tools/list、允许调用、禁止调用、session 跨绑定拒绝、撤销、非法 URL/重定向和 SDK 解析前响应超限。该测试使用假 token、可复现测试 MCP；不是外部商业 MCP 服务或真实模型供应商验收。

生产 Linux/amd64 TCG 验收入口为 `TestRealCodexMCP`，需要显式指定生产 Codex 镜像、VM 素材和 QEMU 工具目录。Host 假模型响应驱动原生 Codex 执行 MCP 工具；真实模型供应商自主选择工具、Claude 原生 MCP 调用及第二平台尚须分别验证，不以本测试替代。

最终生产镜像 `artifacts/mvp-final-codex-r2` 的 `TestRealCodexMCP` 已于本次执行通过（22.32 秒）：允许工具实际执行 1 次、禁止工具 0 次、模型请求 2 次，完整 Run 收尾成功。证据为 `artifacts/review-sessions/mvp-mcp/real-vm-final-r1.log`。早期 `mvp-mcp-real-vm-r1` 至 `r4` 为失败诊断，不能列为整体通过。
