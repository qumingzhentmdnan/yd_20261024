#!/bin/sh
# 原型专用 Guest，仅挂载本实验所需设备，不提供网络或宿主共享。
export PATH=/bin:/usr/bin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
chmod 1777 /tmp
chown 1000:1000 /home/agent
for entry in /sys/class/virtio-ports/*; do
    name=$(cat "$entry/name")
    ln -s "/dev/${entry##*/}" "/dev/$name"
    chmod 600 "/dev/${entry##*/}"
done
echo RPC_SPIKE_BOOTED
/bin/probe-guest
echo RPC_SPIKE_GUESTD_EXITED
while :; do sleep 60; done
