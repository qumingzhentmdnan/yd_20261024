> 整合状态（2026-10-25）：两套正式CLI的生产PTY两轮、完整Host Gateway假TLS链路及七牛真实供应商对话均通过。Codex用Responses、Claude Code用Messages，均可在明确导入的项目设置中选择 `gpt-6.1-sol`；配置及限制见 [宿主入口](real-cli-host-implementation.md)。下文包分支的“未验收”仅为其整合前阶段记录，不能当作当前阻塞。

# 真实 CLI 镜像实现与验收

所属文件实现及分层验证已完成；共享Gateway查询/头兼容问题已在整合中修复，两种CLI完整链路通过。包分支与主工作区codexx只读复审均已通过，早期环境限制不再是交付阻塞。

工作目录 `/tmp/safeagent-realcli-agents`。变更仅涉及 `agents/**`、`internal/manifest/**`、`internal/guest/prepare/**`、`cmd/guest-agent/**`、`scripts/guest-agent/**` 以及本实现记录与对应设计。无 Git 暂存/提交，无共享 protocol/runner/gateway/proxy 或 go.mod 变更，无新增 Go 依赖。

## 固定版本与程序摘要

| CLI | 版本 | 固定模型 | 实际程序 SHA256 |
|---|---|---|---|
| Codex | 0.114.0 | gpt-5.4 | `847939c01c628eaad8a8754ac9eb365c03c55a24d5264a6f2deba8fc67c909b4` |
| Claude Code | 2.1.81 | claude-sonnet-4-6 | `047e3f5591d6238b08dd9518729ac335b0e8df1c80fe985e5d7fbda2c18fc281` |

来源、下载包摘要、Codex rg 摘要与运行依赖版本见 `agents/packages.lock.json`；官方配置依据与契约见 `design/real-agent-packages.md`。包分支构建阶段没有执行官方安装器、Manifest安装命令或Host上的真实CLI，没有读取Host登录信息。后续整合验收在VM运行两种真实CLI，由Host隐藏输入用户授权的真实凭据。下载仅在显式 artifacts 目录进行。

## 可复现构建

需要 Linux/amd64 Go 工具链、Python3、curl、dpkg-deb、cpio、gzip；真实验证另需 QEMU 和既有只读内核/rootfs/基础盘。当前环境工具链只读使用 `/home/yedong/workspace/sandbox/artifacts/toolchain`，Go 构建缓存设在 `/tmp`。

```sh
export PATH=/home/yedong/workspace/sandbox/artifacts/toolchain/go/bin:/home/yedong/workspace/sandbox/artifacts/toolchain/bin:$PATH
export GOPATH=/home/yedong/workspace/sandbox/artifacts/toolchain/gopath
export GOCACHE=/tmp/safeagent-go-cache
python3 scripts/guest-agent/fetch-packages.py --agent codex --output artifacts/real-agent-packages
python3 scripts/guest-agent/fetch-packages.py --agent claude --output artifacts/real-agent-packages
python3 scripts/guest-agent/build-image.py \
  --assets /home/yedong/workspace/sandbox/artifacts/validation \
  --output artifacts/final-codex --agent codex --workspace \
  --packages artifacts/real-agent-packages
python3 scripts/guest-agent/build-image.py \
  --assets /home/yedong/workspace/sandbox/artifacts/validation \
  --output artifacts/final-claude --agent claude --workspace \
  --packages artifacts/real-agent-packages
```

输出目录必须不存在。fetch 对已有缓存重新校验；校验失败明确报错，不静默重下或回退版本。build 只消费已核验离线包，不联网。真实 CLI 自动启用 proxy；`--workspace` 是显式必需条件。镜像 rootfs 只安装选中的一个 CLI，不包含另一个 CLI、Host key、Node 包装 shim 或 Guest 下载逻辑。

