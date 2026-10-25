# Guest Bridge / proxy 实现与验证

工作目录：`/home/yedong/workspace/sandbox/.worktrees/guest-proxy`，分支 `feat/guest-proxy`，基线44f3a1b（包含631f2df、6742a52）。未暂存、未提交、未合并。复用既有Gateway、QEMU/VM生命周期、RPC和device.Conn，不新增依赖或修改公共VM/guest.Client接口。

正式新增 internal/proxywire、internal/guest/bridge、internal/host/proxy，具体RPC新增Identity/Done/Probe，QEMU新增safeagent.proxy唯一端点；Guest显式镜像开关、127.0.0.1 Bridge和固定非root HTTP测试程序。Host身份由已Hello的RPC与相同VMInstance端点建立，不接受Guest自报身份。只有Gateway持有本地TLS上游与明确测试假认证。

设计见design/guest-proxy.md。单请求，第二并发立即有界503；上传有界缓存，响应二进制分块Flush，取消/异常终止通道并先撤销Gateway绑定再关闭、回收VM。Guest proxy故障同时失效control，Host有界Probe检测；4096控制请求容量为短时验证边界，耗尽需新epoch，不自动重连。Host Run显式ready回调后再启动固定工作负载。cleanup始终使用独立10秒context，不借用请求context。

## 可复现命令

```sh
source /home/yedong/workspace/sandbox/artifacts/toolchain/activate.sh
python3 scripts/validate-guest-protocol.py
go test -count=1 -timeout=3m ./...
go vet ./...
CGO_ENABLED=1 go test -race -count=1 -timeout=3m ./...
python3 scripts/guest-agent/build-image.py --assets /home/yedong/workspace/sandbox/artifacts/validation --output artifacts/guest-proxy/new-race --race --proxy
SAFEAGENT_GUEST_INTEGRATION=1 SAFEAGENT_GUEST_IMAGE="$PWD/artifacts/guest-proxy/new-race" CGO_ENABLED=1 go test -race ./internal/host/proxy -run TestQEMUProxyIntegration -v -count=1 -timeout=5m
SAFEAGENT_GUEST_INTEGRATION=1 SAFEAGENT_GUEST_IMAGE="$PWD/artifacts/guest-proxy/new-race" CGO_ENABLED=1 go test -race ./internal/host/guest/rpc -run 'TestQEMU.*Integration|TestQEMUWireFailures|TestQEMUTerminalFaults' -v -count=1 -timeout=4m
```

输出镜像目录必须不存在。素材绝对路径仅适用于当前本机，不随checkout交付。没有/dev/kvm，全部真实VM为Linux/amd64 microvm/TCG，保持-nic none，不宣称KVM。

## 已完成证据

本worktree artifacts保存全部日志和镜像。final-{protocol,test,vet,race}.log记录当前协议校验、全仓test/vet/race通过；Fake及宿主TLS测试与真实VM分别报告。新codec fixture含Request/Response及短写、半帧、超长帧；Host适配器测试覆盖旧epoch/connection/撤销/service/path、迟到帧、上传停滞；RPC测试覆盖未Hello与Probe slots/write/response/capacity失败。宿主真实TLS测试验证认证隔离、SSE首块、二进制、正常复用、撤销后HTTP读取错误、第二请求停滞body有界503、慢上传超时后通道仍可用。

可丢弃设备探针round-7通过：双向65536字节、真实device.Conn ReadDeadline、Close解除阻塞写、背压期间control/terminal可用、Guest关闭Host无EOF、QEMU退出EOF/reset、基础盘不变/进程回收。round-1/2/3/5/6失败均保留；原因与修复见scripts/spikes/guest-proxy/README.md。round-4取消证据不足，未作为最终Close证据。

真实生产链路real-8.log六个独立race Guest TCG全部通过：normal、client-cancel、control-loss、upstream-stall、slow-upload、slow-read。验证UID1000实际HTTP数据、Host认证、上游未结束即Guest看到二进制首块、503、终端/GetState/Stop、故障上游取消和独立cleanup Stop/Wait/Destroy。每次基础盘SHA256均为4e16706e345f0bacbae79893a423ef6c8c1b522f6f078caceaee30f8b07216fc且前后不变。

