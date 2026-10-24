# Guest Workspace Import 实现与验收

日期：2026-10-24，基线957fc53。设计见[导入设计](../design/guest-workspace-import.md)，编码前可行性证据见[原型记录](guest-workspace-import-validation.md)。本轮不包含Manifest配置安装、真实Agent CLI、导出或MCP。

## 已实现

- internal/workspacebundle维护唯一规范包路径、清单、PAX/tar头和内容校验；Linux Extract同一遍解析中流式写入root私有树，逐层openat不跟随链接、排他创建文件。Host旧API与包格式保持一致，私有转发函数已移除。
- internal/workspacewire提供独立SAWI帧、固定transferID=1、严格扁平JSON、黄金字节文件、短写及长度/半帧校验。没有修改SAGP控制/终端DTO。
- QEMU新增safeagent.workspace、workspace.sock与同boot一次交付的OpenWorkspace，句柄仍随实际VMInstance失效。
- Guest root私有staging接收归档，校验提取后以根句柄renameat2(RENAME_NOREPLACE)发布project。失败清理同样使用根句柄递归删除，不通过绝对路径清理。
- Boot预留导入状态，Imported记录与Config.Content分离；导入模式下Prepare匹配真实基线，Start工作目录固定/workspace/project。旧固定测试镜像模式独立保留。提交点测试验证发布前取消拒绝、发布期间取消不撤销已经提交的记录。
- Host Import绑定已Hello/GetState的可信RPC和同实例Channels；发送前在Store既有完整校验路径应用Guest固定限额；成功核对回执的ref/digest/文件数/字节数。模块不关闭共享控制Client、不绑定Gateway或回收VM，调用方在错误后必须结束本boot，按独立context Stop/Wait/Destroy，不能重试Import。
- 镜像使用--workspace显式启用，--proxy可单独组合；cpio统一--owner=0:0，避免Host用户所有权进入Guest后允许工作负载替换系统或工作区父目录。Guest初始化再授权/home/agent，导入只授权project内容。

Go依赖没有新增。新增Store.OpenImportForGuest、Boot工作区根句柄/导入方法和QEMU具体通道方法是本轮真实消费者所需，不改变VMBackend或guest.Client接口。

## 当前验证

普通包测试覆盖规范提取、真实父目录链接/末端覆盖、内容篡改、尾部多数据、容量、取消、身份、准备门禁、禁止覆盖和失败清理。共享wire黄金字节覆盖六类帧；短写使用每次最多3字节的真实流包装器。Fake/既有共享契约随全仓测试运行，新增Import主要使用真实临时文件系统与真实Boot，未另建伪成功Fake。

production-all-{test,vet,race}.log与production-protocol.log记录本轮全仓test/vet/race及既有控制协议校验通过。随后补充确定性提交点测试、内容篡改和删除私有转发函数后，implementation-packages-race-1.log受影响包race通过；最终全仓及镜像重验结果见下文。

真实Linux/amd64 microvm,acpi=off/TCG，未使用KVM：

- 独立race Guest production-tcg-3.log四项整轮通过，28.06秒：Host Stage→Import→Prepare→PTY真实读取中文/二进制与执行文件（UID1000）→Resize→Stop→Wait；旧epoch拒绝；实际文件内容篡改拒绝；传输取消后有界Probe观察失效。每台基础盘SHA256不变并Stop/Wait/Destroy。
- production-regression-rpc.log旧RPC/PTY整轮通过，77.820秒；production-regression-proxy.log正常和control-loss两项通过，15.95秒。镜像均为本轮重新构建的独立race Guest。
- 本轮没有重跑proxy全部故障/60秒总预算组，不将这两项冒称全proxy验收；导入与proxy在同一VM的业务联调已由最终镜像补齐，见下文。

### 失败记录

