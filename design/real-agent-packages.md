# 真实 CLI 的镜像内置包

目标是将真实 Codex CLI 与 Claude Code 预装到 Linux/amd64 Guest；每镜像明确选择一个，Guest 只获得 `proxy-managed` 占位认证。Host 外层选择镜像并读取 `content.json`，在真实 Import 后以登记的 ManifestRef/ManifestDigest/ConfigDigest 和 Imported 工作区引用发 Prepare。不扩展 protocol、runner、gateway、proxy 或 go.mod。

## 官方配置依据

2026-10-25 核实 [OpenAI 官方配置文档](https://developers.openai.com/codex/config-reference)（目前重定向到官方 ChatGPT Learn）：`model_provider` 选择命名 provider，`model_providers.<id>.base_url`、`env_key`、`wire_api="responses"` 配置 Responses 上游，`requires_openai_auth=false` 避免要求 Host/ChatGPT 登录。真实包验证针对固定版本 0.114.0，不以滚动文档代替该版本的 Guest 实测。

[Claude Code 官方环境变量](https://code.claude.com/docs/en/env-vars)明确 `ANTHROPIC_BASE_URL` 重定向 API、`ANTHROPIC_AUTH_TOKEN` 产生 Bearer 认证；[官方设置](https://code.claude.com/docs/en/settings)使用 JSON，支持 `env`；[官方安装说明](https://code.claude.com/docs/en/setup)支持固定版本与官方原生程序。只下载官方安装器以核实来源路径，未执行安装器、Claude Agent 或 Host CLI 登录。固定程序版本 2.1.81。

## 固定选择与配置

| 选项 | Manifest | 模型 | Guest 程序 | 协议 |
|---|---|---|---|---|
| `--agent codex` | `codex@0.114.0` | `gpt-5.4` | `/opt/agents/codex/bin/codex` | Responses `/v1/responses` |
| `--agent claude` | `claude@2.1.81` | `claude-sonnet-4-6` | `/opt/agents/claude/bin/claude` | Anthropic Messages |
| `--agent probe` / `--manifest` | `probe@1.0.0` | 不调用模型 | `/opt/agents/probe/bin/probe` | test-http |
| 不传上述选项 | `fixed-v01` | 不调用模型 | `/bin/fixed-workload` | 固定回归 |

Codex 在独立 HOME 的 `.codex/config.toml` 中使用 provider `safeagent`、`base_url="http://127.0.0.1:18080/v1"`、Responses 和占位环境键。Claude 在 `.claude/settings.json` 及受控环境中使用 `http://127.0.0.1:18080`、固定主模型和固定轻量调用模型；禁用非必要流量及自动更新。不存在动态模型猜测。Host 必须为对应镜像显式授权合适的模型和允许路径；若以后要求动态选择 Manifest 中的模型，需要先讨论公共输入契约。

## 包来源与完整性

`agents/packages.lock.json` 锁定来源 URL、版本、下载包 SHA256、实际程序 SHA256，以及 Codex 同包 rg 的 SHA256。Codex 来源为 OpenAI 官方 npm 包 `@openai/codex@0.114.0-linux-x64`，首次下载还核验 npm 官方元数据中的 SHA512 integrity；提取其中真正的静态 musl CLI，不安装 Node 包装层或 shim。Claude 来源为官方 `downloads.claude.ai/claude-code-releases/2.1.81/linux-x64/claude`，SHA256 与该版本官方 manifest.json 对照。

大型程序不嵌入 Guest Go 可执行文件。`agents/*/executable.sha256` 是编译时可信资源；Catalog 的 fingerprint 将这个真实程序摘要与 YAML、模板摘要组合。安装镜像时同时核对下载包、程序与编译资源摘要；Prepare 再以流式 SHA256 检查 Guest 磁盘程序，避免数百 MB 临时内存。不可从 RPC 传入 executable/argv/env 或动态包目录。

`manifest.NewCatalog()` 保留原 probe 语义；生产入口 `NewCatalogFor(ref)` 只登记所选单个内置包，精确引用不支持 latest 或静默回退。配置继续由现有 JSON 结构模板生成真实 JSON/TOML，复用现有隔离 HOME 和发布边界。

## 动态依赖与构建边界

现有 BusyBox 素材虽然包含 libc 和加载器，缺少 Claude 所需的 libm、libpthread、libdl、librt 等。独立构建从锁定 Ubuntu 官方 deb 提取 glibc 2.39-0ubuntu8.9、Bash 5.2.21-2ubuntu4、libtinfo6 6.4+20240113-1ubuntu2.2 的明确程序/库文件，不运行 maintainer scripts，不依赖 Host 当前库版本。程序 ELF 的 NEEDED/解释器已手工检查；最终还须在实际 Guest 运行 `--version` 和 Bash。Codex 的 rg 一并预装。镜像未安装 git、完整开发工具链或所有语言运行时；需要这些工具的任务应在后续镜像范围显式提供。

下载是独立构建前阶段；镜像构建只能消费已核验的本地包，不联网安装，不在 Host 执行 Manifest 命令，不允许 Guest 临时下载。`build.json` 记录所选 agent/version/model/source、程序/包摘要和实际运行库文件摘要；`content.json` 保持现有公共 Preparation 形状供 Host 编排读取。原 fixed/probe/proxy/workspace 选项保持兼容。

## 验证边界

纯配置测试、Prepare/Guest 回归、race、真实 TCG 版本验证及生产启动分别记录于 `docs/real-agent-packages-implementation.md`。版本 smoke 使用生产 rootfs 的独立副本、独立 initrd/overlay/QMP，无网卡，UID1000；不以 probe/shim 替代真实程序。没有真实密钥时只能报告 CLI 启动及已实际执行的假上游验证，不能宣称真实供应商任务成功。KVM 与跨平台需另行验收。


## 向主 Agent 报告的 Host Gateway 依赖

固定 Claude Code 2.1.81 已实测发送 `POST /v1/messages?beta=true`，`Anthropic-Version: 2023-06-01`，`Anthropic-Beta: claude-code-20250219,interleaved-thinking-2025-05-14,prompt-caching-scope-2026-01-05,effort-2025-11-24`。证据在 `artifacts/final-smoke-claude-routing/console.log`。当前 `internal/host/gateway/transport.go` 的 `validPath` 禁止查询参数、路由按整个 Path 精确匹配；`gateway.go` 只保留 Content-Type/Accept/Anthropic-Version，丢弃 Anthropic-Beta。因此当前完整 Claude→Guest Bridge→Host Gateway 链路尚未验收可用；不能把本地假上游结果当作经 Host 代理成功。

需由共享 Gateway 所有者明确实现受限的路径/查询处理并保留合法的 Anthropic-Beta（继续剥离 Guest 认证、由 Host 注入真实认证），以及配置 `/v1/messages`、可选 `/v1/messages/count_tokens`；Codex配置 `/v1/responses`。不能在 Guest 伪造新协议、去掉必要 beta 头或私改共享代码。该要求与 [Claude 官方 Gateway 兼容说明](https://code.claude.com/docs/en/llm-gateway-protocol) 一致。Header 值不能只硬编码为本次观察到的集合，后续版本需按受控策略审核。当前生产 CLI 的配置输入不需要修改 Prepare 协议。
