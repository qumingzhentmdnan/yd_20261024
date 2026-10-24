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


# generation 读取完整的设备重开代次，避免只匹配前缀导致假阳性。
def generation(control):
    control.sendall(b'generation\n')
    response = expect(control, b'\n')
    assert response.startswith(b'GENERATION:'), response
    return int(response.removeprefix(b'GENERATION:').strip())


# drain_eof 在固定期限内排空已缓存数据，确认最终读到 EOF。
def drain_eof(stream):
    deadline = time.monotonic() + 2
    remaining_bytes = 0
    while True:
        remaining = deadline - time.monotonic()
        assert remaining > 0 and select.select([stream], [], [], remaining)[0], '等待设备 EOF 超时'
        chunk = stream.recv(4096)
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
        shutil.copyfile(source.with_name('session.sh'), rootfs / 'bin/probe-session')
        (rootfs / 'bin/probe-session').chmod(0o755)
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
        while not (output / 'console.log').exists() or b'CHANNEL_SPIKE_BOOTED' not in (output / 'console.log').read_bytes():
            assert process.poll() is None, (output / 'qemu-stderr.log').read_text()
            assert time.monotonic() < deadline, '等待 Guest 启动超时'
            time.sleep(0.05)
        control = connect(directory, 'control')
        terminal = connect(directory, 'terminal')
        proxy = connect(directory, 'proxy')
        streams += [control, terminal, proxy]
        control.sendall(b'ping\n')
        expect(control, b'pong\n')
        results['checks']['control_bidirectional'] = True
        control.sendall(b'terminal\n')
        expect(control, b'TERMINAL_STARTED')
        terminal.sendall(b"stty -echo; id; tty; printf 'PTY_%s\\n' READY\n")
        identity = expect(terminal, b'PTY_READY\r\n')
        assert b'uid=1000' in identity and b'/dev/pts/' in identity, identity
        results['checks']['nonroot_pty'] = identity.decode(errors='replace')
        # Resize 由独立控制端触发，不通过终端输入执行 stty 修改尺寸。
        control.sendall(b'resize\n')
        expect(control, b'RESIZE_EXIT:0')
        terminal.sendall(b'stty size\n')
        results['checks']['host_control_resize'] = expect(terminal, b'40 120\r\n').decode(errors='replace')
        terminal.sendall(b"head -c 33554432 /dev/zero; printf 'TTY_FLOOD_%s\\n' DONE\n")
        control.sendall(b'flood\n')
        expect(control, b'FLOOD_STARTED')
        # 两个数据端口均不读，确认有输出等待后再验证控制仍响应。
        assert select.select([terminal], [], [], 5)[0]
        assert select.select([proxy], [], [], 5)[0]
        time.sleep(0.5)
        latencies = []
        for _ in range(20):
            started = time.monotonic()
            control.sendall(b'ping\n')
            response = expect(control, b'pong\n', timeout=2)
            assert b'FLOOD_EXIT:' not in response, '输出已结束，未证明慢读期间控制可用'
            latencies.append(round((time.monotonic() - started) * 1000, 2))
        results['checks']['control_during_two_slow_readers_ms'] = latencies
        # 消费剩余数据，证明背压可恢复，且总字节数不丢失。
        total = 0
        deadline = time.monotonic() + 30
        while total < 33554432:
            total += len(receive(proxy, 0.05))
            assert time.monotonic() < deadline, total
        assert total == 33554432, total
        expect(control, b'FLOOD_EXIT:0')
        results['checks']['proxy_flood_bytes'] = total
        total = 0
        tail = b''
        deadline = time.monotonic() + 30
        while b'TTY_FLOOD_DONE\r\n' not in tail:
            chunk = receive(terminal, 0.05)
            total += chunk.count(b'\x00')
            tail = (tail + chunk)[-4096:]
            assert time.monotonic() < deadline, tail
        assert total == 33554432, total
        results['checks']['terminal_flood_bytes'] = total
        # 非零 shell 退出与设备 EOF 分开观察，不能将 socat 退出码当成 shell 退出码。
        terminal.sendall(b'exit 7\n')
        terminal_exit = expect(control, b'SESSION_EXIT:7\n')
        results['checks']['socat_exit_notification'] = terminal_exit.decode(errors='replace')
        results['checks']['direct_child_exit_code'] = 7
        control.sendall(b'ping\n')
        expect(control, b'pong\n')
        results['checks']['control_after_terminal_process_exit'] = True
        # 重连同一 QEMU socket，观察旧 Guest fd 是否能够继续收到数据。
        generation_before = generation(control)
        control.close()
        time.sleep(1.2)
        control = connect(directory, 'control')
        streams.append(control)
        control.sendall(b'ping\n')
        expect(control, b'pong\n')
        generation_after = generation(control)
        assert generation_after > generation_before, (generation_before, generation_after)
        results['checks']['control_reconnect'] = {'before': generation_before, 'after': generation_after}
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
        results['checks']['all_ports_eof_after_vmm_exit'] = True
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
