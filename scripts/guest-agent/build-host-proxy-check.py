"""构建明确标注的测试副本，真实CLI通过正式Guest/Host代理调用宿主假TLS服务。"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess


# digest 绑定测试副本实际登记的固定启动脚本。
def digest(data):
    return 'sha256:' + hashlib.sha256(data).hexdigest()


# main 保留生产镜像不变，只在独立副本登记非交互检查器。
def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--image', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--race', action='store_true')
    args = parser.parse_args()
    source = args.image.resolve()
    build = json.loads((source / 'build.json').read_text())
    agent = build['package']['agent']
    if agent not in ('codex', 'claude'):
        raise ValueError('requires real CLI image')
    args.output.mkdir(parents=True, exist_ok=False)
    output = args.output.resolve()
    root = output / 'rootfs'
    shutil.copytree(source / 'rootfs', root, symlinks=True)
    repo = Path(__file__).resolve().parents[2]
    command = ['go', 'build'] + (['-race'] if args.race else []) + ['-o', str(root / 'bin/host-proxy-check'), './scripts/guest-agent/host-proxy-check']
    subprocess.run(command, cwd=repo, env=dict(os.environ, CGO_ENABLED='1' if args.race else '0', GOOS='linux', GOARCH='amd64'), check=True)
    workload = f'#!/bin/sh\nexec /bin/host-proxy-check {agent}\n'.encode()
    (root / 'bin/fixed-workload').write_bytes(workload)
    (root / 'bin/fixed-workload').chmod(0o755)
    fixed = root / 'etc/safeagent'
    (fixed / 'manifest').write_bytes(workload)
    registration = json.loads((fixed / 'registration.json').read_text())
    content = registration['content']
    content['manifestRef'] = 'fixed-host-proxy-check'
    content['manifestDigest'] = digest(workload)
    registration.update(mode='fixed', executable='/bin/fixed-workload', directory='/workspace/project')
    (fixed / 'registration.json').write_text(json.dumps(registration) + '\n')
    (output / 'content.json').write_text(json.dumps(content, indent=2) + '\n')
    (root / 'init').write_text((root / 'init').read_text().replace('chown 0:0 /home/agent', 'chown 1000:1000 /home/agent'))
    with (output / 'initramfs.cpio.gz').open('wb') as target:
        subprocess.run(['bash', '-o', 'pipefail', '-c', 'find . -print0 | cpio --null -o --format=newc --owner=0:0 2>/dev/null | gzip -1'], cwd=root, stdout=target, check=True)
    (output / 'build.json').write_text(json.dumps(dict(testOnly=True, agent=agent, source=str(source), sourceInitramfsDigest=build['initramfsDigest'], command=command, initramfsDigest=digest((output / 'initramfs.cpio.gz').read_bytes())), indent=2) + '\n')


if __name__ == '__main__':
    main()
