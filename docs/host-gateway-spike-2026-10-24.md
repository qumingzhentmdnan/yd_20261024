# Host Gateway 本地可行性验证

日期：2026-10-24。环境：Linux/WSL、Go 1.27.3。范围仅本机 HTTP/TLS/SSE 与地址策略探针，代码位于 scripts/spikes/host-gateway 独立模块，不是生产 Gateway。

## 实测

命令：在仓库根目录加载 artifacts/toolchain/activate.sh 后，进入 scripts/spikes/host-gateway，执行 go test -v -timeout 25s ./...。需要允许创建本机监听 socket；第一次沙箱运行因 EPERM 失败，不算方案失败或通过。

修正测试上游后全部四个子场景组通过：

- SSE 第一块在上游等待取消、未结束响应时已经可见。
- 客户端断开和绑定 context 撤销都使上游观察到取消。
- 慢读测试计划产生 64 MiB，实际产生 7,311,354 字节后因适配器写超时终止，未整包读取；该次观测堆增量 476,240 字节。此为本机观察，不是所有平台及任意并发的数学内存保证。
- 重定向不跟随，目标请求数为零；模拟 DNS 从 loopback 变为未授权地址，实际拨号前拒绝。
- Guest 假 Authorization/Cookie 未传入，上游只收到 Host 明确测试凭证及固定路径。

## 失败与修正

第一次获准运行时，测试上游未消费请求体就等待 r.Context.Done；Go HTTP 服务端在这种情况下无法按该夹具预期及时观察断开，测试 25 秒超时。修正为上游先完整消费请求体，再写 SSE 首块并等待取消。没有把失败记为通过，没有放宽取消观察上限。

## codexx 补充验证

首次设计审查提出两项有效问题：原 HTTP 适配器只调用 r.Body.Close，不能保证解除阻塞上传；仅用 IP URL 未覆盖域名证书/SNI。已修正并在 race-2 中全部通过：

- 原始 TCP 客户端声明 10 字节请求体但只发送 1 字节、保持连接，适配器设置读取 deadline 后，总超时和绑定撤销均使 handler 在观察期限内结束。
- 以 model.example.com 为 URL 域名，解析到 loopback 后按 IP 拨号；上游观察到原域名 SNI，错误主机名和不受信任证书均拒绝。
- 响应写首块后停滞，总超时结束阻塞读取，上游观察到取消。
- race-2 全部通过，无 race 报告；该次慢读产生 7,065,594/67,108,864 字节，堆增量 1,532,992 字节。

命令：CGO_ENABLED=1 go test -race -v -timeout 30s ./...，输出保存在 artifacts/host-gateway-spike/race-2.txt。之前 race-1 也通过，但不包含上述补充场景。

## 正式实现仍需验证

探针证明标准库流式、取消、TLS、背压和出站策略机制可用。它没有生产绑定表、并发配额、完整字节计数与空闲计时，不证明这些业务实现已完成。身份隔离、绑定替换竞态、容量精确边界与资源回收必须在设计通过后通过生产模块测试验证。

未访问真实模型、未读取真实 Vault、未接入 QEMU/Guest Bridge，未验证 MCP 或其他平台。
