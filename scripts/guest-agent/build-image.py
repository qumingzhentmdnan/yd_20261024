"""构建固定登记工作负载的正式 Guest 验证 initramfs；不修改输入素材。"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess


# digest 计算镜像登记文件的真实摘要。
def digest(data):
    return 'sha256:' + hashlib.sha256(data).hexdigest()


# main 从显式素材复制独立 rootfs 并生成受控登记，不接受运行时命令。
def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--assets', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--race', action='store_true')
    parser.add_argument('--proxy', action='store_true')
    parser.add_argument('--workspace', action='store_true')
    parser.add_argument('--manifest', action='store_true')
    args = parser.parse_args()
    if args.manifest and not args.workspace:
        parser.error('--manifest requires --workspace for real imported project')
    args.output.mkdir(parents=True, exist_ok=False)
    root = args.output.resolve() / 'rootfs'
    shutil.copytree(args.assets.resolve() / 'rootfs', root, symlinks=True)
    repo = Path(__file__).resolve().parents[2]
    environment = dict(os.environ, CGO_ENABLED='1' if args.race else '0', GOOS='linux', GOARCH='amd64')
    command = ['go', 'build'] + (['-race'] if args.race else []) + ['-o', str(root / 'bin/guest-agent'), './cmd/guest-agent']
    subprocess.run(command, cwd=repo, env=environment, check=True)
    workload = b'''#!/bin/sh
trap 'printf "EXIT_TAIL_READY\\n"; exit 0' TERM
printf 'READY uid='; id -u
while IFS= read -r line; do
 case "$line" in
  size) stty size ;;
  flood) while :; do printf '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'; done ;;
  *) printf 'ECHO:%s\\n' "$line" ;;
 esac
done
while :; do sleep 1; done
'''
    if args.proxy:
        proxy_command = ['go', 'build'] + (['-race'] if args.race else []) + ['-o', str(root / 'bin/fixed-http'), './cmd/guest-http-probe']
        subprocess.run(proxy_command, cwd=repo, env=environment, check=True)
        # 工作负载仍只接受固定命令，不开放任意HTTP目标或脚本执行。
        workload = workload.replace(b'  size)', b'  http) /bin/fixed-http ;;\n  http-cancel) /bin/fixed-http cancel ;;\n  http-upload) /bin/fixed-http upload ;;\n  http-slow) /bin/fixed-http slow ;;\n  http-abort) /bin/fixed-http abort ;;\n  http-total) /bin/fixed-http total ;;\n  size)')
    if args.workspace:
        workload = workload.replace(b'  size)', b'  workspace) pwd; id -u; cat imported.txt; sha256sum binary.bin; test -x run.sh && ./run.sh ;;\n  size)')
    config = b'fixed-nonroot-pty-v01\n'
    workspace = b'fixed-workspace-v01\n'
    fixed = root / 'etc/safeagent'
    fixed.mkdir(parents=True, exist_ok=True)
    (root / 'bin/fixed-workload').write_bytes(workload)
    (root / 'bin/fixed-workload').chmod(0o755)
    if args.manifest:
        config = b'manifest-prepare-v1\nhomeRoot=/home/agent\nworkspace=/workspace/project\nmodelGatewayURL=http://127.0.0.1:18080\n'
        # 安装内置 probe，登记摘要与 Go manifest.Catalog 的 fingerprint 保持一致。
        probe_dir = root / 'opt/agents/probe/bin'
        probe_dir.mkdir(parents=True, exist_ok=True)
        probe = repo / 'agents/probe/probe.sh'
        shutil.copyfile(probe, probe_dir / 'probe')
        (probe_dir / 'probe').chmod(0o755)
        manifest_raw = (repo / 'agents/probe/agent.yaml').read_bytes()
        template_raw = (repo / 'agents/probe/templates/settings.json').read_bytes()
        record = {'Manifest': digest(manifest_raw), 'Executable': digest(probe.read_bytes()), 'Templates': {'templates/settings.json': digest(template_raw)}}
        manifest_digest = digest(json.dumps(record, separators=(',', ':')).encode())
        for name, value in [('manifest', manifest_raw), ('config', config), ('workspace', workspace)]:
            (fixed / name).write_bytes(value)
        content = dict(preparedID='registered', manifestRef='probe@1.0.0', manifestDigest=manifest_digest, configDigest=digest(config), workspaceRef='fixed-workspace', workspaceDigest=digest(workspace))
        registration = dict(manifest='/etc/safeagent/manifest', configuration='/etc/safeagent/config', workspace='/etc/safeagent/workspace', executable='/opt/agents/probe/bin/probe', directory='/home/agent', content=content)
    else:
        for name, value in [('manifest', workload), ('config', config), ('workspace', workspace)]:
            (fixed / name).write_bytes(value)
        content = dict(preparedID='registered', manifestRef='fixed-v01', manifestDigest=digest(workload), configDigest=digest(config), workspaceRef='fixed-workspace', workspaceDigest=digest(workspace))
        registration = dict(manifest='/etc/safeagent/manifest', configuration='/etc/safeagent/config', workspace='/etc/safeagent/workspace', executable='/bin/fixed-workload', directory='/home/agent', content=content)
    registration['mode'] = 'manifest' if args.manifest else 'fixed'
    if args.workspace:
        (fixed / 'workspace-enabled').write_text('import-v01\n')
        (root / 'workspace').mkdir(exist_ok=True)
        (root / 'workspace').chmod(0o755)
    if args.proxy:
        (fixed / 'proxy-enabled').write_text('model\n')
    (fixed / 'registration.json').write_text(json.dumps(registration))
    (args.output / 'content.json').write_text(json.dumps(content, indent=2) + '\n')
    (root / 'init').write_text('''#!/bin/sh
# 验证镜像仅初始化固定设备和固定登记，不自动重启 guest-agent。
export PATH=/bin:/usr/bin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /run/safeagent
mount -t devpts devpts /dev/pts
chmod 700 /run/safeagent
chmod 1777 /tmp
chown 0:0 /home/agent
chmod 755 /home/agent
ip link set lo up
for entry in /sys/class/virtio-ports/*; do
 name=$(cat "$entry/name")
 ln -s "/dev/${entry##*/}" "/dev/$name"
 chmod 600 "/dev/${entry##*/}"
done
# 固定验证结果留在串口，控制失效后仍能观察Guest客户端断言。
/bin/guest-agent
# 保留 boot 标记和工作负载状态，由 Host 回收 VM。
while :; do sleep 60; done
''')
    if not args.manifest:
        (root / 'init').write_text((root / 'init').read_text().replace('chown 0:0 /home/agent', 'chown 1000:1000 /home/agent'))
    if args.proxy:
        init = (root / 'init').read_text()
        observer = ''.join('(while [ ! -f /tmp/http-' + name + '-result ]; do sleep 0.1; done; cat /tmp/http-' + name + '-result) &\n' for name in ('abort', 'stop'))
        (root / 'init').write_text(init.replace('/bin/guest-agent\n', observer + '/bin/guest-agent\n'))
    (root / 'init').chmod(0o755)
    with (args.output / 'initramfs.cpio.gz').open('wb') as target:
        subprocess.run(['bash', '-o', 'pipefail', '-c', 'find . -print0 | cpio --null -o --format=newc --owner=0:0 2>/dev/null | gzip -1'], cwd=root, stdout=target, check=True)
    (args.output / 'build.json').write_text(json.dumps(dict(race=args.race, manifest=args.manifest, command=command, initramfsDigest=digest((args.output / 'initramfs.cpio.gz').read_bytes())), indent=2) + '\n')


if __name__ == '__main__':
    main()
