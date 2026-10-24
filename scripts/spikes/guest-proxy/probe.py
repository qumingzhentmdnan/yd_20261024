"""真实 QEMU 设备探测；不实现正式线协议，也不读取凭据。"""

import argparse
import hashlib
import json
from pathlib import Path
import select
import shutil
import socket
import subprocess
import tempfile
import time


# connect 连接本轮 VM 的专属 Unix 端点。
def connect(directory, name):
    stream = socket.socket(socket.AF_UNIX)
    stream.settimeout(5)
    stream.connect(str(directory / (name + '.sock')))
    return stream


# receive 在给定期限内收集设备数据，限制单轮收集大小。
def receive(stream, duration=0.3, limit=1048576):
    deadline = time.monotonic() + duration
    chunks = []
    size = 0
    while size < limit:
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not select.select([stream], [], [], remaining)[0]:
            break
        chunk = stream.recv(min(65536, limit - size))
        if not chunk:
            break
        chunks.append(chunk)
        size += len(chunk)
    return b''.join(chunks)


# expect 收集直到预期字节出现，超时时保留实际数据以便诊断。
def expect(stream, marker, timeout=5):
    deadline = time.monotonic() + timeout
    data = b''
    while marker not in data and time.monotonic() < deadline:
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not select.select([stream], [], [], remaining)[0]:
            break
        chunk = stream.recv(65536)
        if not chunk:
            break
        data += chunk
    assert marker in data, (marker, data[-4096:])
    return data


