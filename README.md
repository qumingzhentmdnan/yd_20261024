# SafeAgent

SafeAgent 是一个将 Codex 和 Claude Code 运行在隔离 MicroVM 中的本地工作台。项目文件经过明确选择后导入 VM，模型密钥由 Host 代理保存，不写入沙箱。

## 功能演示

[观看功能演示视频](./展示.mp4)

## 当前能力

- QEMU MicroVM 生命周期管理
- Codex、Claude Code 隔离终端
- Host 模型代理与凭据隔离
- 工作区变更审核与确认导出
- 沙箱停止、保留和删除
