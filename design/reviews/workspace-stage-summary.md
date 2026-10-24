# Workspace Stage 设计评审

日期：2026-10-24。范围：design/workspace-stage.md 与 internal/host/workspace。先完成设计并通过 codexx 复审，再实现并完成代码复审。

## codexx

工作目录：/home/yedong/workspace/sandbox。供应商 xapi，模型 gpt-6.1-sol，推理 medium。本任务与 Guest 通信设计独立，因此创建 Workspace 专用 session：01a16d34-b818-7121-9835-06b886ad119c；后续实现和复审复用该 ID。

首轮发现三项有效问题：

1. P1：os.OpenRoot 会跟随根目录链接，无法满足禁止链接的约束。
2. P2：严格 USTAR 无法支持中文路径。
3. P2：保留带绝对路径的原始文件错误，会泄露 Host 路径。

已修正为从 / 逐组件无链接打开授权目录、显式受限 PAX，以及保留 errno 的路径脱敏。原 session 第二轮复审结论：三项已解决，无新的具体阻塞问题，可以进入实现。提醒实现接受 FormatPAX 对普通条目产生的合法 USTAR 头，并分别校验扩展头。

## 实现评审

复用同一 codexx session，模型与供应商不变。首轮发现四项有效 P2 问题：

1. host.json 的读写上限不一致，长 Root 配合小归档限额可能发布无法读取的基线。
2. Dup 后设置 CloseOnExec 存在并发 exec 继承目录句柄的窗口。
3. 元数据读取失败直接传播错误，可能泄露存储绝对路径。
4. OpenImport 把实际 EIO 等故障也标记为内容损坏。

修复分别为 Host 元数据独立 1 MiB 读写上限、openat 原子设置 O_CLOEXEC、读取错误脱敏、明确区分结构/截断与实际 I/O 故障。新增长源路径与精确空包上限往返、底层错误分类及多错误脱敏等回归测试。复制中取消和源修改使用上下文屏障同步，不使用 sleep。

同 session 第二轮代码复审通过：四项均已修复，未发现新的具体问题。评审覆盖目录内全部 8 个 Go 文件；主 Agent 提示词误写为 9 个，reviewer 已按磁盘实际文件逐一核对，不存在第 9 个遗漏文件。评审未自行运行测试，以下验证由主 Agent 执行。

## 实现验证

以下命令在当前 Linux/WSL 环境均通过：

```sh
source artifacts/toolchain/activate.sh
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...
git diff --check
```

覆盖空包及 PAX 黄金摘要、中文/二进制/可执行位、重启恢复、源修改后的基线隔离、并发 Stage/读取、取消、复制中源修改、链接/FIFO、文件/总量/归档精确上限及损坏包。Git diff 检查不覆盖未跟踪文件内容；新增 Go 文件另经 gofmt、编译和测试检查。

当前为真实 Host 文件系统实现，不另建无消费者的 Fake。未接入 Guest Import、QEMU 文件流或导出审批；未运行非 Linux 平台验证，不将其记为通过。

## 冗余及防御性代码专项审查

按用户要求额外进行三轮，继续复用同一 codexx session，模型为 xapi/gpt-6.1-sol、medium；不再调用 Claude。

第一轮沿调用链确认并简化三项 P3 冗余：

- openSourceFile 返回安全打开时已获得的 FileInfo，copySource 不再紧接着重复 Stat；复制后检查保留。
- importReader 仅保存 ctx/file，不再通过嵌入 reader 重复保存同一文件句柄。
- 固定 Manifest/hostRecord 类型的 JSON 编码不再保留不可达错误分支；外部 JSON 解码及规范字节校验保留。

第二轮验证第一轮修正，并进一步处理三项：

- P2：删除 ValidateBundle 失败后再次检查 ctx.Err 的分支，避免把已经发生的 I/O 或内容错误替换成稍后发生的取消；原始取消仍由 contextReader 传递并可通过 errors.Is 判断。
- P3：核查 Go 1.27.3 源码确认 crypto/rand.Read 保证不返回错误，newID 改为直接返回 string，删除 Stage 两组不可达错误分支。
- P3：sourceStamp 改为 sourceChanged，直接比较同一文件句柄前后的 Size/Mtim/Ctim，不再生成字符串及比较不可能改变的 Dev/Ino。

另删除单个测试用例名称转换辅助函数，直接使用具名场景。

保留的检查具有实际边界依据：路径/类型/硬链接/权限、归档规范及摘要、容量、不同阶段取消、复制后源变更、错误脱敏和分类。显式 Close 检查成功路径关闭错误，defer Close 处理提前返回；不为消除二次关闭新增状态或清理抽象。非 Linux 辅助函数的 panic 为编译占位，入口已明确返回不支持，不以静默默认值代替。

第三轮重新读取全部 8 个 Go 文件，专项复审通过：未发现新的具体问题，也未发现值得继续删除的冗余、防御性代码或过度抽象。确认两轮简化保留必要输入、资源及状态边界。Reviewer 没有运行测试，其输出中的测试状态取自发起时提示词；主 Agent 已随后确认全仓库 go test、go vet、CGO_ENABLED=1 go test -race 均通过。

原始输出按 cleanup-1/2/3 分别保存在 artifacts/review-sessions/workspace/，未覆盖既有评审结果。修改仅涉及本轮 Workspace 实现、测试和评审记录，无 Git 写操作。

## Claude CLI

未取得有效评审结果，不记为通过。首次调用因沙箱网络 EPERM 失败，modelUsage 为空；明确恢复原 ID 返回会话不存在，检查持久目录也无记录。随后使用相同 ID 在授权网络下重试创建：fc935444-c463-4a80-b518-bef065dbc617，180 秒超时退出 124。用户随后明确要求停止使用 Claude，后续仅使用 codexx，不再以 Claude 结果作为实施门槛。CLI 提示配置模型为 deepseek-flash[1m]，不能当作 Anthropic Claude 模型评审。

## 手工验证

Linux/WSL：Python 调用 openat 对末端符号链接返回 ELOOP，对目录链接返回 ENOTDIR；FIFO 可非阻塞打开后识别类型。Python USTAR 验证证明部分长文件名无法表示；codexx 另核查 Go 1.27.3 标准库确认中文路径限制。Go 1.27.3 手工 PAX 探针成功往返 files/中文.txt，扩展记录仅有 path。该结果未验证完整 Workspace 实现或 Guest 导入。

运行输入输出与 session 信息保存在忽略目录 artifacts/review-sessions/workspace/。无 Git 写操作，不修改其他 Agent 拥有的实现。
