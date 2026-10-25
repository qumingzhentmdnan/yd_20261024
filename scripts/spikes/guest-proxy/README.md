# 独立 proxy 设备探针

从本 worktree 根执行（必须已有本机素材）：

```sh
source /home/yedong/workspace/sandbox/artifacts/toolchain/activate.sh
python3 scripts/spikes/guest-proxy/probe.py --assets /home/yedong/workspace/sandbox/artifacts/validation --output artifacts/guest-proxy-spike/new-round
```

输出目录必须不存在。仅为 Linux/amd64 microvm/TCG 设备风险探针，不是生产协议或真实 Bridge 验收。

本轮 round-1 因 Guest 首次打开 control 早于 Host 而读 EOF，PID1退出；round-2 业务探针通过，QEMU退出时未消费数据使proxy返回reset，原EOF断言失败；round-3内核早期启动30秒超时；round-4通过。原始日志均保留。round-4双向65536字节二进制一致，proxy背压时control/terminal可用，Guest阻塞写可取消，Host阻塞写0.318秒deadline退出（已写4259840字节）。Guest写者关闭未产生Host EOF；QEMU退出后control/terminal EOF、proxy reset，均有界结束。基础盘不变，自有进程已回收。

不支持或验证控制重连。脚本首次连接等待只服务于可丢弃原型，不导入生产包。

设计评审指出 shell kill/wait 未跟踪实际 head，且 close 命令未关闭FD，因此 round-4 的取消/关闭不作为充分证据。改 Go deviceprobe 直接复用 device.Conn；round-5/6暴露字符设备短写，补完整写循环；round-7全部通过，额外验证ReadDeadline、Close唯一FD并等待实际写goroutine错误退出。正式codec必须处理短写。round-7日志为后续设计依据，旧失败不覆盖。
