# Guest proxy v0.1 最小设计

本任务基线 44f3a1b（包含 631f2df、6742a52）。Linux/amd64 microvm/TCG；本机没有 /dev/kvm。素材只读使用主工作区 artifacts/validation，工具链使用主工作区 artifacts/toolchain；本 worktree 独立保存镜像和运行证据。既有合入验证是历史证据，不能替代本轮验证。

## 协议选择

选择独立 internal/proxywire namespace，保留 SAGP control/terminal 的端口、类型与 v0.1 语义。独立头为 16 字节：SAPX、版本 0/1、kind、保留零、uint32 BE payload 长度、uint32 BE 请求号。每帧最多 16 KiB；元数据 JSON 严格解码（未知字段、尾部拒绝），body 为原始字节。公共包同时拥有 DTO、codec、黄金 fixture，不依赖 Gateway。

Host→Guest Ready(id=0)，表示可信 Hello/GetState 与 Bind 已完成；Guest 收到 Ready 前不启动 HTTP listener。Guest→Host Request(id递增非零，service/method/path/headers)，Data、End；Host→Guest Response(status/headers)、Data、End 或 Abort（固定无秘密错误）。Host Ready 不携带身份、绑定或上游配置。请求头元数据最多16KiB；HTTP body限1MiB；Gateway仍执行自己的实际配置限额。响应每块16KiB，不按行解析、不更改二进制。

只允许一个在途请求，Guest 用非阻塞资格立即返回503给第二个请求；直到正常End/流关闭才释放。请求体可先在Guest有界缓存，读取总预算10秒，避免客户端上传停滞占据设备读者；Gateway输入仍为可关闭的有界内存body。每个HTTP请求最多60秒，读写空闲预算2秒。HTTP请求头上限16KiB，ReadHeaderTimeout 2秒、IdleTimeout 2秒，HTTP服务只绑定127.0.0.1固定端口。

正常End后允许同一物理通道的下一递增ID；收到乱序、重复、未知类型/ID、半帧、超长帧即终止通道，不扫描magic。任何中途取消、超时、异常或Abort都使整个proxy通道不可复用，避免迟到数据被下一请求消费。不支持同boot重连、重放、终止确认后复用；终止确认是物理VM回收完成。HTTP响应开始后的异常使用http.ErrAbortHandler关闭HTTP传输，不能输出成功EOF。取消由关闭proxy物理句柄并设置HTTP读写deadline打断；Guest设备Close不保证Host EOF，由独立控制探测兜住。

## 可信装配与失效

QEMU增加safeagent.proxy、proxy.sock；Channels.OpenProxy只交付一次，所有句柄仍由channelRun跟踪。保持VMBackend/Driver/Process及guest.Client不变。RPC具体Client新增已握手可信身份快照（VMInstance/connectionID）、失效Done，以及有界Probe：Hello成功后才可取身份，Probe复用GetState，任何探测失败使当前控制连接失效。身份不从Guest proxy payload或HTTP头提取。

新增internal/host/proxy具体Session：接收已Hello的*rpc.Client、同一实例的*qemu.Channels、Gateway、BindingSpec及独立VM回收回调；核对实例一致，打开一次proxy，绑定后发送Ready。控制探测每1秒一次，每次最多2秒（包括请求资格、写资格和响应等待），繁忙/超时也视为失败，不无限豁免。proxy逐帧读取从首字节起最多2秒，空闲首字节等待由控制探测和生命周期context中断；写每帧最多2秒。响应读取通过Gateway总/空闲预算控制，输出停滞通过帧写预算控制。

退出先Revoke（独立context），再关闭proxy、RPC Client，再调用独立cleanup context中的Stop/Wait/Destroy回调。控制或proxy失效一律回收VM，无自动重连。普通HTTP正常body.Close或terminal.Detach不触发VM清理；HTTP中途取消将proxy标记故障，按已授权生命周期结束当前VM。清理回调属于显式装配契约，即使请求context已取消仍执行。

## 并发与所有权

Guest固定一个设备读者、串行写者（只在取得单请求资格后写），HTTP并发请求没有队列；Host固定一个proxy处理循环、一个控制探测循环。HTTP handler在获得资格后发送Request/Data/End并同步消费响应；设备关闭由context.AfterFunc执行，等待回调完成后handler退出。无每帧/每次Read goroutine。Host在读取完整有界请求后调用真实Gateway.Serve；内存请求body的Close可解除Read（不阻塞）。响应由单处理循环Read/Write/Flush，关闭Gateway body保证配额释放。慢HTTP读写与设备读写均有deadline。Host不需要缓存整响应，最多一帧加Gateway内部缓冲。Guest同样最多一响应帧。

