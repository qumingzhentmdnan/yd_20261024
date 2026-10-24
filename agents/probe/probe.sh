#!/bin/sh
# 固定探针只输出版本和独立参数，不执行配置里的文本或发起模型调用。
printf '%s\n' 'manifest-probe-v1'
printf '%s\n' "$@"