`build.json` 记录实际包/程序摘要与运行库文件摘要；`content.json` 保持公共 Preparation 形状。Host 编排应使用该文件的 ManifestRef/ManifestDigest/ConfigDigest，真实 Import 后用 Imported 工作区引用和摘要替代文件中的固定工作区占位登记，再发 Prepare。每个镜像至少配置 2048MiB 内存作为已实测规格，不以旧 probe 的512MiB作为真实 CLI 的验收规格。

## 手工原型与依赖检查

下载时核验 OpenAI npm 平台包官方 SHA512 integrity，Claude 官方固定版本 manifest.json 中的 SHA256。以 `readelf -l/-d` 检查原生程序：Codex 是静态 musl；Claude 需要 `/lib64/ld-linux-x86-64.so.2` 与 libc/libm/libpthread/libdl/librt。独立镜像从锁定官方 deb 补齐这些文件、Bash 和 libtinfo6，不使用 Host 当前运行库。Codex 同包原生 rg 一并安装。

首轮自有版本 smoke 使用普通 microvm 时，Codex 在 ACPI 早期时钟初始化卡住；改为与当前生产 Driver 一致的 `microvm,acpi=off` 后启动通过。未修改共享 Driver。Claude 的欢迎界面使用 ANSI 光标移动分隔单词，初版测试匹配导致误判超时；匹配修正后生产启动通过。失败日志保留在 artifacts，不计为通过。

## 验证命令与分层结果

普通及 race 检查：

```sh
go test ./...
CGO_ENABLED=1 GOCACHE=/tmp/safeagent-go-race-cache go test -race \
  ./internal/manifest ./internal/guest/prepare ./cmd/guest-agent \
  ./internal/guest ./scripts/guest-agent/...
go vet ./internal/manifest ./internal/guest/prepare ./cmd/guest-agent ./scripts/guest-agent/...
```

配置测试覆盖所选单包 Catalog、程序摘要、固定模型、真实 JSON/TOML 回读、localhost endpoint 与占位认证。既有 Prepare 安装权限、取消、摘要、隔离 HOME 与 Guest 账本回归继续使用原测试。

真实 TCG（每个 agent 单独运行，用独立 output）：

```sh
python3 scripts/guest-agent/smoke-version.py --protocol --race \
  --image artifacts/final-codex \
  --assets /home/yedong/workspace/sandbox/artifacts/validation \
  --toolchain /home/yedong/workspace/sandbox/artifacts/toolchain/bin \
  --output artifacts/acceptance-codex
SAFEAGENT_GUEST_INTEGRATION=1 \
SAFEAGENT_REAL_CLI_IMAGE=$PWD/artifacts/final-codex \
CGO_ENABLED=1 go test -race ./scripts/guest-agent -run TestRealCLIProductionStart -v -count=1 -timeout=4m
```

Claude 同样替换 image/output 为 final-claude/acceptance-claude。版本 smoke 复制生产 rootfs，使用独立 initrd、overlay、QMP，以 UID1000 启动真正程序；不覆盖生产镜像。`--protocol` 在这个验证副本内安装正式 Manifest 渲染的配置，启动 localhost 假上游，再运行实际 CLI 的 headless 命令。已观察 Codex Responses 与 Claude Messages 请求、固定 model、占位认证、SSE `MOCK_OK` 和零退出。

该 localhost 假上游检查**未经过 Host Gateway**。生产启动测试另外验证真实 Host VM Manager/QEMU/Import/Prepare/Start/PTY/Stop/Wait，使用 `content.json` 选择对应 Manifest，并校验基础盘摘要未变；只报告真实界面启动。Host Bridge 使用既有 fixed HTTP 的真实 TCG 回归单独验证，不将这些分层检查拼称为真实供应商任务完成。

fixed/probe 回归仍以原 `--workspace --proxy` 和 `--manifest --workspace` 构建，分别执行 `TestQEMUWorkspaceProxy` 与 `TestQEMUManifestPrepare`。包分支执行证据如下，整合最终状态以本文顶部及宿主入口文档为准。