早期普通测试因正常沙箱禁止本地socket失败，授权环境下重跑。新增测试曾因临时存储根目录权限和Stop字段拼写失败，已修正。production-tcg-1.log正常/旧epoch/坏包通过，cancel错误依赖Guest关闭后Host自动EOF而失败；一次测试补丁定位失败使production-tcg-2.log重复执行旧用例，同样失败，均保留。修正为既有有界Probe后production-tcg-3.log整轮通过，没有扩大生产超时、自动重试或改变设备语义。

初次生产镜像未设置cpio统一root所有权，构建后未用于正式TCG验收；实际production-tcg-{1,2,3}均使用修正后的workspace-import-production-race-2镜像。

## 评审

只使用原Workspace codexx session，xapi/gpt-6.1-sol/medium，不使用Claude。首轮代码review指出Guest入口缺少Import→proxy Ready强制顺序，已补明确的workspaceReady完成屏障；其余未发现可触发文件安全或生命周期问题。最终复审通过，未发现新的阻塞问题，也未发现有实际删除收益的重复校验、静默兜底或多余抽象；原始评审记录保存在忽略目录artifacts/review-sessions/workspace。

## 复现

镜像输出目录必须不存在，运行记录保存在本机忽略目录，不随checkout提供内核、基础盘或工具链。

```sh
source artifacts/toolchain/activate.sh
python3 scripts/guest-agent/build-image.py --assets "$PWD/artifacts/validation" --output "$PWD/artifacts/workspace-import-next-race" --race --workspace --proxy
SAFEAGENT_GUEST_INTEGRATION=1 SAFEAGENT_WORKSPACE_IMAGE="$PWD/artifacts/workspace-import-next-race" CGO_ENABLED=1 go test -race ./internal/host/workspaceimport -run 'TestQEMUWorkspace(Import|Proxy)' -count=1 -v -timeout=4m
```

本轮不声称跨平台、KVM、真实供应商或整个沙箱产品完成。128MiB归档/64MiB内容极值、全部真实设备故障组合和长会话资源量化尚未验收。首次合法End后输入不再消费、回执后关闭单次通道；不会另起后台任务逐一确认拒绝End后的帧，设计已与此边界对齐。

## 最终增量验收

production-tcg-final-1.log使用当前新构建的workspace-import-final-race（--race --workspace --proxy），五台独立TCG场景整轮通过：四项Import共30.75秒，同VM Workspace+proxy联调6.97秒，总包38.806秒。额外联调故意提前发送proxy Ready，在300ms观察窗口内未收到确认；导入完成后确认Ready，并在同一非rootPTY执行真实项目脚本与HTTP请求，Host TLS上游核对实际body和Host假认证，Guest确认二进制首块、第二请求503及完整响应，最后Stop/Wait。300ms窗口本身不证明所有竞态，生产代码的完成通道提供顺序约束。

implementation-commit-race.log另验证回执首字节后断开：以实际已发布后的回执字节作确定性闸门，确认发布记录和项目不回滚，boot进入failed并禁止后续业务。未用sleep猜测提交时机。

最终全仓final-all-{test,vet,race}.log与final-protocol.log全部退出0，通过；git diff --check亦通过。旧RPC/proxy回归发生在新增完成屏障之前；屏障对未启用workspace的旧镜像保持nil跳过，不将旧日志伪称最终镜像回归。

最终代码与镜像证据更新：拒绝响应的写期限也取原总预算与5秒预算的较小值，不在到期后追加等待。codexx-import-code-2.txt已读取该修复并最终复审通过。随后final-2-all-{test,vet,race}.log、final-2-protocol.log全部退出0；production-tcg-final-2.log使用再次重新构建的workspace-import-final-race-2镜像，四项导入与一项同VM代理联调全部通过，总包46.272秒，每台基础盘摘要不变且自有VM回收。具名函数名称前缀与结构字段注释的AST静态检查通过。无新增Go依赖，无Git写操作，原有未提交文档变更未改动。
