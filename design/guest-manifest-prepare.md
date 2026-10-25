# Guest Manifest Prepare

渲染在 Guest 内完成。控制请求仅引用镜像内置清单，不能提交 executable、argv、环境变量或文件路径。Catalog 校验 ManifestRef、ManifestDigest 和 Linux/amd64 平台；安装器核对实际程序字节与 ExecutableDigest，Runner 不继承守护进程环境。

## 独立准备结果

每个新操作生成 preparedID，并渲染 `/home/agent/<preparedID>` 下的独立 HOME。镜像中的 `/home/agent` 必须由 root 持有、权限 0755，Agent 不能替换或重命名其他准备目录。每次安装用受限根句柄和排他创建，不覆盖已有 HOME，不再复用 Agent 能修改的旧配置路径。所有配置文件为0600，全部中间目录为0700，文件及目录所有权最终交给 UID/GID1000。

安装期间 HOME 根保持 root:root 0700。文件和子目录全部完成后检查取消；最后将 HOME 根换主作为发布边界。此前失败清理整个新目录，旧准备不受影响；此后无条件登记操作成功，响应丢失不撤销副作用。同一 operationID 重放固定结果，参数冲突沿用既有 Fingerprint 契约。活动进程组或并行准备由 Boot 的已有预留检查拒绝。

Boot 保存每个 preparedID 对应的 argv/env/cwd/UID/GID，不修改全局 Runner 或 Config.Content。Start 使用指定准备的执行配置，Imported 的 workspaceRef/digest 从当前 boot 的真实记录绑定。工作区摘要表示导入基线，不保证运行中内容不变。Agent 能修改自己的配置；准备摘要描述发布时字节。

## 摘要与协议

ManifestDigest 绑定内置 YAML、模板和程序资源；ExecutableDigest 绑定实际程序字节。可选 PlanDigest 绑定渲染的完整计划，包括 HOME、argv、环境、文件字节和权限。固定测试模式不返回 PlanDigest。

Manifest 模式 ConfigDigest = SHA256(prepare.Policy 的规范字节)，绑定版本、HOME 根、workspace 与 Bridge 地址。Guest.New 独立核对 Policy 摘要，镜像入口再核对磁盘配置字节，避免用旧配置冒充清单输入。ManifestDigest 与 ConfigDigest 共同确定渲染配方；preparedID 决定独立 HOME，PlanDigest 绑定这次实际产物。固定模式沿用脚本和配置文件字节摘要。ManifestRef 的版本引用格式与 PlanDigest 已同步公共 schema，仍需完整 Fake/共享契约覆盖。

## 验收状态

安装器、真实 Catalog Boot 账本、Fake 与共享契约已覆盖权限、摘要、快照隔离、取消清理、发布前/后取消和重放。最终全仓test/vet/race、49个协议正反例通过。最终重新构建的race Guest分别通过Manifest真实配置读取、多准备/旧preparedID、冲突错误码验收，以及fixed Workspace导入和Proxy回归。具体命令与证据见 `docs/guest-manifest-prepare-implementation.md`。

最终同session只读复审已通过，无剩余P1/P2实现问题或阻塞验收的测试缺口。没有KVM、跨平台或真实Codex CLI验收；probe不发起模型调用，不能宣称真实供应商兼容。
