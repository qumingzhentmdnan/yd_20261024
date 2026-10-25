# 本地工具安装与手工可行性验证

日期：2026-10-24。范围：开发环境安装与真实 QEMU 最小设备链路；没有开始 SafeAgent 业务代码、Runner、Gateway 或正式 RPC 实现。临时 init 脚本与探测脚本仅用于设备验证，位于 Git 忽略的 artifacts/。

结论：microvm + 无通用网卡 + virtio-serial 多端口 + 非 root PTY + HTTP/SSE 字节桥，在当前 WSL2 环境的显式 TCG 模式下可行。尚未满足“全部前置验证通过”的开工门槛。

## 环境和安装

- 宿主：Ubuntu 24.04.4 LTS / WSL2 / x86_64；内核 6.18.40.1-microsoft-standard-WSL2。
- Go：go1.27.3 linux/amd64，执行时从 [Go 官方发布清单](https://go.dev/dl/?mode=json) 查询到的最新稳定版。
- Go 安装包：go1.27.3.linux-amd64.tar.gz，大小 70588678 字节。
- Go SHA-256：`65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40`；下载后已核对大小及哈希。
- QEMU：Ubuntu 软件包 `1:8.2.2+ds-0ubuntu1.18`，系统模拟器与 qemu-img 均可运行。
- 辅助工具：socat 1.8.0.0、busybox-static 1.36.1、cpio；从 Ubuntu APT 软件包下载并本地解压。
- Guest 内核：Ubuntu 6.8.0-147-generic；virtio-mmio、virtio-console、virtio-blk 和 ext4 已编入该内核。
- Guest 用户空间：用于设备测试的 BusyBox initramfs 与 glibc socat 及依赖。不是最终 Agent 基础镜像，未证明 Claude/Codex 运行兼容性。

系统级安装尝试因 sudo 要求密码未执行，改为项目本地安装；不修改 /usr/local、系统软件包数据库或 shell 启动配置。

```sh
cd /home/yedong/workspace/sandbox
. artifacts/toolchain/activate.sh
go version
qemu-system-x86_64 --version
qemu-img --version
```

activate.sh 仅对当前 shell 设置 PATH、GOCACHE、GOPATH；工具和缓存均在 artifacts/toolchain。Go 本机编译运行临时 hello 成功，linux/arm64 交叉编译成功；ARM64 产物未在 ARM64 Guest 执行。

## 实际验证结果

| 项目 | 状态与证据 | 结论边界 |
|---|---|---|
| QEMU microvm 启动 | 通过，Linux 启动并输出 SAFEAGENT_GUEST_BOOTED | 仅 WSL2/TCG/amd64 |
| 硬件加速 | 未通过，/dev/kvm 不存在，显式 -accel kvm 返回无法初始化 | TCG 是明确选择的功能测试模式，不是硬件加速或性能验证 |
| 多端口设备 | 通过，一个 virtio-serial-device 上配置 control、terminal、proxy 三个 virtserialport | 验证候选拓扑可用；不代表跨平台参数或业务 framing 已冻结 |
| 控制双向通信 | 通过，HELLO-manual-spike、ping/pong、status/STATUS-ready | 纯文本设备探测，不是正式 Hello/epoch/鉴权协议 |
| 非 root PTY | 通过，uid=1000(agent)、gid=1000(agent)、tty=/dev/pts/0 | 无非 root → root 提权全面测试 |
| 终端基础行为 | ANSI、stderr 合流、Ctrl+C 中断 sleep 后继续输入均通过 | Ctrl+D 已发送，但没有独立证明连接 EOF/退出状态；Host Resize RPC 未实现 |
| 窗口调整 | Guest 内 stty rows 40 cols 120 后查询正确 | 初始尺寸未设置时 stty 报错，正式 Runner 必须在启动时设置初始尺寸 |
| 外网默认拒绝 | Guest 只有 lo、无默认路由，IPv4/IPv6 TCP、UDP 与 DNS 均返回 Network is unreachable | 无网卡候选配置的负向冒烟；不包含正式 Gateway SSRF/白名单验证 |
| 宿主文件隔离 | Guest 无法看到预制的宿主假秘密文件路径；无宿主目录共享参数 | 不等于证明不存在 VMM 逃逸漏洞 |
| 差分磁盘 | Guest 在 /workspace/result.txt 写入 guest-change；base SHA-256 始终相同，qemu-img check 无错误 | 验证 COW，不是完整项目导入/审核导出 |
| 差分内容可提取 | 测试 overlay 转 raw 后 debugfs 可读到 result.txt，base 中不存在此文件 | 仅受控测试盘离线检查，不作为生产不可信文件导出方案 |
| Guest-local HTTP 桥 | Guest wget 访问 127.0.0.1:18080，经独立设备流到 Host 测试响应器，返回 200 | 上游为本地固定测试响应器，无模型 API、认证或 MCP |
| 分块 SSE 与控制并发 | 首块 data 可见后，Host 暂缓末块；控制 ping/pong 正常；再放行末块，wget 成功退出 | 证明不同物理端口能基本独立推进；未测试多并发模型流、背压上限和长期饥饿 |
| QMP 管理与回收 | qmp_capabilities、query-status、quit 成功，QEMU 进程退出，控制 socket 读结束，无遗留 QEMU 进程 | QMP quit 是 VMM 终止，不代表 Guest 优雅关机或正式 StopSession 完成 |

## 使用的候选设备参数

核心配置如下；完整启动命令保存在本地 artifacts/validation/start-vm.sh：

```sh
-machine microvm -accel tcg -cpu max -m 512M -smp 2
-nodefaults -no-user-config -display none -no-reboot -nic none
-device virtio-serial-device,id=serial0
-device virtserialport,bus=serial0.0,nr=1,chardev=control,name=safeagent.control
-device virtserialport,bus=serial0.0,nr=2,chardev=terminal,name=safeagent.terminal
-device virtserialport,bus=serial0.0,nr=3,chardev=proxy,name=safeagent.proxy
-device virtio-blk-device,drive=workspace
```

Host 端点在权限 0700 的 /tmp/safeagent-manual-vm 下；Guest 无通用网卡，只启用 loopback。基础盘为 64 MiB raw ext4，工作盘为 qcow2 overlay，没有挂载宿主项目。

手工探测使用 socat、BusyBox、QMP 和临时 Python socket 客户端；不是并行实现了生产 Host/Guest。完整本地证据：

- artifacts/toolchain/downloads/go-release.json：Go 官方元数据。
- artifacts/toolchain/downloads/ubuntu-packages.json：Ubuntu 包版本与 SHA-256。
- artifacts/validation/rootfs/init：临时 Guest 启动与转发配置。
- artifacts/validation/start-vm.sh：实际 QEMU 启动命令。
- artifacts/validation/probe-vm.py：临时设备探测命令集合，结束时关闭 VM。
- artifacts/validation/probe-results.json：最终成功一轮的控制、PTY、网络和 SSE 结果。
- artifacts/validation/console.log：Guest 内核及 init 日志。
- artifacts/validation/base-before.sha256：基础磁盘基线摘要。

以上 artifacts 被 Git 忽略，仅存在于本机；本文记录结果与关键配置，不声称全新检出仓库即可复现。复现前需要重新准备相同版本工具、内核、initramfs 和测试盘。

## 手工验证暴露的实现约束

1. QEMU 本地解包后 BIOS 数据路径需要显式修正。已将 bios-microvm.bin 链接到同一安装目录的 SeaBIOS 数据文件；未来产品打包需验证数据文件布局。
2. socat 在打开 PTY 前降权会失败。当前由 root 测试服务创建 PTY，再通过 su 启动 UID 1000 子进程；正式 Runner 需明确 PTY 打开、终端初始化、降权的顺序。
3. Host 未连接时 virtio 端口可能返回 EOF。临时流程先连接 Host attachment，再由控制命令启动 PTY 转发；正式协议需处理离线、重连和端口关闭，不能用 EOF 直接判断 Agent 退出。
4. stderr 必须显式接入 PTY，否则错误落到 Guest 启动控制台，不会出现在用户终端。
5. 创建 PTY 时必须设置初始 rows/cols；Guest 内 stty 成功不能替代未来 Host Resize RPC 的验收。

## 单次创建与启动计时

应用户询问补测一次，使用独立新 overlay 与端点，测后已关闭 VM，退出码 0。配置为 microvm/TCG、2 vCPU、512 MiB 内存、64 MiB 基础盘、临时 BusyBox initramfs：

| 阶段 | 单次实测 |
|---|---|
| 创建新 qcow2 overlay | 41.84 ms |
| 启动 QEMU 到 Guest 就绪标记 | 3425.01 ms |
| 启动 QEMU 到控制通道 pong | 3443.69 ms |

本地记录：artifacts/validation/timing-results.json。这是本机文件缓存已热的单次测量，不含下载、项目导入、模型连接或真实 Agent 启动，不能作为生产镜像、冷启动分位数或硬件加速性能承诺。

## 开始业务代码前仍需完成的手工验证

1. 确定真实主平台，验证 KVM/WHPX/HVF 和拟交付的设备组合；当前没有可用 KVM，Windows/macOS 未测试。
2. 使用最终架构的 glibc Guest，预装并锁定 Claude/Codex，分别验证隔离 HOME、配置优先级、非 root PTY、实际退出码和子进程停止。
3. 在无通用外网配置下，通过 Host 侧临时代理完成真实模型请求与 SSE；真实 Key 只在 Host，检查 Guest env/fs/进程及日志无 Key；断代理后不得直连回退。不要将真实 Key 写入当前临时探测脚本。
4. 完成一个可复现 MCP 服务的 initialize、tools/list、受控 tools/call 与会话隔离；固定测试凭据仅在 Host。
5. 手工完成项目副本导入、变更取回、Diff 审核、宿主冲突阻止覆盖；停止前快照成功和快照失败两条路径均需明确。
6. 验证关闭/慢读/强停/guestd 异常的设备层行为，以及 SSE、PTY、控制持续并发；明确限制和超时。
7. 验证目标平台系统 Vault 的写入、读取、撤销与无桌面会话时的失败行为；本轮没有实际 Vault。

正式协议的 request/operation 去重、epoch 校验、Fake 与真实实现契约一致性，需要后续候选实现才能测试。它们不能通过“安装 QEMU 并手工 echo”提前证明。当前保留业务代码不开工的状态，不将底层冒烟通过等同于全部可行性或安全验收通过。
