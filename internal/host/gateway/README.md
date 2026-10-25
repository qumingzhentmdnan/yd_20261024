# Host 模型 Gateway

本包实现进程内服务授权和 HTTP 转发，不监听对外端口。设计见 [host-gateway.md](../../../design/host-gateway.md)。

Host 完成 VM 握手后构造 `ChannelIdentity`，通过 `Bind` 登记允许的服务，再用 `Serve` 转发请求。VM 新 epoch 或新连接绑定时旧绑定失效；`Revoke`/`Close` 取消在途流。授权只在内存中保存，进程重启后必须重新握手。

配置必须显式提供服务、网络白名单、凭证引用、并发/字节限额及总/空闲超时。只接受已配置服务的精确 POST 路径；上游地址和认证由 Host 决定。拨号前逐个检查本次 DNS 地址，保留原域名的 TLS 验证与 SNI，不用环境代理、不跟随重定向。

固定 CLI 使用 `CLIService(agent, origin, credentialRef string, allowedNetworks []netip.Prefix) (Service, error)` 构造服务，再交 `New` 校验。仅接受 `codex`、`claude`，Ref 固定为 `model`；Origin、凭证引用和网段由 Host 显式提供。Codex 仅 POST `/v1/responses` 并注入 Bearer；Claude 仅 POST `/v1/messages` 及精确 `/v1/messages?beta=true` 变体并注入 X-API-Key。所有其他查询仍拒绝，未登记 beta 变体的服务不会自动获得该权限。没有 `count_tokens` 路由。来源、头部和验证边界见 [real-cli-transport.md](../../../design/real-cli-transport.md)。

请求体按明确上限缓存后发送，响应体按流读取且持续占用并发额度，直到 EOF、Close 或取消。调用方必须关闭响应体。输入 `Body` 在调用时转移所有权；其 Close 必须可以与 Read 并发并解除阻塞。

HTTP 适配器不能直接假设原始 `r.Body.Close` 能解除阻塞上传，必须设置读取 deadline；输出须逐块 Flush 并设置写入超时。设备通道适配器也要实际验证关闭行为。本包没有实现这些接入层。

凭证接口只在 Host 使用，生产调用方必须提供可取消且错误文本不含秘密的实现。`internal/testkit/fakecredential` 仅使用显式假值，不访问环境或 Vault。绑定和 Serve 不能开放为 Guest 自报身份的通用 HTTP 代理。

验证命令（环境须允许本机监听 socket）：

```sh
source artifacts/toolchain/activate.sh
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...
```

当前验证为本机 HTTP/TLS 测试，不代表真实模型、MCP、Vault、Guest-local Bridge、proxy 线协议或 VM 端到端验收。
