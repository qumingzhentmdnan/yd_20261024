# 真实 CLI 会话接入设计

目标：用户在Host选择Codex/Claude Code、项目和模型服务，并在Host输入API Key；真实CLI在无宿主共享目录、无默认网络的VM中运行，经既有独立proxy通道调用Host Gateway。真实Key不进入Guest配置、环境、镜像、命令行或控制协议。

## 并行归属

- agents worktree：内置codex/claude清单、可信包和镜像安装、Guest Manifest登记、可复现版本与摘要。
- credentials worktree：Host内存凭证库，明确引用、替换、删除与关闭；不默认落盘，不读取既有用户登录信息。
- transport worktree：两种供应商协议必要路由/headers、Host认证注入、长会话Probe与控制请求容量；共享协议变更先协调。
- 主工作区：Host会话编排、最小宿主终端入口、集成验收和最终整合。原有说明修订及交接文档保留。

每条分支独立测试/race和codexx只读review，在各worktree保存独立session；有效问题修复后原session复审，再提交和整合。主工作区完成全仓与重新构建Guest的真实TCG验收。

## Host入口

第一版提供宿主CLI作为用户入口，不等待UI：显式指定可信Guest镜像目录、内核、基础盘、QEMU、加速器、项目与确认选择的文件。密钥从宿主隐藏输入或显式FD读取，不提供命令行key值参数，也不继承宿主环境到Guest。会话内存保存密钥并在结束关闭引用，持久化钥匙串和OAuth/订阅账号不在本次API Key路径中冒充已支持。

先用Git跟踪文件/显式文件列表生成快照；真实密钥和项目选择是不同的边界。不能自动打包HOME或未确认的未跟踪文件。Host只将不可变快照传输，VM内工作区改动暂不自动写回Host。

## 生命周期

Stage → VM.Create/Start → 唯一控制通道Hello → Import → proxy.Run的Bind/Ready → Prepare → StartSession → AttachTerminal。proxy.Run开始前失败由外层清理；进入后由其唯一拥有Revoke/通道关闭及VM Stop/Wait/Destroy。不得两边同时清理。交互退出、SIGINT和控制失效均转入独立有界清理；终端支持原始字节、Resize与退出码。

Host会话使用镜像构建输出content.json，核对所选Manifest与实际Image相匹配；清单和CLI版本不从Guest自报身份获得远程可信证明。Secret只存在于Host凭证库和TLS上游认证头，Guest使用无真实权限的占位值；VM停止时撤销该绑定。

## 验证门槛

模块测试覆盖取消、失败阶段、资源所有权、凭证不外泄、流式响应、旧epoch和长会话。独立假TLS上游能验收真实CLI协议请求但不能替代真实供应商调用。每个CLI需实际VM启动、终端输入、多轮和工具执行验证；真实供应商验收需要用户提供明确授权的Key，不能擅用本机已有凭证。没有Key时明确停在真实供应商验证缺口，继续完成所有不依赖Key的工作。

当前实现已完成 Host 密钥隔离、Codex/Claude VM 安装、PTY 交互和 Host Gateway 链路；真实供应商能力仍按端点实际支持的协议与模型验收。
