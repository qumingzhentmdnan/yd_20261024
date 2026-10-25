"""只下载并校验锁定官方包，不运行安装器或 CLI，不访问宿主认证。"""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess


# main 将选中 CLI 和运行依赖下载到显式目录，逐个核对仓库锁定摘要。
def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--agent', choices=['codex', 'claude'], required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    lock = json.loads((Path(__file__).resolve().parents[2] / 'agents/packages.lock.json').read_text())
    args.output.mkdir(parents=True, exist_ok=True)
    for package in [lock['agents'][args.agent], *lock['runtime']]:
        target = args.output / package['archive']
        if not target.exists():
            partial = target.with_suffix(target.suffix + '.partial')
            subprocess.run(['curl', '-q', '--http1.1', '--fail', '--location', '--retry', '5', '--output', str(partial), package['url']], check=True)
            if hashlib.sha256(partial.read_bytes()).hexdigest() != package['sha256']:
                raise ValueError('package digest mismatch: ' + package['archive'])
            partial.rename(target)
        if hashlib.sha256(target.read_bytes()).hexdigest() != package['sha256']:
            raise ValueError('package digest mismatch: ' + package['archive'])
        print(package['archive'], package['sha256'])


if __name__ == '__main__':
    main()