## 包分支阶段验证边界

包分支阶段仅使用假服务；后续主工作区已完成两种CLI的七牛真实供应商对话。KVM及跨平台仍未验证。未提供 git、完整语言工具链或全部 CLI 可选功能依赖。首次交互启动保留各 CLI 原生的目录信任/主题引导，Host 终端负责正常转发用户输入。禁止将欢迎界面或 localhost 假上游成功表述为外部供应商兼容性已完全验收。


## 整合前向主 Agent 报告的 Host Gateway 依赖

固定 Claude Code 2.1.81 已实测发送 `POST /v1/messages?beta=true`，`Anthropic-Version: 2023-06-01`，`Anthropic-Beta: claude-code-20250219,interleaved-thinking-2025-05-14,prompt-caching-scope-2026-01-05,effort-2025-11-24`。证据在 `artifacts/final-smoke-claude-routing/console.log`。整合后的Gateway已明确支持 `/v1/messages` 和精确的 `/v1/messages?beta=true`，受控透传Anthropic-Beta/Version并由Host注入认证；Codex使用 `/v1/responses`。两种实际CLI经完整Host链路的假TLS及七牛真实验证均通过，不再存在此兼容阻塞。

当前仅开放实际消费者所需路由，没有加入未观察到的count_tokens或任意查询。配置输入沿用既有Prepare协议，不在Guest伪造新协议。


## 包分支验收记录

工具版本：Go 1.27.3 linux/amd64、QEMU 8.2.2（Ubuntu 1:8.2.2+ds-0ubuntu1.18）、Python 3.12.3、dpkg-deb 1.22.6。构建使用显式现有素材，运行库使用锁文件中独立校验的官方包。镜像 cpio 包含实际构建时间与素材元数据，命令可复现，未宣称镜像逐字节可复现。

| 检查 | 结果与证据 |
|---|---|
| 全仓普通测试 | 通过，`artifacts/go-test.log`；真实集成仍须显式开启，不能把默认 skip 当作真实 VM 通过 |
| 相关单元/Guest/race | 通过，`artifacts/race-test.log` |
| vet/Python 编译/diff whitespace | 通过，`artifacts/go-vet.log`、`python3 -m py_compile scripts/guest-agent/*.py`、`git diff --check` |
| 实际生产 Codex 启动，Host 测试带 race | 通过，`artifacts/final-codex/production-start.log`；真实 Import、Prepare、PTY界面、Stop/Wait |
| 实际生产 Claude 启动，Host 测试带 race | 通过，`artifacts/final-claude/production-start.log`；真实 Import、Prepare、欢迎/主题界面、Stop/Wait |
| Codex 实际 TCG --version/Bash/本地假上游，检查器带 race | 通过，`artifacts/acceptance-codex/result.json`、`console.log`；`/v1/responses`、固定模型、占位认证、SSE MOCK_OK、PROTOCOL_EXIT=0 |
| Claude 实际 TCG --version/Bash/本地假上游，检查器带 race | 通过，`artifacts/acceptance-claude/result.json`、`console.log`；实际 `/v1/messages?beta=true` 与 beta 头、固定模型、占位认证、SSE MOCK_OK、PROTOCOL_EXIT=0 |
| fixed 真实导入与 Host Gateway/Bridge 回归 | 通过，`artifacts/regression-fixed/test.log`，`TestQEMUWorkspaceProxy` |
| probe Manifest/Prepare 真实回归 | 通过，`artifacts/regression-probe/test.log`，`TestQEMUManifestPrepare` |
| 篡改包/BusyBox保护手工检查 | 篡改包在写入 rootfs 前被明确拒绝；两套镜像 BusyBox 字节与只读素材一致 |
| KVM / 跨平台 | 未运行 |
| 真供应商任务 | 整合后两种CLI均经七牛gpt-6.1-sol真实VM对话通过，配置及限制见宿主入口文档 |
| CLI 完整经 Host Gateway 任务 | 整合后真实TCG生产PTY两轮、独立副本假TLS及七牛对话均通过 |

