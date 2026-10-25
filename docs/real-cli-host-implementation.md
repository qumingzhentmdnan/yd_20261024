# 真实 CLI 宿主入口

当前入口为 Linux/amd64 的 `cmd/safeagent-host`。用户选择可信镜像、项目文件与模型源站，在Host隐藏输入API Key。真实CLI在VM内使用loopback Bridge和无权限占位值；Host Gateway解析内存凭证并向TLS上游添加认证。没有读取环境密钥、既有CLI登录目录或默认持久化凭证的路径。Go字符串不能承诺物理擦除，关闭Store只释放所持引用。

## 启动配置

先按 `docs/real-agent-packages-implementation.md` 构建对应镜像，再编译：

```sh
go build -o artifacts/safeagent-host ./cmd/safeagent-host
```

启动JSON由可信Host用户编写，不包含任何密钥。例如Codex：

```json
{
  "agent": "codex",
  "qemu": "/absolute/toolchain/bin/qemu-system-x86_64",
  "qemuImg": "/absolute/toolchain/bin/qemu-img",
  "dataRoot": "/absolute/private-session-data",
  "image": {
    "kernel_path": "/absolute/assets/guest-kernel/boot/vmlinuz-6.8.0-147-generic",
    "initrd_path": "/absolute/codex-image/initramfs.cpio.gz",
    "base_disk_path": "/absolute/assets/workspace-base.raw"
  },
  "contentFile": "/absolute/codex-image/content.json",
  "accelerator": "tcg",
  "vcpus": 2,
  "memoryMiB": 2048,
  "project": "/absolute/my-project",
  "files": ["README.md", "src/main.go"],
  "origin": "https://api.openai.com",
  "allowedNetworks": ["0.0.0.0/0", "::/0"]
}
```

`allowedNetworks` 是Host明确批准的源站拨号网络；示例允许全部IPv4/IPv6地址，可按实际部署收窄为源站网络。Origin仍固定为用户选择的HTTPS源站，不能由Guest请求改写。`files`精确列出用户确认的普通文件，不自动扫描Git、HOME、未跟踪文件或密钥文件；空列表导入空项目。私有数据根必须为当前用户拥有的0700目录。加速器只支持显式`kvm`或`tcg`，不回退。真实CLI的已测规格为2CPU/2048MiB；使用其他规格需另外验证。

```sh
artifacts/safeagent-host --config /absolute/session.json
```

默认从stdin读取一行密钥，终端输入关闭回显。读取按字节进行，避免提前消费之后的会话输入。也可由调用者通过专用FD交付密钥：

```sh
artifacts/safeagent-host --config /absolute/session.json --key-fd 3
```

调用者负责建立FD 3；入口没有命令行密钥值、环境变量密钥或默认密钥文件参数。不要把密钥写入启动JSON。Claude应选择对应镜像及`content.json`、`agent=claude`和明确的Anthropic兼容HTTPS源站。此路径是API Key模式，不代表订阅登录或OAuth已支持。

## 生命周期与交互

Stage → Create/Start → Hello → Import → Gateway绑定与Ready → Prepare → StartSession → AttachTerminal。VM镜像必须为可信构建输出，`content.json`只用于选择登记，不是远程镜像可信证明。Guest重新核对Manifest和配置摘要。实际导入工作区引用替代构建占位工作区。

进入proxy.Run前由会话层清理；进入后由proxy.Run唯一负责撤销绑定、关闭通道和停止/等待/销毁VM。Create部分失败也按明确ID清理；配置冲突不获得已有资源的清理权限。Start返回错误时查询实际运行事实，不把零值实例当作未启动。清理使用独立有界context，真实清理错误向调用方返回。

终端支持原始字节、窗口缩放、stdin EOF与非零退出码。取消会解除输入等待及输出背压，恢复终端和输出FD标志。VM停止会撤销模型绑定。本次导入快照保存在Host私有资源目录；VM内项目改动暂不自动写回Host。当前入口没有导出接口，会话结束销毁VM后，VM内改动随之删除。

## 验证记录

