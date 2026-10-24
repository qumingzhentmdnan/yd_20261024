# Host Gateway 可行性探针

仅使用本机测试 HTTP/TLS 服务和明确假凭证，验证标准库传输机制；不是生产 Gateway，不连接 VM、真实模型或 Vault。

从仓库根目录执行：

```sh
source artifacts/toolchain/activate.sh
cd scripts/spikes/host-gateway
go test -v -timeout 30s ./...
CGO_ENABLED=1 go test -race -v -timeout 30s ./...
```

环境必须允许监听和访问本机 TCP socket；沙箱 EPERM 不表示测试通过或方案不支持。取消测试的 HTTP 适配器必须设置读取 deadline，仅调用 r.Body.Close 不能保证解除阻塞上传。

覆盖 SSE 首块、客户端取消、绑定取消、部分上传停滞、响应停滞、慢读写超时、认证头隔离、重定向拒绝、IP 授权变化、域名 SNI 和证书拒绝。慢读结果记录具体产生字节数与堆增量，只是该平台的实测观察。

身份绑定表、容量计数、空闲计时及一次性释放留给正式模块验证。详细结果和历史失败见 docs/host-gateway-spike-2026-10-24.md。
