#!/bin/sh
# 固定探针读取实际配置，记录身份与工作目录，不执行配置内的文本。
set -eu
printf '%s\n' 'manifest-probe-v1'
printf 'PROBE_UID='; id -u
printf 'PROBE_CWD='; pwd
printf 'PROBE_HOME=%s\n' "$HOME"
printf 'PROBE_ARG=%s\n' "$@"
test "$1" = --config
test "$2" = "$PROBE_CONFIG"
printf 'PROBE_JSON_BEGIN\n'
cat "$PROBE_CONFIG"
printf 'PROBE_TOML_BEGIN\n'
cat "$HOME/.config/probe/settings.toml"
printf 'PROBE_FILES\n'
stat -c '%a %u %g' "$PROBE_CONFIG" "$HOME/.config/probe/settings.toml"
printf 'PROBE_DONE\n'
