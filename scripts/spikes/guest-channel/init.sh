#!/bin/sh
# 本文件仅用于设备实验，不是正式 guestd、RPC 或 Runner。
export PATH=/bin:/usr/bin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
chmod 1777 /tmp
chown 1000:1000 /home/agent
ip link set lo up
for entry in /sys/class/virtio-ports/*; do
    name=$(cat "$entry/name")
    ln -s "/dev/${entry##*/}" "/dev/$name"
done
echo CHANNEL_SPIKE_BOOTED

# Host 连接后发送命令；EOF 后重新打开，以观察设备是否允许重新连接。
generation=0
while :; do
    exec 3<>/dev/safeagent.control
    while IFS= read -r line <&3; do
        case "$line" in
            ping) echo pong >&3 ;;
            terminal)
                # 固定测试 shell；本实验只创建一个 PTY，编号固定为 0。
                (
                    socat FILE:/dev/safeagent.terminal EXEC:'/bin/probe-session',pty,setsid,ctty,stderr
                    echo "TERMINAL_EXIT:$?" >&3
                    echo "SESSION_EXIT:$(cat /session-exit)" >&3
                ) &
                echo TERMINAL_STARTED >&3
                ;;
            resize)
                stty -F /dev/pts/0 rows 40 cols 120
                echo "RESIZE_EXIT:$?" >&3
                ;;
            flood)
                # 固定大输出用于填满设备缓冲；不接受任意命令参数。
                (
                    head -c 33554432 /dev/zero > /dev/safeagent.proxy
                    echo "FLOOD_EXIT:$?" >&3
                ) &
                echo FLOOD_STARTED >&3
                ;;
            generation) echo "GENERATION:$generation" >&3 ;;
            *) echo UNKNOWN_COMMAND >&3 ;;
        esac
    done
    exec 3>&-
    generation=$((generation + 1))
    echo "CONTROL_EOF:$generation"
    # 无 Host 时读立即 EOF；仅在此探测脚本中节流，正式实现不能忙循环。
    sleep 1
done
