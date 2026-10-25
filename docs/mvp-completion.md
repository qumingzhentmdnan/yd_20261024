# MVP 补齐与验收

本轮对照 `design/design.md` 补齐工作区审核导出、远程 MCP 和本机 Web 工作台。最终结果须以本文后续验收表及实际日志为准；早期真实供应商对话证明不替代本轮生产镜像验收。

## 工作流

1. 使用锁定官方包构建 Codex、Claude Code 镜像，提供不含密钥的可信启动配置。
2. 启动 `safeagent-web --codex-config <配置> --claude-config <配置>`，打开终端给出的本机访问链接。
3. 填写模型源站、模型名称和密钥，读取 Git 项目候选文件并明确选择；疑似敏感文件默认不选，未跟踪文件需手动选择。
4. 如需远程 MCP，填写精确 HTTPS 端点、独立 Bearer Token 和确切工具白名单，并确认所列工具的业务含义。网关强制限制工具名，但不能证明第三方工具业务上只读。
5. 启动 VM 并与 CLI 交互。修改保留在 Guest 副本，宿主项目不自动变化。
6. 选择“停止 Agent 并审核变更”，查看冻结差异，再明确批准该审核 ID。宿主发生冲突时不会直接覆盖。

模型与 MCP 公开配置生成到导入包，不写宿主项目，也不作为成果回写。如果源目录已有同名配置，入口明确拒绝；可以使用原项目配置或先整理项目后重新选择，不能静默覆盖配置。

CLI 入口仍可使用 `safeagent-host --config <配置>`。正常结束交互后冻结审核；`--review-export` 可在终端预览确认，`--resume-review <审核ID>` 可恢复已冻结内容。恢复审核不需要模型密钥。

当前工作区已构建 Host 可执行文件与两份无密钥配置，可直接启动：

```sh
artifacts/safeagent-web \
  --codex-config "$PWD/artifacts/mvp-codex-session.json" \
  --claude-config "$PWD/artifacts/mvp-claude-session.json"
```

配置使用本机最终 `mvp-final-codex-r2`、`mvp-final-claude-r2` 镜像和明确选择的 TCG。CLI 配置预选七牛源站和 `gpt-6.1-sol`，Web 页面需要自行填写源站、模型和密钥；未把聊天中曾出现的密钥放入配置。模型源站允许地址取自生成配置时的 DNS 精确 IP，DNS 变化需重新确认允许地址。Web 的 MCP 端点沿用相应配置的允许网段，连接其他服务前须显式补充其允许地址。

## 边界

- 已冻结的审核内容保存在 Host 私有目录；尚未完成冻结的 VM 内容不具备崩溃恢复保证。中止会话、代理失效或宿主进程被杀不保证取回修改。应先审核再结束。
- 导出按文件执行，发生 I/O 故障或取消可能只完成部分文件，不是整个项目的事务。修改或删除旧文件时保留同目录 `.safeagent-backup-*`，以保留仍持有旧文件句柄的编辑器所写内容；检查后由用户清理。
- MCP 固定使用官方 Go SDK 的 Streamable HTTP 基线，支持工具发现和受限调用。服务端在 Host 终止协议，下游和远端 session ID 不直接透传；不支持任意服务能力透传、任意网络或 stdio MCP 凭据隔离。
- API Key 和 MCP Token 当前仅保存在 Host 内存；未接入 OS 系统钥匙串，不承诺物理擦除 Go 字符串。浏览器在输入期间同样持有用户填写的密钥，提交后清空输入框，不保存到浏览器持久存储。
- 敏感文件筛查不能识别所有秘密，Agent 可以读取全部实际导入内容，并将其发往获准服务。
- Linux/amd64 TCG 是本机可验证范围；KVM、Windows、macOS 和 ARM64 的运行验收仍需要相应环境。交叉编译不等于运行验收。
- 内置 Manifest 仍随程序登记；添加新 Agent 的配置化扩展、完整安装器、DSH 和统一审计系统不在本轮已实现范围。

## 本轮验收记录

本轮使用专门创建的假凭据及临时项目。模型为本机假 TLS 上游时，必须明确记录，不能称为真实供应商验证。真实 CLI、真实 VM、远程协议测试和 Fake 分别记录。

| 范围 | 结果 | 证据与限制 |
|---|---|---|
| 全仓库单元与契约测试 | 通过 | `artifacts/mvp-full-test-r3.log`；含 Fake、真实临时文件系统及宿主 PTY，默认跳过 VM 集成 |
| 全仓库 race | 通过 | `artifacts/mvp-full-race-r1.log`，不替代真实 VM |
| vet、协议结构与样例 | 通过 | `mvp-full-vet-r1.log`、`mvp-protocol-r1.log`；54 个负载正反例及 2 个指纹向量 |
| 生产 Codex、Claude 原生两轮交互 | 通过 | `mvp-final-{codex,claude}-validation-r1.log` 中 `TestRealCLIInteractiveDialogue`；真实 CLI 与 TCG，Host 假 TLS 模型 |
| 生产镜像网络／文件／假凭据隔离 | 两种 CLI 均通过 | 同组日志中 `TestRealCLIProductionIsolation`；UID 1000、仅 loopback、IPv4/IPv6 TCP/UDP/DNS 无路由、宿主假秘密不可见、可读配置及进程无随机假凭据；包含导出收集和 VM 回收 |
| 越权模型路径 fail-closed | Codex 通过；Claude 复测中 | `mvp-codex-denial-r2.log`；必须实际得到 Host `ErrForbidden` 并确认 VM 已销毁，不以超时当通过 |
| 原生 Codex MCP | 通过 | `review-sessions/mvp-mcp/real-vm-final-r1.log`；真实 VM、正式 Host Gateway 和 TCP/TLS 测试 MCP，允许调用 1 次、禁止工具 0 次；假模型驱动 |
| Web HTTP→真实 VM→审核导出 | 通过 | `review-sessions/mvp-ui/real-web-vm-r6.log`；模型配置生效、Guest 修改不立即回写、宿主冲突拒绝、再次批准导出、生成配置未导出、正常清理 |
| 浏览器渲染、Claude 原生 MCP、真实模型自主工具决策、第二平台 | 未验收 | 不由 HTTP API 测试、Codex MCP 或交叉编译替代 |

上述 `validation-r1.log` 内旧版 `TestRealCLIUnauthorizedRoute` 失败：代理正确撤销，但 READY 输出尚未到终端，测试错误地把输出可见性作为前置条件。修订为先运行完整隔离检查、确认成功，再独立启动越权请求；复测日志单列，旧失败记录保留。更早的导出断链和 CLI 引导尝试同样不记为通过。

模块只读审查均使用 `xapi / gpt-6.1-sol / medium` 并在各自 session 复审：`mvp-export`、`mvp-mcp`、`mvp-ui`，有效 P1/P2 已关闭。总集成审查使用独立 `mvp-integration` session；最终结果另记录，运行记录不提交。