所有最终生产生命周期测试核对基础盘 SHA256 为 `4e16706e345f0bacbae79893a423ef6c8c1b522f6f078caceaee30f8b07216fc` 且未变化，各次使用独立 VM/overlay/socket，并停止等待销毁。

最终输出：

| 镜像 | ManifestDigest | initramfs SHA256 |
|---|---|---|
| `artifacts/final-codex` | `54a92c01e9527a84ebd105eed3085595351e33bc8380f76b1f0e0da6afa5c2ae` | `d7cb38372404399613287d7479087608421d09be695f8161b6fcc0acc1dcb797` |
| `artifacts/final-claude` | `6df9e559038249c53bb0fb60359265b1922e28a2faa7a827473a86fec9313000` | `d3f6a1b102b046360bcd2a7ce49c79fc6aa30a9d11b5aa05477af6a8e535b536` |

上述目录分别含 `content.json`、`build.json`、`rootfs/`、`initramfs.cpio.gz`、`elf-dependencies.txt` 与生产启动/最终 smoke 日志。ConfigDigest 均为 `sha256:48764eb77c6b92dee2ce6ae9490d98a08352017795625186a6f13200deb38747`，绑定原有固定 Prepare Policy；两个 ManifestDigest 分别绑定不同 CLI 的程序和配置。实际程序摘要见本文顶部及锁文件。

## 真实工具执行补充验收

`protocol-smoke` 不再接受纯文本作为工具完成证据。假上游首次下发 Codex 正式 shell function_call 或 Claude Bash tool_use，真实 CLI 执行固定 `/bin/bash /workspace/project/tool-check.sh`：输出 pwd、id -u，读取只存于 Guest 文件的随机 nonce，并创建标记。第二次请求必须回传对应调用 ID 和三项输出，才返回最终文本；最终检查标记内容与 UID/GID1000，并要求恰好两次推理请求。

测试使用独立 HOME、占位 key、独立 initramfs/overlay/socket；Codex 保留 workspace-write 沙箱，Claude 仅测试 argv 用官方 `--allowedTools` 精确批准固定脚本。没有修改生产 Manifest 权限机制，没有新增运行时依赖。Claude 复合命令精确授权被拒绝的首次实验保留在 r1，改用固定脚本精确授权后通过；拒绝未被静默跳过。

命令（分别将 AGENT 替换为 codex、claude）：

```sh
env PATH=/home/yedong/workspace/sandbox/artifacts/toolchain/go/bin:/home/yedong/workspace/sandbox/artifacts/toolchain/bin:$PATH GOPATH=/home/yedong/workspace/sandbox/artifacts/toolchain/gopath GOCACHE=/tmp/safeagent-go-race-cache python3 scripts/guest-agent/smoke-version.py --protocol --race --image artifacts/final-AGENT --assets /home/yedong/workspace/sandbox/artifacts/validation --output artifacts/tool-smoke-AGENT-r2 --toolchain /home/yedong/workspace/sandbox/artifacts/toolchain/bin
```

实际证据为对应目录 console.log/result.json，检查器启用 race；这是 Guest 本地假上游工具协议验证，不是 Host Gateway 或真实供应商任务。下载器增加首选项 `curl -q` 禁用宿主 curlrc，单测 mock subprocess 检查全部下载命令；未读取宿主认证。

## codexx 只读 review 状态

主 Agent 已代跑首轮，session `01a1711f-d155-7ab0-afd4-54cc62178b08`，请求模型 gpt-6.1-sol、medium。唯一 P2 为 curl 默认配置读取问题，现已修复并增加回归测试。首轮证据保存在 artifacts/review-sessions/round1-*；后续复审严格复用同 ID，不创建新 session。复审结果另记录于 artifacts/implementation-session/status.md。