最终镜像final-race首轮normal Hello超时，其余五项通过；normal单项重验成功。Hello超时根因尚未确定，不能归因为已证明的环境抖动。早期real-1也有Hello超时、real-3编译失败、real-5测试错误要求取消后GetState成功，均保留。旧control/terminal整轮12通过/1准备阶段超时；该共享契约单项重验通过（regression-retry-1.log），不把重验改写为首次全通过。

## 评审与尚未完成的验收

只使用本worktree同一codexx session，xapi/gpt-6.1-sol/medium，记录在artifacts/review-sessions。设计第一轮五项有效问题全部采纳，第二轮允许实现。代码第一轮三项（上传空闲预算、撤销顺序、cleanup独立断言），第二轮一项（拒绝停滞body）均修复并复审；第三轮未发现阻断生产bug，指出Ready启动门禁描述不实，已增加显式ready通知。冗余专项未建议增加防御/兼容层。没有不采纳的有效bug。

当前尚缺完整的Bind/Ready内部确定性闸门、真实旧epoch跨VM迟到请求和新epoch完整重绑，以及Guest/整体goroutine资源证据；设备故障、60秒总时限和宿主FD证据已在下文补齐，仍不声称全部验收完成。部分边界只在宿主真实codec/Gateway测试，不以其代替真实设备验收。真实异常HTTP返回错误目前由宿主网络测试明确断言；Guest取消场景验证上游取消和VM回收。跨平台、KVM、真实供应商/Vault/Agent CLI未运行。Workspace/Import/MCP/UI不在本轮范围。


最终增量状态：显式ready通知曾引入局部变量重名，packages-final.log、packages-boundary.log、real-final-2.log保留编译失败；修复为confirmation后packages-ready-fixed.log通过。real-final-3.log以final-race镜像跑六个独立TCG场景全部通过（51.09秒），包含slow-read时GetState和Run独立回收断言。此后完成最终全仓protocol/test/vet/race重跑，均退出0，对应final-*.log，包含ready与新增边界测试增量。第四轮代码与冗余复审未发现新的阻断bug，剩余覆盖缺口仍按上文列出。

最终旧回归重跑（regression-final.log）：正常闭环、5个共享契约、3个control故障和3个terminal故障通过；HandshakeAndPreCancelledStart的未Hello调用在15秒预算超时，整轮失败。该场景随后独立重验通过（regression-final-retry.log，5.34秒）。两个整轮中均出现不同启动阶段超时，原因未定位，作为未解决的真实运行稳定性问题保留，不能将整轮记为通过。所有场景的独立清理和基础盘校验均通过。

## 继续验收：真实设备故障和总时限

本轮保持生产proxy/Bridge协议和QEMU机器参数不变，只扩充固定HTTP验证入口及测试。新增串口包装器在本轮唯一目录保留console/stderr；正常测试只替换serial输出，不改机器策略。修复了review发现的两项测试假阳性：不再扫描历史轮次console，Guest验证标记通过临时文件Rename原子发布；QEMU端点关闭明确要求EOF，timeout不算通过。

- `boot-diagnostic-1.log`：10个独立启动Hello通过；随后真实故障测试再次出现Hello超时，串口明确停在内核APIC/TSC初始化，未进入/init。PIT对照20轮出现同类早期超时，不能作修复；`boot-icount-comparison.log`采用仅诊断包装器增加显式icount，10次通过但更慢，尚不足以改变生产TCG时钟参数。历史超时不再混称proxy握手故障，内核/QEMU时钟根因仍需独立定位。
- `continuation-verified-2.log`：abort和真实60秒total通过；total上游持续299次进展，在59.99秒结束，早于Guest75秒及Gateway80秒预算，Guest本轮串口确认HTTP_ABORT_ERROR，随后独立Stop/Wait/Destroy完成。device的disconnect/partial/oversized通过且有本轮Guest control服务退出证据，stale-id在Hello阶段超时；后者在`continuation-stop-proof.log`独立重验通过。不能把此整轮记为全通过。
- `continuation-startup.log`：before-open预取消与ready通知内取消两台真实VM通过；cleanup context独立、相同身份可重新Bind（证明没有遗留绑定）、旧Client Identity不可用。尚未覆盖Bind内部/Ready写前所有确定性闸门。
- `continuation-resources.log`：12轮真实宿主HTTP/TLS+内存设备会话，自有Host/HTTP任务均显式等待退出，FD前后8→8。不是Guest进程FD或全宿主goroutine量化证据。
- `continuation-stop-proof.log`：慢读期间Resize成功，Stop调用后固定HTTP进程在本轮串口记录HTTP_STOP_TERM，证明Guest收到TERM；StopSession响应仍可能因HTTP取消引发控制失效而返回Disconnected，不能称其成功完成。Host最终VM回收通过。早期两轮错误测试及GraceMs=0无效错误均保留。该行为是当前“HTTP中途取消结束本boot通道”策略的交互局限。