- 密钥库独立普通/race测试通过，xapi只读评审发现的测试同步问题已修复，原session复审通过。
- Host配置边界、二进制输出、非零退出、实际转发后取消、下游背压、创建/启动部分失败清理测试及相关race/vet通过。
- Host入口xapi只读评审发现两项生命周期清理遗漏，均已修复并在原session复审通过。
- `internal/host/session/TestRealCLIHostStartup`须显式启用真实VM验证，检查实际CLI界面、完整Host启动编排、正常交互结束清理、基础盘不变；不把界面启动当作模型对话。
- 两种真实CLI的TCG启动、独立测试副本完整Host Gateway假TLS验收通过；工具执行见包实现记录。Codex正式Manifest/PTY两轮假TLS交互通过。
- 七牛 `https://llmapi.qiniu.io` 的真实Responses请求与VM内Codex对话通过，明确选择 `gpt-6.1-sol`。随机算术题只在模型响应中出现正确结果；密钥从Host隐藏输入。验收后由Host终止并回收VM，不能将主动终止的退出码当作CLI正常退出。
- 当前提供的七牛密钥列出的模型不含Claude，默认 `gpt-5.4` 也被该服务明确拒绝；不宣称其Claude供应商兼容性通过。KVM与跨平台未验证。

评审session及运行日志保存在各worktree的`artifacts/review-sessions/`，不提交运行记录。主入口与已完成Prepare是不同任务，使用独立session并保留旧记录。

## 独立测试副本的完整代理验收

`build-host-proxy-check.py`只复制生产rootfs，在副本登记固定非交互检查器，保持真实CLI程序、正式清单渲染配置和原有Guest Bridge。检查器由正式Runner以UID/GID1000启动；它不在Guest建立假上游，不赋予真实密钥。副本使用fixed登记，不能作为生产Manifest交互流程的替代证据；生产界面启动另测。

```sh
python3 scripts/guest-agent/build-host-proxy-check.py \
  --image /absolute/production-codex-image \
  --output /absolute/independent-codex-check
SAFEAGENT_GUEST_INTEGRATION=1 \
SAFEAGENT_CLI_AGENT=codex \
SAFEAGENT_REAL_CLI_IMAGE=/absolute/independent-codex-check \
SAFEAGENT_VM_ASSETS=/absolute/assets \
SAFEAGENT_QEMU_TOOLS=/absolute/toolchain/bin \
go test -race ./internal/host/session -run TestRealCLIHostGateway -v -count=1 -timeout=4m
```

Claude分别选择对应生产镜像、副本目录与`SAFEAGENT_CLI_AGENT=claude`。Host只给假TLS服务添加专用测试密钥；测试检查实际模型、路由、两种认证、Claude query与beta头、请求体不含Host密钥、Guest终端解析出的结构化最终结果精确匹配Host随机响应与实际零退出、VM销毁和基础盘不变。这证明实际CLI经过完整Host代理，仍不等于真实供应商验收；工具执行另见包实现记录。

当前模型请求总上限为60秒（`proxywire.Total`），入口Gateway的空闲预算与其一致，允许该预算内的静默推理。响应等待不应复用完整物理帧的两秒预算；帧首字节到齐后的完整帧读取、设备写入和上传预算仍独立约束。超过60秒的单次推理会终止通道并回收VM；本版尚未提供动态请求预算。

## 非默认模型

内置Codex默认模型为 `gpt-5.4`，源站和密钥必须实际支持所选模型。对于七牛等提供其他模型的Responses服务，在用户确认导入的项目中创建 `.codex/config.toml`：

```toml
model = "gpt-6.1-sol"
```

将 `.codex/config.toml` 明确加入Host配置的 `files` 列表，并把 `origin` 设为用户批准的HTTPS源站。进入CLI后确认该隔离项目的信任提示；Codex读取项目模型配置，UI和请求均使用此模型。该配置不含Key，也不改变Host Gateway批准的目标源站。此旧版CLI未内置新模型元数据，会显示metadata警告；本次真实文本对话已验证，不能据此推断所有新模型特性均受支持。Claude模型通过内置清单选择，本次仅验证其固定Sonnet 4.6配置。
