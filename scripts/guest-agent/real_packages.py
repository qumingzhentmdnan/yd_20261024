"""从已核验离线包安装单个原生 CLI 和明确的运行依赖。"""
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile


# verified 只读取摘要与仓库锁一致的构建输入，不执行包内程序。
def verified(directory, entry):
    data = (directory / entry['archive']).read_bytes()
    if hashlib.sha256(data).hexdigest() != entry['sha256']:
        raise ValueError('package digest mismatch: ' + entry['archive'])
    return data


# write_file 替换镜像内 BusyBox 链接时先解除链接，避免覆盖其共享程序。
def write_file(root, name, data, mode=0o755):
    target = root / name.lstrip('/')
    target.parent.mkdir(parents=True, exist_ok=True)
    if target.is_symlink():
        target.unlink()
    target.write_bytes(data)
    target.chmod(mode)


# install 将锁定程序及运行库写入独立镜像，并返回可审计的实际文件摘要。
def install(repo, root, directory, agent):
    lock = json.loads((repo / 'agents/packages.lock.json').read_text())
    package = lock['agents'][agent]
    data = verified(directory, package)
    if agent == 'codex':
        with tarfile.open(fileobj=io.BytesIO(data)) as archive:
            executable = archive.extractfile(package['executable']).read()
            rg = archive.extractfile(package['rg']).read()
        if hashlib.sha256(rg).hexdigest() != package['sha256Rg']:
            raise ValueError('rg digest mismatch')
        write_file(root, '/usr/bin/rg', rg)
    else:
        executable = data
    expected = (repo / f'agents/{agent}/executable.sha256').read_text().strip()
    if 'sha256:' + hashlib.sha256(executable).hexdigest() != expected or expected != 'sha256:' + package['sha256Executable']:
        raise ValueError('executable digest mismatch')
    write_file(root, f'/opt/agents/{agent}/bin/{agent}', executable)
    # 只安装固定库文件，dpkg 只解包，不执行 maintainer scripts。
    runtime_files = {
        'libc6': ['usr/lib/x86_64-linux-gnu/' + name for name in ['libc.so.6', 'libm.so.6', 'libpthread.so.0', 'libdl.so.2', 'librt.so.1', 'ld-linux-x86-64.so.2']],
        'bash': ['usr/bin/bash'],
        'libtinfo6': ['usr/lib/x86_64-linux-gnu/libtinfo.so.6.4'],
    }
    records = {}
    for entry in lock['runtime']:
        verified(directory, entry)
        with tempfile.TemporaryDirectory(prefix='safeagent-package-') as temporary:
            subprocess.run(['dpkg-deb', '--extract', str((directory / entry['archive']).resolve()), temporary], check=True)
            for source in runtime_files[entry['package']]:
                content = (Path(temporary) / source).read_bytes()
                destination = '/' + source
                if destination.startswith('/usr/lib/'):
                    destination = destination.removeprefix('/usr')
                if destination.endswith('ld-linux-x86-64.so.2'):
                    destination = '/lib64/ld-linux-x86-64.so.2'
                if destination.endswith('libtinfo.so.6.4'):
                    destination = '/lib/x86_64-linux-gnu/libtinfo.so.6'
                write_file(root, destination, content)
                records[destination] = 'sha256:' + hashlib.sha256(content).hexdigest()
    # Bash 工具使用固定 /bin/bash，基础镜像的 /usr/bin 可能不是合并布局。
    write_file(root, '/bin/bash', (root / 'usr/bin/bash').read_bytes())
    return dict(agent=agent, version=package['version'], model=package['model'], executableDigest=expected, source=package['url'], packageDigest='sha256:' + package['sha256'], runtimeFiles=records)