Guest入口打开proxy设备并等待Ready的任务独立于Boot.Serve；只有proxy配置存在时开启（镜像显式登记），既有PTY镜像兼容。新增固定Go HTTP工作负载由构建脚本显式选择，UID1000沿用Runner；原PTY固定入口保留，可在PTY命令中触发固定HTTP测试程序，证明控制/终端和proxy并行。lo仅在新镜像初始化时启用，QEMU保持-nic none。

## 编码门槛与验收

先运行scripts/spikes/guest-proxy真实设备原型：双向二进制、Guest→Host停滞写取消、Host→Guest停滞写取消、Guest关闭设备与Host EOF差异、proxy背压期间control ping与terminal echo。每次独立TCG镜像、socket目录、overlay，记录命令/失败/基础盘前后摘要和进程回收。设计与本轮探针经codexx只读评审并修复有效问题后才写生产实现。

正式验证覆盖公共codec黄金fixture/半帧/限额/方向与状态，真实Gateway+TLS/SSE内存通道测试，身份/Hello/撤销/新epoch闸门，取消和单并发拒绝。真实VM新构建race Guest验证UID1000、实际请求/Host认证、SSE首块/二进制、慢上传/慢读/取消/超时/断联/控制探测回收及旧control/terminal回归。全仓protocol/test/vet/race另列。Fake、宿主真实TLS、TCG、跨平台分别报告；KVM/真实Vault/供应商/Agent CLI不在范围。

## 设计评审第一轮修订

1. Guest proxy一旦已接入后异常，入口必须同时关闭control Conn，使Boot.Serve退出；入口拥有两个设备和listener并等待任务。Host探测因此会在有界预算内失败，不能只关闭proxy。上传尚未发送任何proxy字节失败只结束本地HTTP请求。
2. Host读到Request即开始60秒总预算、每帧间2秒空闲预算，累计body硬上限1MiB，独立于Guest和Gateway；头解码同样有16KiB边界。Gateway转发纳入剩余Host请求预算。任一超限/超时销毁通道。
3. Probe走callWithCancellation(...,false)，到期直接fail，且发送deadline与ctx统一，不追加Cancel。沿用4096请求的既有不可淘汰账本，本轮明确为短时验证会话；耗尽是通道失效并回收VM的容量边界，不承诺长期运行。后续长会话设计单独解决，不盲目放大或淘汰账本。
4. Session启动/失效由同一生命周期任务拥有：先安装控制失效取消回调；OpenProxy/Bind/Ready均使用此生命周期context，每一步结束复核取消。Bind一旦成功必由统一defer Revoke，所有启动失败也关闭已接管句柄并执行独立10秒cleanup。控制失效只取消/关连接，不在回调中Bind或执行cleanup；唯一Session任务待各子任务结束后清理，杜绝清理结束后重新绑定。Ready与失效竞态由取消关闭打断；即便Ready刚写出，失效已取消生命周期，不能服务后续请求。用闸门验证Bind和Ready前后的取消。
5. Session只关闭自己拥有的response Body和绑定，不Close共享Gateway。Guest入口等待设备/HTTP任务。Read必须先交付n>0数据再处理错误；Flush错误显式检查。
6. 原shell取消断言不充分，round-4仅作为双向传输/隔离初步证据，不宣称Close验证完成。新增可丢弃Go设备程序直接复用device.Conn，单一FD持有，Close并等待实际写goroutine；另测ReadDeadline，Host堵塞写deadline，最后关闭后再观测Host。所有旧失败保留。

## 实现期间装配细化

Host Ready后Guest先绑定127.0.0.1:18080，再回复同一Ready(id=0)；Host只在确认后接收Request，避免固定程序早于监听器启动。该双向确认不承载身份，单帧仍受2秒预算。Guest非root客户端的模式由镜像固定PTY命令选择（普通、取消、慢上传、慢读），没有动态上游或通用网络。

并发拒绝路径设置Connection: close与2秒写deadline，不排空第二请求未完成body。正常上传按每次Read刷新2秒deadline、10秒总预算；尚未发送proxy前超时只结束本地HTTP。Bridge.Close与HTTP handler登记共锁，服务结束等待已登记任务及其取消回调。

共享QEMU后端的TCG机器参数调整：本机内核6.8/QEMU8.2.2的精简ACPI无PMtimer；TSC校准偶发失败后APIC校准等待无法推进的jiffies。PIT原本已存在，仅pit=on无效；单线程调度仍复现。icount导致60秒预算提前结束，已撤销。最终TCG明确关闭ACPI，使用传统PIT/MP表启动；KVM不变，不改变VM契约、vCPU数、网络策略或运行时钟。此修改影响全部TCG使用者，须验证VM契约和旧RPC/PTY，以及真实短deadline和60秒预算。