# digest 计算基础盘摘要，验证本轮未改写共享输入。
def digest(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


# drain_eof 在固定期限内排空已缓存数据，确认最终读到 EOF。
def drain_eof(stream):
    deadline = time.monotonic() + 2
    remaining_bytes = 0
    while True:
        remaining = deadline - time.monotonic()
        assert remaining > 0 and select.select([stream], [], [], remaining)[0], '等待设备 EOF 超时'
        try:
            chunk = stream.recv(4096)
        except ConnectionResetError:
            return {'bytes': remaining_bytes, 'termination': 'reset'}
        if not chunk:
            return remaining_bytes
        remaining_bytes += len(chunk)


# main 创建独立镜像与端点，完成探测并仅回收自己启动的 QEMU。
def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--assets', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    assets = args.assets.resolve()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    source = Path(__file__).resolve().with_name('init.sh')
    results = {'scope': 'Linux/amd64/microvm/显式TCG，设备探测，不是协议验收', 'checks': {}, 'passed': False}
    streams = []
    process = None
    qfile = None
    base = assets / 'workspace-base.raw'
    before = digest(base)
    directory = Path(tempfile.mkdtemp(prefix='sa-channel-', dir='/tmp'))
    try:
        # 复制本机验证素材，不改共享 rootfs、initramfs 和基础盘。
        rootfs = output / 'rootfs'
        shutil.copytree(assets / 'rootfs', rootfs, symlinks=True)
        shutil.copyfile(source, rootfs / 'init')
        (rootfs / 'init').chmod(0o755)
        subprocess.run(['go', 'build', '-o', str(rootfs / 'bin/deviceprobe'), './scripts/spikes/guest-proxy/device'], check=True)
        with (output / 'initramfs.cpio.gz').open('wb') as target:
            subprocess.run(['bash', '-o', 'pipefail', '-c', 'find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip -1'], cwd=rootfs, stdout=target, check=True)
        subprocess.run(['qemu-img', 'create', '-f', 'qcow2', '-F', 'raw', '-b', str(base), str(output / 'overlay.qcow2')], check=True, capture_output=True)
        command = ['qemu-system-x86_64', '-machine', 'microvm', '-accel', 'tcg', '-cpu', 'max', '-m', '512M', '-smp', '2', '-nodefaults', '-no-user-config', '-display', 'none', '-no-reboot', '-nic', 'none', '-kernel', str(assets / 'guest-kernel/boot/vmlinuz-6.8.0-147-generic'), '-initrd', str(output / 'initramfs.cpio.gz'), '-append', 'console=ttyS0 reboot=t panic=-1', '-serial', 'file:' + str(output / 'console.log'), '-qmp', 'unix:' + str(directory / 'qmp.sock') + ',server=on,wait=off', '-device', 'virtio-serial-device,id=serial0', '-drive', 'id=workspace,file=' + str(output / 'overlay.qcow2') + ',format=qcow2,if=none', '-device', 'virtio-blk-device,drive=workspace']
        for number, name in enumerate(('control', 'terminal', 'proxy'), 1):
            command += ['-chardev', 'socket,id=' + name + ',path=' + str(directory / (name + '.sock')) + ',server=on,wait=off', '-device', f'virtserialport,bus=serial0.0,nr={number},chardev={name},name=safeagent.{name}']
        (output / 'qemu-argv.json').write_text(json.dumps(command, indent=2) + '\n')
        with (output / 'qemu-stderr.log').open('wb') as stderr:
            process = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=stderr)
        deadline = time.monotonic() + 30
        while not (output / 'console.log').exists() or b'PROXY_SPIKE_BOOTED' not in (output / 'console.log').read_bytes():
            assert process.poll() is None, (output / 'qemu-stderr.log').read_text()
            assert time.monotonic() < deadline, '等待 Guest 启动超时'
            time.sleep(0.05)
        control = connect(directory, 'control')
        terminal = connect(directory, 'terminal')
        proxy = connect(directory, 'proxy')
        streams += [control, terminal, proxy]
        control.sendall(b'terminal\n')
        expect(control, b'TERMINAL_STARTED')
        payload = bytes(range(256)) * 256
        control.sendall(b'echo\n')
        proxy.sendall(payload)
        actual = b''
        deadline = time.monotonic() + 5
        while len(actual) < len(payload):
            assert time.monotonic() < deadline, '二进制接收总超时'
            chunk = proxy.recv(len(payload) - len(actual))
            assert chunk, '二进制接收提前EOF'
            actual += chunk
        assert actual == payload
        expect(control, b'ECHO_DONE')
        results['checks']['bidirectional_binary_bytes'] = len(payload)
        control.sendall(b'read-deadline\n')
        expect(control, b'READ_DEADLINE')
        results['checks']['device_read_deadline'] = True
        control.sendall(b'flood\n')
        expect(control, b'FLOOD_STARTED')
        assert select.select([proxy], [], [], 5)[0]
        control.sendall(b'ping\n')
        expect(control, b'pong')
        terminal.sendall(b'TERMINAL_DURING_PROXY_BACKPRESSURE\n')
        expect(terminal, b'TERMINAL_DURING_PROXY_BACKPRESSURE')
        results['checks']['control_terminal_during_proxy_stall'] = True
        # Guest不再读取proxy；Host写满后必须按socket deadline退出。
        proxy.settimeout(0.3)
        sent = 0
        started = time.monotonic()
        try:
            while True:
                sent += proxy.send(bytes(65536))
        except socket.timeout:
            results['checks']['host_stalled_writer_deadline'] = {'bytes': sent, 'seconds': time.monotonic() - started}
        control.sendall(b'ping\n')
        expect(control, b'pong')
        control.sendall(b'cancel\n')
        expect(control, b'CANCELED_CLOSED')
        results['checks']['device_close_unblocks_writer'] = True
        proxy.settimeout(2)
        receive(proxy, duration=0.5, limit=67108864)
        eof = False
        if select.select([proxy], [], [], 0.3)[0]:
            eof = proxy.recv(1) == b''
        results['checks']['host_eof_after_guest_writer_close'] = eof
        # 关闭 VMM 后，所有 Host 设备读均须在有界时间内看到 EOF。
        qmp = connect(directory, 'qmp')
        streams.append(qmp)
        qfile = qmp.makefile('rwb', buffering=0)
        json.loads(qfile.readline())
        for request_id, method in enumerate(('qmp_capabilities', 'quit'), 1):
            qfile.write((json.dumps({'execute': method, 'id': request_id}) + '\n').encode())
            while True:
                message = json.loads(qfile.readline())
                if message.get('id') == request_id:
                    assert 'return' in message, message
                    break
        process.wait(timeout=10)
        results['checks']['post_exit_remaining_bytes'] = {
            name: drain_eof(stream)
            for name, stream in [('control', control), ('terminal', terminal), ('proxy', proxy)]
        }
        results['checks']['all_ports_terminated_after_vmm_exit'] = True
        results['qemu_returncode'] = process.returncode
        results['checks_completed'] = True
    finally:
        if qfile is not None:
            qfile.close()
        for stream in streams:
            stream.close()
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        results['base_disk_unchanged'] = before == digest(base)
        results['owned_process_reaped'] = process is None or process.poll() is not None
        results['passed'] = results.get('checks_completed', False) and results['base_disk_unchanged'] and results['owned_process_reaped'] and results.get('qemu_returncode') == 0
        (output / 'results.json').write_text(json.dumps(results, ensure_ascii=False, indent=2) + '\n')
        shutil.rmtree(directory)
    assert results['base_disk_unchanged']
    print(json.dumps(results, ensure_ascii=False, indent=2))


if __name__ == '__main__':
    main()
