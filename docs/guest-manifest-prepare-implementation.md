# Guest Manifest Prepare 实现与验证

实现、最终回归及原session最终codexx只读复审均已完成。

## 实现

内置 `probe@1.0.0` 在 Guest 渲染；控制请求不能提供任意可执行路径、环境或 argv。`internal/guest/prepare` 核对实际程序摘要，并用受限根句柄在每个 preparedID 的独立 HOME 安装 JSON/TOML。文件0600、目录0700；安装期间根保持root所有，最后换主发布。此前取消或失败清理新目录，成功发布后操作账本固定成功。Boot 按准备保存执行配置，旧准备不被覆盖。工作区仍绑定真实 Imported。

ConfigDigest 绑定 `prepare.Policy` 规范输入字节，ManifestDigest 绑定内置清单和资源，PlanDigest 绑定实际渲染产物。默认构建保留 fixed 模式，显式 `--manifest` 启用内置 probe。probe 实际读取配置并记录身份、cwd、argv、权限和所有权。

## 最终执行证据

全仓日志在 `artifacts/prepare-policy-race/final-{test,vet,race,protocol}.log`：test、vet、race通过；协议schema、49个正反例、2个指纹通过。运行进程退出码均为0，摘要记录于 `final-exit-codes.json`。

最终源码重新构建的独立race镜像：

- `artifacts/prepare-final-manifest/tcg.log`：真实导入、两次Prepare后启动旧preparedID、相同操作重放、精确CONFLICT/FAILED_PRECONDITION、UID1000读取JSON/TOML、HOME/cwd/argv、0600/1000:1000通过。
- `artifacts/prepare-final-fixed/tcg.log`：Workspace normal/wrong-epoch/corrupt/cancel四场景，以及同VM Imported→Gateway Ready→Prepare→非root工作区/HTTP→退出通过。两种镜像均基础盘摘要不变，自有VM已回收。

Boot `TestManifestCommitLedger` 通过真实Catalog渲染、真实安装器和账本：发布前取消无可启动结果、发布后取消仍登记Preparation/Runner、模拟响应丢失后重放不重复安装。文件边界仅映射到测试私有目录，不在Host执行清单程序。Fake构造输入与Prepare/GetState快照隔离、共享契约的快照污染检查均通过race。安装器另覆盖重复HOME、坏程序摘要、取消清理、受限根路径逃逸、第二文件失败与旧准备保留。

## 复现

```sh
source artifacts/toolchain/activate.sh
python3 scripts/guest-agent/build-image.py --assets artifacts/validation --output artifacts/prepare-new-race --workspace --proxy --manifest --race
SAFEAGENT_GUEST_INTEGRATION=1 SAFEAGENT_WORKSPACE_IMAGE="$PWD/artifacts/prepare-new-race" CGO_ENABLED=1 go test -race ./internal/host/workspaceimport -run '^TestQEMUManifestPrepare$' -count=1 -v -timeout=2m
```

输出目录必须不存在。旧 Workspace/Proxy 集成场景需要独立构建不含 `--manifest` 的 fixed 镜像，不能对 probe 镜像运行固定交互程序断言。

## 复审与边界

原session前三轮有效P1/P2已修复。第四轮确认没有新增P1/P2实现缺陷，提出Boot账本测试、最终重建回归、精确错误码断言与文档同步，现已逐项完成，原session第五轮最终复审通过，未发现剩余P1/P2实现问题或阻塞验收的测试缺口；记录位于 `artifacts/review-sessions/prepare/round5-reply.txt` 与 `round5-events.jsonl`。

没有KVM、其他宿主/架构、真实Codex CLI或真实供应商验收。Manifest probe仅实际读取配置，不发模型请求；模型代理行为由独立fixed同VM回归取证。归档极值与长会话账本问题沿用原有边界，不属本次实现。
