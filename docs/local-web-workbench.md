# 本地 Web 工作台

`safeagent-web` 在随机 `127.0.0.1` 端口提供单用户入口，直接复用真实 QEMU/session/Host Gateway，不需要 Node 构建或外部 CDN。

```sh
source artifacts/toolchain/activate.sh
go build -o artifacts/safeagent-web ./cmd/safeagent-web
artifacts/safeagent-web --codex-config /绝对路径/codex.json --claude-config /绝对路径/claude.json
```

配置结构复用 `safeagent-host` 的无凭据 JSON：QEMU、镜像、contentFile、资源目录、CPU/内存、allowedNetworks 等由可信本机启动者登记。至少配置一个 Agent。Web 中项目、所选文件、源站和模型覆盖对应会话选择；没有登记的 Agent 启动失败。资源目录不可由浏览器选择。不要同时让多个 Host 进程使用同一个 dataRoot。

终端打印本次随机访问链接，令牌放在 URL fragment，页面读取后从地址栏移除。刷新后认证失效时重新打开终端打印的原链接。不要分享这个链接。本地 HTTP 不提供远程部署能力；不经反向代理开放至网络。

页面流程：填写模型 HTTPS 源站与 API Key → 选择 Git 根目录 → 读取文件 → 检查勾选和敏感提示 → 启动 → 操作终端 → 停止并审核 → 预览冻结 Diff → 确认全部导出，或保留审核副本结束。密钥输入随请求进入本机 Host，页面立即清空输入，不保存至 localStorage、配置或日志。Go 字符串和浏览器内存不保证物理擦除。

Git 默认只选择跟踪文件；未跟踪且未忽略的文件需勾选。常见凭据文件、私钥内容和超过扫描限额的文件默认不选。符号链接不列入候选。筛查不能识别全部秘密，导入的文件可以被 Agent 发送给获准模型。Git 文件名输出最多 4MiB、候选最多 4096 项、内容扫描最多 1MiB。

自定义模型和 MCP 配置由 Host 生成到副本，退出时不导出。若源项目已有同名 `.codex/config.toml`、`.claude/settings.json` 或 `.mcp.json`，生成配置会明确拒绝覆盖；请使用原配置或先整理项目配置。MCP 支持精确 HTTPS Streamable HTTP 端点、Host Bearer Token 和明确工具名白名单；用户须确认这些工具的业务语义为只读。没有 OAuth 登录能力。MCP 连接沿用启动配置的 allowedNetworks 拨号约束。

Web 当前使用纯文本 PTY 视图，支持键盘、粘贴、控制键和窗口宽度同步，但不完整模拟 ANSI 光标、颜色及复杂 TUI；不能将此入口称为完整终端模拟器。输出最多缓存 1MiB，浏览器需持续轮询。浏览器渲染尚无自动化验收；Go HTTP 边界与 VM 测试分别记录。

审核阶段先停止 Agent 进程组，再保存冻结副本。回写仅接受用户所见的当前审核 ID，宿主冲突停止覆盖，原文件备份保留在同目录 `.safeagent-backup-*`；请确认后自行清理备份。中止与进程强制退出不承诺采集未冻结的修改。保留审核或重启后可用 CLI 恢复：

```sh
artifacts/safeagent-host --config /绝对路径/session.json --resume-review 审核ID
```

CLI 默认正常退出后展示 Diff 并要求输入完整 `EXPORT 审核ID`。`--review-export=false` 仍冻结并打印恢复 ID，但不自动回写。恢复阶段不启动 VM 或读取模型密钥。

2026-10-25 验证：`go test -race` 覆盖本地 HTTP Host/Origin/Bearer 边界、审核回写与停止串行化、可取消 CLI 确认、Git 默认选择与敏感提示。`node --check frontend/web/app.js` 与相关 Go vet 通过。生产 Codex 镜像 `artifacts/mvp-final-codex-r2` 在 Linux/amd64 TCG 下真实完成 HTTP 启动、自定义模型配置、PTY 本地 shell 写文件、停止冻结、宿主冲突 409 拒绝、重新确认导出、生成配置不导出及完整 VM 清理（`artifacts/review-sessions/mvp-ui/real-web-vm-r6.log`）。此测试未调用真实模型供应商，未执行浏览器 TUI 渲染或跨平台验收。只读 xapi reviewer 第2轮确认首轮3项 P2 修复且无新 P1/P2；第3轮核对真实验证日志后关闭本次审查，无仍有效 P1/P2。
