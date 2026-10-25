# 独立 proxy v0.1

Host/Guest共用SAPX编解码，不改变SAGP control/terminal。协议和状态见 design/guest-proxy.md。16字节头，最大16KiB payload；body按原始字节传输。黄金fixture包含双向Ready、Request、Response、Data、End、Abort；go test ./internal/proxywire校验唯一黄金源及短写/畸形帧。

Ready为双方启动确认，id=0且空payload；其他帧id非零。请求号同物理通道递增，正常End后复用；异常/取消不复用且回收VM。方向及请求阶段由两端状态机检查。元数据最大16KiB；Host额外累计请求体1MiB、单请求60秒、帧间2秒。未完成上传不发送上游，响应逐块发送，异常Abort不能解释为成功End。
