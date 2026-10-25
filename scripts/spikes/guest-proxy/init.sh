#!/bin/sh
# 本探针只验证独立设备风险，不使用生产协议。
export PATH=/bin:/usr/bin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
for entry in /sys/class/virtio-ports/*; do
 name=$(cat "$entry/name")
 ln -s "/dev/${entry##*/}" "/dev/$name"
done
echo PROXY_SPIKE_BOOTED
/bin/deviceprobe
while :; do sleep 60; done