仍未完成：Bind/Ready各内部竞态的全部确定性闸门、真实旧epoch跨VM迟到请求、新epoch重新绑定完整场景，以及无故障整轮旧契约运行的稳定性。KVM/跨平台/真实供应商未执行。没有自动重试、放大生产超时或静默降级来掩盖启动问题。

本轮最终全仓检查：continuation-all-test.log、continuation-all-vet.log、continuation-all-race.log、continuation-protocol.log全部通过。原session续审确认总时限、原子本轮串口标记和资源测试有效；指出启动取消测试须独立断言cleanup错误，已补该断言后重验。Stop TERM只证明固定进程收到信号，不证明整个进程组停止或响应交付。

续审最终结论：`continuation-4.txt`确认启动取消cleanup独立结果断言修复有效，小范围复审通过，无新增具体问题；`continuation-startup-fixed.log`两项真实TCG均通过。全仓结果仍按上段本轮日志报告；新增独立断言只改测试，已按影响范围执行race真实重验。

补充失败记录：`continuation-epoch.log`尝试用两个独立Manager验证同资源新epoch，发现两者都从本地持久计数1起步；该测试前提错误，已撤下，不作为跨epoch验收。正确验收必须在同一Manager/同一资源停止后重新Start；当前未宣称完成这一场景。生产Gateway对同一可信资源旧epoch/connection的授权检查仍由既有及本轮宿主测试覆盖。

当前完整验收组 `continuation-suite-final.log`：15台独立真实TCG场景中14通过，client-cancel在Hello阶段90秒超时，整轮失败；串口停在APIC/TSC初始化且未运行/init。其余8项生产链路（包括真实60秒总预算）、4项设备故障、2项启动取消均通过，基础盘不变且自有VM回收。不能将此结果改写为完整全绿；此前client-cancel独立真实成功证据仍保留。当前受影响包race `continuation-final-packages.log`通过，无生产代码增量。

跨epoch补充：已新增正确的同一Manager/同资源Stop→Wait→Start测试，`continuation-epoch-final.log`通过两次真实Hello、epoch/connectionID变化、旧身份＋旧/新binding拒绝、新身份＋旧binding拒绝，以及新身份Host Gateway HTTP成功。这里HTTP由Host直接调用，不能替代新epoch Guest Bridge完整重连链路。corrected首轮业务成功但旧epoch兜底Stop报StaleInstance；仅该清理允许StaleInstance后corrected-2在内核早期超时，corrected-3与final均通过，历史失败保留。

时钟对照确认：`boot-icount-comparison.log`和`boot-icount-confirm.log`累计20次启动通过，每次约10秒；仍是显式诊断参数，不作为默认TCG生产参数或默认完整回归全通过的证据。

2026-10-24 启动稳定性续修（以下为最新状态，保留上文历史失败）：

- `clock-boot-30.log`：icount auto/sleep=on启动30/30成功；完整RPC/proxy回归失败，未作为交付修复。`single-boot-30.log`：仅thread=single仍复现早期停滞，撤销该候选。
- 当前显式TCG采用 `-icount shift=auto,align=off,sleep=off`，KVM不变。QEMU文档说明auto时钟仅接近实时，sleep=off避免休眠时跳到下个计时器；不能据此保证2秒预算严格实时。未采用自动重试或扩大生产超时。
- 诊断发现两项独立验证问题：旧RPC必须使用不启用proxy的固定PTY镜像，proxy镜像10秒Ready等待属于另一装配契约；故障注入原始Write须刷新启动阶段遗留的写deadline，避免在实际注入前报过期。已修正。旧终端故障测试原5秒辅助函数包含内核启动，慢于5秒时必然失败；当时尝试用原场景20秒context先Hello，再调用5秒辅助函数；这是已撤销的中间方案，后续发现重复Hello，最终改法见第81行后的记录，生产预算不变。
- `realtime-pty-build.log`构建本轮独立race PTY镜像。`realtime-vmcontract.log`真实TCG VM契约、持久重开及后端检查全部通过；`realtime-partial-clock.log`在Host观测Guest两秒半帧预算为2.188秒，通过1.5–6秒验收窗口。
- `single-all-test.log`、`single-all-vet.log`、`single-all-race.log`、`single-protocol.log`为此前候选阶段全仓普通检查，均通过，不替代最终参数重验。当前完整真实proxy/启动组、修正后旧RPC组和上传时钟测量仍需等待结果。

