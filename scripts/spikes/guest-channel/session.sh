#!/bin/sh
# 测试专用：单独记录直接子进程返回码，避免 socat 的状态覆盖它。
/bin/su -s /bin/sh agent
status=$?
echo "$status" > /session-exit
exit "$status"
