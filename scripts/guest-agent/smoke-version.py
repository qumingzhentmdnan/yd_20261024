"""在独立 TCG VM 以 UID1000 检查真实 CLI 的版本与动态依赖，不发供应商请求。"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess


# main 复制生产镜像到独立输出，只替换验证 init，不修改生产镜像或共享素材。
def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--race', action='store_true', help='为并发假上游检查器启用 Go race，需 CGO/gcc')
    parser.add_argument('--protocol', action='store_true', help='额外运行 Guest localhost 假上游协议检查，不连接供应商')
    parser.add_argument('--image', type=Path, required=True)
    parser.add_argument('--assets', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--toolchain', type=Path, required=True)
    args = parser.parse_args()
    package = json.loads((args.image / 'build.json').read_text())['package']
    agent, version = package['agent'], package['version']
    args.output.mkdir(parents=True, exist_ok=False)
    output = args.output.resolve()
    root = output / 'rootfs'
    shutil.copytree(args.image / 'rootfs', root, symlinks=True)
    (root / 'init').write_text('''#!/bin/sh
export PATH=/bin:/usr/bin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
chmod 1777 /tmp
chown 1000:1000 /home/agent
chmod 700 /home/agent
printf 'SMOKE_UID='
su -s /bin/sh agent -c 'id -u'
su -s /bin/sh agent -c 'env -i HOME=/home/agent PATH=/usr/bin:/bin /opt/agents/AGENT/bin/AGENT --version'
status=$?
printf 'CLI_VERSION_EXIT=%s\\n' "$status"
su -s /bin/sh agent -c 'env -i HOME=/home/agent PATH=/usr/bin:/bin /bin/bash -c "printf BASH_READY"'
printf '\\nSMOKE_DONE\\n'
reboot -f
'''.replace('AGENT', agent))
    if args.protocol:
        repo = Path(__file__).resolve().parents[2]
        subprocess.run(['go', 'build'] + (['-race'] if args.race else []) + ['-o', str(root / 'bin/protocol-smoke'), './scripts/guest-agent/protocol-smoke'], cwd=repo, check=True, env=dict(os.environ, CGO_ENABLED='1' if args.race else '0', GOOS='linux', GOARCH='amd64'))
        init = (root / 'init').read_text().replace('reboot -f', f'ip link set lo up\nchown 0:0 /home/agent\nchmod 755 /home/agent\n/bin/protocol-smoke {agent}\nprotocol_status=$?\nprintf "PROTOCOL_EXIT=%s\\n" "$protocol_status"\nreboot -f')
        (root / 'init').write_text(init)
    with (output / 'initramfs.cpio.gz').open('wb') as target:
        subprocess.run(['bash', '-o', 'pipefail', '-c', 'find . -print0 | cpio --null -o --format=newc --owner=0:0 2>/dev/null | gzip -1'], cwd=root, stdout=target, check=True)
    tools = args.toolchain.resolve()
    overlay = output / 'workspace.qcow2'
    subprocess.run([str(tools / 'qemu-img'), 'create', '-f', 'qcow2', '-F', 'raw', '-b', str((args.assets / 'workspace-base.raw').resolve()), str(overlay)], check=True)
    command = [str(tools / 'qemu-system-x86_64'), '-machine', 'microvm,acpi=off', '-accel', 'tcg', '-cpu', 'max', '-m', '2048M', '-smp', '2', '-nodefaults', '-no-user-config', '-display', 'none', '-no-reboot', '-kernel', str((args.assets / 'guest-kernel/boot/vmlinuz-6.8.0-147-generic').resolve()), '-initrd', str(output / 'initramfs.cpio.gz'), '-append', 'console=ttyS0 reboot=t panic=-1', '-serial', 'file:' + str(output / 'console.log'), '-qmp', 'unix:' + str(output / 'qmp.sock') + ',server=on,wait=off', '-drive', 'id=workspace,file=' + str(overlay) + ',format=qcow2,if=none', '-device', 'virtio-blk-device,drive=workspace', '-nic', 'none']
    result = subprocess.run(command, timeout=240)
    console = (output / 'console.log').read_text(errors='replace')
    passed = result.returncode == 0 and version in console and 'CLI_VERSION_EXIT=0' in console and 'SMOKE_UID=1000' in console and 'BASH_READY' in console and 'SMOKE_DONE' in console
    if args.protocol:
        passed = passed and 'PROTOCOL_SMOKE_OK agent=' + agent in console and 'PROTOCOL_SMOKE_ERROR' not in console and 'PROTOCOL_EXIT=0' in console and 'WARNING: DATA RACE' not in console and 'tool_roundtrip=true' in console and 'TOOL_FILE_VERIFIED agent=' + agent in console
    (output / 'result.json').write_text(json.dumps(dict(agent=agent, version=version, passed=passed, command=command, scope='真实 TCG CLI --version、Bash' + (' 与 Guest localhost 假上游协议检查；未连接 Host Gateway 或真实供应商' if args.protocol else '；未调用供应商')), indent=2) + '\n')
    print(console[-6000:])
    if not passed:
        raise RuntimeError('Guest CLI version smoke failed')


if __name__ == '__main__':
    main()
