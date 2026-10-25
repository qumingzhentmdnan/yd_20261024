# Manifest 模块实现记录

日期：2026-10-24。

## 已实现范围

`internal/manifest` 提供内置清单加载、固定 schema 校验、精确引用/摘要/平台选择，以及纯运行计划生成。`agents/builtin.go` 只嵌入明确登记的 `probe@1.0.0` 资源；现有 Codex/Claude 占位资源保持原状。

调用顺序为 `NewCatalog → List → Load(ref, expectedDigest, platform) → Render(values)`。Values 包含 Guest HOME、工作区和 loopback Model Gateway 地址；输出 Plan 包含可执行文件摘要、参数、环境、0600 配置文件字节和计划摘要。JSON/TOML 使用结构化编码，变量只替换一次。

定义保持私有只读，每次返回的描述和运行计划独立。外部资源边界检查覆盖未知/重复字段、YAML 别名、非法 Unicode、路径穿越、目标冲突、执行路径与环境白名单，以及解析、展开和编码预算。

新增 YAML/TOML 固定版本依赖用于真实格式处理；未升级已有依赖。详细契约见 [设计](../design/manifest.md)。

## 验证

- 编码器命令探针验证 YAML 严格字段解析、JSON/TOML 特殊字符往返。
- 全仓 `go test -count=1 -timeout=3m ./...`、`go vet ./...` 和 `CGO_ENABLED=1 go test -race -count=1 -timeout=3m ./...` 均通过。
- 删除重复 URL 校验后，重新运行 Manifest/agents 测试、vet 与 Manifest race，均通过。
- 测试覆盖拒绝样例、输入与展开预算、双格式反解、大整数、摘要变化、确定性、返回数据隔离和并发 Render。
- 命令生成的示例计划与配置保存在忽略目录 `artifacts/manifest-validation/output/`，没有在 Host 执行嵌入脚本。

本轮未运行 QEMU；上述结果不代表 Guest 安装、文件落盘或真实 CLI 已验证。

## 评审

使用 `gpt-6.1-sol / medium`，设计讨论和代码审查复用同一个 Manifest 专用 codexx session，记录位于忽略目录 `artifacts/review-sessions/manifest/`。设计评审补充了资源预算和 JSON Unicode 严格校验后通过。首轮代码评审无阻塞问题；采纳其意见，删除已被原始 URL 字符禁令覆盖的重复检查。最终静态复审通过，无未解决问题；未发现需要继续处理的明显重复校验、静默兜底或防御性分支。测试由主 Agent 实际运行，reviewer 没有代跑。

## 后续接入边界

本模块不执行命令、不读取 Host 凭证、不联网、不写文件。后续 Guest 准备层需要负责安全安装配置、核对镜像中实际可执行文件、选择非 root 用户，并与现有固定 Guest 登记衔接。新 ManifestDigest/PlanDigest 不能直接替换当前协议里绑定测试脚本的摘要字段。

Guest Bridge/proxy Agent 可继续独立修改 Guest 入口、Runner 和通道协议；本模块没有修改这些文件。真实 Codex/Claude 清单和 CLI 兼容验证仍需单独完成。