后续否决与修正：`realtime-suite.log`的60秒预算在27.911秒/139次进展时提前结束，`realtime-upload-clock.log`两秒上传在1.423秒结束；因此sleep=off仍不可交付，icount已完全撤销。`tsc-stability.log`中的1GHz早期频率也已撤销：QEMU普通TCG使用宿主CPU周期而非纳秒，不可填固定1GHz。`notsc-stability.log`证明屏蔽TSC仍在APIC阶段卡住，亦撤销。历史失败不删除。

最终候选为仅显式TCG增加 `microvm,acpi=off`。源码证据：QEMU8.2.2的FADT标记HW_REDUCED_ACPI，无PMtimer；Linux6.8 `calibrate_APIC_clock`在tsc_khz=0时等jiffies，精简ACPI环境没有传统全局tick，造成校准死等。关闭ACPI使用传统PIT/MP表启动；串口确认2 CPU已激活，virtio-mmio仍可用，无icount。`acpi-stability.log`完整通过真实60秒持续进展场景及10台新VM Hello；旧RPC/proxy完整回归正在执行。

测试修正过程保留：第一次将Hello前移后仍调用readyClient，导致重复Hello违反现有契约，`realtime-rpc-fixed.log`三项FailedPrecondition；当前已改为原20秒场景context下单次Hello及Prepare，未改变Guest协议。`acpi-vmcontract.log`仅旧参数字符串断言失败，其真实契约通过；已更新断言后重跑，不记整轮成功。

当前确认：`acpi-rpc.log`完整13项旧真实RPC/PTY场景通过（85.665秒）；`acpi-vmcontract-fixed.log`真实后端契约和普通检查通过；`acpi-final-protocol.log`、`acpi-final-test.log`、`acpi-final-vet.log`、`acpi-final-race.log`均通过。`acpi-stability.log`总预算Host59.993秒/299次进展，10次新VM启动通过，基础盘不变并回收自有VM。codexx原session复审未发现契约问题，提醒关闭ACPI切换qboot/单IOAPIC/命令行virtio发现，已在README声明固件素材要求；本机qboot.rom实际存在。采纳slow-upload必须出现超时标记建议，并新增每次启动串口实际2CPU断言，正在真实重验。

最终真实整轮结果：`acpi-suite.log`退出0，9项生产proxy场景、4项Guest设备故障、2项启动取消及同资源epoch替换全部通过（192.843秒）；60秒预算Host59.993秒/299次进展。`acpi-boot-confirm.log`额外20次启动通过，加上`acpi-stability.log`10次及`acpi-clock-assertions.log`10次，累计40台新VM启动均通过；最后10次每次断言串口2CPU实际激活。`acpi-clock-assertions.log`也通过slow-upload必需标记与半帧真实预算，Host分别2.566秒、2.501秒；此为宽松端到端窗口而非精确校准。`acpi-assertions-race.log`受影响包race通过。自有QEMU按独立context Stop/Wait/Destroy并核对基础盘不变，最终无本任务QEMU残留。失败候选日志均保留，未自动重试或修改生产预算。

当前交付限制仍保留：全部内部Bind/Ready确定性闸门、Guest进程FD/全局goroutine量化、新epoch完整Guest Bridge业务重连尚未覆盖；epoch测试的HTTP仍在Host执行。Stop场景只证明TERM送达与VM回收，不声称StopSession响应交付或整个Guest进程组完成。KVM、跨平台和真实供应商未执行。启动修复40次无复现是本机验证结果，不保证所有内核/QEMU版本。源码支持的APIC全局tick解释与本机对照一致，未将其扩称所有历史超时的唯一根因。

最终原session复审 `acpi-final-review.txt`退出0，接受本轮增量，未发现新增阻塞问题或无依据防御代码；指出第73行中间方案未标历史，已更正。已有验收覆盖限制保持明确，未提交运行记录或执行暂存/提交。
