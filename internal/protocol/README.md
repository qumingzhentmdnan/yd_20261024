# Guest 公共协议

本包定义 control/terminal 共用的 SAGP v0.1 帧、严格 JSON DTO、错误码和操作指纹，不依赖 Host、Guest 或 Fake。字段语义见 [协议候选](../../design/guest-protocol-v0.1.md)，仍须真实生产实现验收后冻结。

- `ReadFrame` / `WriteFrame`：十六字节头、端口/方向、分配前长度检查、短读短写。控制负载上限 64 KiB，终端 16 KiB。
- `EncodeRequest` / `DecodeRequest`：envelope 和按 method 独立校验的参数。未知 method 保留给 Handler 返回 UNIMPLEMENTED。
- `EncodeResponse` / `DecodeResponse`：传入原 Request，检查关联身份、成功/错误互斥及方法结果。
- `Request.DecodeParams`：返回具体参数 DTO 的指针。普通 DTO 使用 `Encode` / `Decode`；`Decode` 传入非空类型指针。`Validate` 只检查类型值约束，不代替运行状态校验。
- `Sequence`、`Counter` 在 JSON 中使用 uint64 十进制字符串；`Fingerprint` 接受 PrepareAgentParams 或 StartSessionParams 值，固定字段顺序，不把 operationID、连接身份或超时纳入摘要。

线协议边界必须调用专用 Request/Response API，不能用通用 `json.Unmarshal` 或仅 `Validate` 替代严格解码。共享负载正反例在 `design/fixtures/`，帧黄金字节唯一源在本包 `testdata/frames.json`；旧 Go/Python spike 也读取它。

本包不实现请求序号追踪、连接握手、pending 容量、响应迟到处理、首字节起算的帧预算或流状态机。调用方负责串行写入、设备读写截止时间、取消和失败后关闭连接；不要把 framing 通过当作这些状态机通过。
