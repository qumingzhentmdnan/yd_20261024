# Guest Workspace Import 设计与可行性记录

日期：2026-10-24；基线957fc53。本轮仅新增设计和验证记录，没有生产实现、接口修改或Git写操作。原有docs/guest-agent-implementation.md未提交变更保持原样。

## 已确定的设计

见[Guest Workspace Import](../design/guest-workspace-import.md)。独立第四virtio-serial通道safeagent.workspace；Host Stage→Hello→GetState→Import→Gateway→Prepare→Start。每boot只导入一次；失败回收VM，不重连重放。规范bundle沿用既有格式；root私有staging/tree校验后经renameat2(RENAME_NOREPLACE)发布/workspace/project，真实Imported记录供Prepare匹配，不改写镜像Config.Content。

codexx复用原Workspace session，xapi/gpt-6.1-sol/medium，无Claude。首轮三个有效问题已修订；第二轮允许进入spike，并采纳禁止覆盖发布意见。第三轮复审确认设计可作为实现输入、可结束spike，未发现阻塞性的文件安全或生命周期设计问题；其发送前Guest限额检查建议已采纳，设计明确复用Store内部单次校验，不让客户端再完整扫描。记录见artifacts/review-sessions/workspace/codexx-import-design-3.txt。

## 手工验证与结果

环境为本机Linux/amd64 QEMU8.2.2、microvm,acpi=off、显式TCG、512MiB/2vCPU、-nic none，无/dev/kvm。不代表KVM或其他平台。

先执行现有Host Workspace/QEMU普通测试通过；device包没有测试。Host临时目录命令验证O_NOFOLLOW、O_EXCL和rename；这些不是Guest证据。

可丢弃原型与运行记录位于忽略目录artifacts/workspace-import-spike：source/main.go、run.py、bundle.tar、baseline.json和run-{1,2,3}/。Host用真实Store.Stage/OpenImport生成3文件的规范包：1MiB中文路径二进制文件、零字节文件和可执行脚本。每轮独立复制rootfs、生成initramfs、overlay和短Unix端点，只使用只读共享素材。

- 初次按目录go build因artifacts模块边界失败，改为显式main.go路径构建。
- run-1没有进入导入：复制探针后未设置执行位，Guest报Permission denied。进程已回收、基础盘不变，失败日志保留。改为CGO_ENABLED=0静态构建并显式0755后重验。
- run-2和run-3结果均passed=true；后者补强根句柄和实际写任务关闭证据。不能将其改写为首次全通过。

run-3已验证：

1. 第四设备发现与独立连接；Host规范包真实进入Guest，逐文件核对落盘SHA256、0644/0755、UID1000；UID1000子进程读取中文路径并执行脚本，不能访问workspace设备及root私有staging。
2. 提取后先断言project不存在，通过VALIDATED控制闸门等待Host允许发布；完成后以RENAME_NOREPLACE再次发布另一目录明确得到EEXIST。真实Guest openat拒绝符号链接父目录和已存在文件。
3. 内容篡改、错误清单摘要、归档尾部多数据、截断，以及非规范穿越/链接tar均返回REJECTED_CLEAN，失败目录已删除。穿越/链接样本会在规范头校验阶段拒绝，不声称单独覆盖到了所有内部路径解析分支。
4. workspace接收停滞时Host写任务未退出，独立控制ping和terminal echo仍响应；Guest已经启动的持续写任务在设备关闭后返回net.ErrClosed并等待完成。Host shutdown/close后写线程退出并join；Guest所有探针控制/终端任务退出，串口观测IMPORT_PROBE_EXITED。
5. Guest关闭后Host没有立即观察到EOF。run-3还存在未读Guest输出，所以此单项观测不证明“排空后永不EOF”；设计仍按既有真实设备证据不依赖EOF。
6. 两轮成功运行及首轮失败均核对基础盘摘要不变，独立terminate/wait回收自有QEMU，不操作其他Agent进程；运行后逐个核对三轮自有PID均已不存在。

样本WorkspaceDigest为sha256:d3eaf16722f99ed7939f0fd184d66b776d40059f1fd8f0343ea1389a91749d75；不是tar摘要。

## 证据限制与后续工作

原型为了分别验证已有完整校验器和落盘原语，在同一root私有归档上先ValidateBundle再使用tar.Reader提取；该两遍路径不是生产实现方案。生产应迁移公共规范解析核心后单遍校验/提取，不照搬原型，也不让Guest生产代码依赖Host Store。

尚未验证正式SAWI帧、Hello/GetState的真实Boot身份门禁、120秒总预算/30秒提交预算、配额极值、context取消与发布提交竞态、真实Imported→Prepare→Start，以及新功能加入后的control/terminal/proxy全仓回归。这些是生产实现及验收内容；原型没有实现，也不能据此宣布整体功能完成。

原型没有做race构建，本轮未做全仓test/vet/race；生产实现后必须按影响范围执行。旧proxy记录不替代本轮验收。当前不涉及真实CLI、模型供应商、导出和MCP。

## 本机复现命令

以下依赖当前忽略目录探针源码和已有本机素材，不是checkout开箱即用脚本：

```sh
source artifacts/toolchain/activate.sh
CGO_ENABLED=0 go build -o artifacts/workspace-import-spike/probe artifacts/workspace-import-spike/source/main.go
```

run.py固定使用一个尚不存在的run-N目录；保留历史目录，下一次运行必须显式选择新目录再执行python3 artifacts/workspace-import-spike/run.py。日志及结果中分别记录成功、失败、清理和基础盘结果，不自动重试。

后续生产实现已落地，生产测试与代码评审结果见[实现记录](guest-workspace-import-implementation.md)；本文只保留编码前原型证据，不用原型替代生产验收。
