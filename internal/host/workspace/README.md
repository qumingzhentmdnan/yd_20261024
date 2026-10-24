# Host Workspace

本包实现显式文件选择的 `Stage`、重启后 `Lookup` 和独立读取的 `OpenImport`，不连接 QEMU，不修改宿主源文件。设计及包格式见 [workspace-stage.md](../../../design/workspace-stage.md)。

调用方先完成用户授权与精确文件选择，再传入绝对 Root 和相对 Paths；空 Paths 代表空工作区。默认 Git 文件选择和敏感文件筛查尚未实现，不能直接把未经确认的整个项目传入。

`NewStore` 接收私有目录和显式 Limits。内容保存在 `baselines/<ID>/` 的 host.json、manifest.json、bundle.tar，不使用 VM SQLite。失败的未发布快照会清理；进程崩溃遗留的 staging、已发布基线的回收及导出审批不在本轮范围。

`Stage` 保存工作树当前字节；文件复制完后不再依赖源目录。仅支持普通文件及由其路径隐含的目录，不保留空目录、链接、所有者和时间戳；保留可执行位。Root 及其祖先不接受符号链接。当前仅 Linux/WSL 的安全复制经过验证，其他平台明确返回不支持。

导入包为规范的不压缩 PAX tar。`Baseline.Digest` 是规范 manifest.json 的摘要，清单绑定各文件路径、大小、可执行位和内容摘要；不是整个 tar 的摘要。`ValidateBundle` 校验规范头、清单及全部内容，使用有界流式读取，不将整个包载入内存。

每次 `OpenImport` 先完整校验，再返回定位到开头的独立句柄；调用方负责 Close。取消后后续读取停止，已经执行的单次本地文件读取不承诺异步中断。

Guest 传输、解包与成功登记尚未接入。只有将来 Guest Import 完成后才能把 ID/Digest 用于真实 Prepare；测试中的固定工作区记录不能替代导入。

验证命令：

```sh
source artifacts/toolchain/activate.sh
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...
```

测试覆盖真实临时文件系统、确定性包、中文与二进制、源修改、取消、并发 Stage/读取、重启恢复、链接/FIFO、容量及损坏输入。它们只证明 Host 模块，不代表 Guest Import、真实 VM 或其他平台验收。
