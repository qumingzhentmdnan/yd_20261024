#!/bin/sh
# 固定白名单测试工作负载，不执行 Host 传入的任意 shell 命令。
stty -echo
printf 'WORKLOAD_READY uid=%s pid=%s\n' "$(id -u)" "$$"
while IFS= read -r action; do
    case "$action" in
        echo) printf 'ECHO_OK\n' ;;
        size) stty size ;;
        burst) head -c 33554432 /dev/zero; printf 'BURST_DONE\n' ;;
        exit7) printf 'EXIT_TAIL_READY\n'; exit 7 ;;
        child) sleep 600 & printf 'CHILD_PID:%s\n' "$!"; wait ;;
        ignoreterm) trap '' TERM; printf 'IGNORING_TERM\n'; sleep 600 & wait ;;
        stubbornchild)
            /bin/sh -c 'trap "" TERM HUP; printf "STUBBORN_CHILD_READY\n"; sleep 600 & wait' &
            wait
            ;;
        *) printf 'UNKNOWN_ACTION\n' ;;
    esac
done
