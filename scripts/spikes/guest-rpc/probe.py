"""独立 Python Host 验证 Go Guest；仅运行固定工作负载，不是产品 SDK。"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import queue
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import uuid

HEADER = struct.Struct('!4sBBBBII')


# encode_frame 按独立实现编码候选帧，不复用 Go 编解码逻辑。
def encode_frame(kind, stream, value):
    body = value if isinstance(value, bytes) else json.dumps(value, separators=(',', ':'), ensure_ascii=False).encode()
    return HEADER.pack(b'SAGP', 0, 1, kind, 0, len(body), stream) + body


# exact 在真实字节流上读取指定数量，区分完整 EOF 和半帧断开。
def exact(port, size):
    result = bytearray()
    while len(result) < size:
        chunk = port.recv(size - len(result))
        if not chunk:
            raise EOFError(('半帧' if result else 'EOF', len(result), size))
        result.extend(chunk)
    return bytes(result)


# read_frame 校验方向无关的帧头和端口边界，返回原始负载。
def read_frame(port, terminal=False):
    magic, major, minor, kind, flags, size, stream = HEADER.unpack(exact(port, 16))
    assert (magic, major, minor, flags) == (b'SAGP', 0, 1, 0)
    assert size <= (16384 if terminal else 65536)
    assert (stream > 0 and 16 <= kind <= 20) if terminal else (stream == 0 and 1 <= kind <= 4)
    assert kind != 19 or size == 0
    return kind, stream, exact(port, size)


# connect 打开本轮私有端点，所有探测均有读取时限。
def connect(directory, name):
    port = socket.socket(socket.AF_UNIX)
    port.settimeout(5)
    port.connect(str(directory / (name + '.sock')))
    return port


# check_fixtures 校验 Python 编解码与共同黄金字节完全一致。
def check_fixtures():
    fixtures = json.loads((Path(__file__).resolve().parents[3] / 'internal/protocol/testdata/frames.json').read_text())
    for case in fixtures:
        raw = bytes.fromhex(case['hex'])
        if case['valid']:
            assert encode_frame(case['kind'], case['stream'], bytes.fromhex(case['bodyHex'])) == raw, case['name']
        # 解码通过分片 socketpair，复用真实 Host 的读取路径。
        left, right = socket.socketpair()
        left.settimeout(2)
        right.sendall(raw)
        right.shutdown(socket.SHUT_WR)
        try:
            try:
                decoded = read_frame(left, case['terminal'])
            except (AssertionError, EOFError):
                assert not case['valid'], case['name']
            else:
                assert case['valid'], case['name']
                assert decoded == (case['kind'], case['stream'], bytes.fromhex(case['bodyHex']))
        finally:
            left.close()
            right.close()


class Peer:
    # __init__ 记录 Host 确认的 epoch，启动一个控制读取者。
    def __init__(self, port, epoch):
        self.port = port
        self.epoch = epoch
        self.connection = str(uuid.uuid4())
        self.lock = threading.Lock()
        self.requests = {}
        self.next_id = 0
        self.events = queue.Queue()
        self.error = None
        self.reader = threading.Thread(target=self.read_loop, daemon=True)
        self.reader.start()

    # fail 唤醒所有等待者，不能把传输故障伪装为 Guest 业务错误。
    def fail(self, error):
        with self.lock:
            if self.error is None:
                self.error = error
            for state in self.requests.values():
                if not state['delivered'] and not state['abandoned']:
                    state['queue'].put_nowait(error)
                    state['delivered'] = True

    # read_loop 关联响应，识别未知、重复和迟到消息。
    def read_loop(self):
        try:
            while True:
                kind, _, body = read_frame(self.port)
                message = json.loads(body)
                assert message['epoch'] == self.epoch and message['connectionID'] == self.connection
                if kind == 3:
                    self.events.put(message)
                    if message['event'] == 'ConnectionClosed':
                        raise ConnectionError(message['params']['code'])
                    continue
                assert kind == 2 and (('result' in message) != ('error' in message))
                with self.lock:
                    state = self.requests[message['requestID']]
                    assert not state['delivered'], '重复响应'
                    state['delivered'] = True
                    if not state['abandoned']:
                        state['queue'].put_nowait(message)
        except Exception as error:
            self.fail(error)

    # send 原子分配并发送 requestID，避免并发调用在设备上倒序。
    def send(self, method, params=None, timeout_ms=5000, fragments=False, epoch=None, connection=None, abandoned=False):
        with self.lock:
            assert self.error is None, self.error
            self.next_id += 1
            request_id = str(self.next_id)
            self.requests[request_id] = {'queue': queue.Queue(1), 'delivered': False, 'abandoned': abandoned}
            message = {'requestID': request_id, 'epoch': epoch or self.epoch, 'connectionID': connection or self.connection, 'method': method, 'timeoutMs': timeout_ms, 'params': params or {}}
            raw = encode_frame(1, 0, message)
            if fragments:
                for offset in range(0, len(raw), 3):
                    self.port.sendall(raw[offset:offset + 3])
            else:
                self.port.sendall(raw)
            return request_id

    # wait 获取请求唯一响应，业务错误由具体用例断言。
    def wait(self, request_id):
        response = self.requests[request_id]['queue'].get(timeout=6)
        if isinstance(response, BaseException):
            raise response
        return response

    # call 同步调用固定方法。
    def call(self, method, params=None, **options):
        response = self.wait(self.send(method, params, **options))
        assert 'result' in response, response
        return response['result']

    # hello 完成当前逻辑连接握手并核对 challenge。
    def hello(self):
        challenge = str(uuid.uuid4())
        result = self.call('Hello', {'vmRef': 'rpc-spike-vm', 'challenge': challenge}, fragments=True)
        assert result == {'version': '0.1', 'challenge': challenge}, result

    # cancel 只取消目标请求的等待，不暗示已提交进程被回滚。
    def cancel(self, request_id):
        with self.lock:
            self.port.sendall(encode_frame(4, 0, {'requestID': request_id, 'epoch': self.epoch, 'connectionID': self.connection}))

    # abandon 模拟调用方在已提交操作响应前取消本地等待。
    def abandon(self, request_id):
        with self.lock:
            self.requests[request_id]['abandoned'] = True

    # close 主动关闭本地 socket，唤醒全部阻塞读取和请求等待。
    def close(self):
        self.port.shutdown(socket.SHUT_RDWR)
        self.port.close()
        self.reader.join(timeout=2)
        assert not self.reader.is_alive()


# terminal_until 读取有界终端帧直到固定测试标记出现。
def terminal_until(port, stream, marker, timeout=10):
    deadline = time.monotonic() + timeout
    tail = b''
    zeros = 0
    while marker not in tail:
        assert time.monotonic() < deadline, (marker, tail[-1000:])
        kind, actual_stream, body = read_frame(port, True)
        assert kind == 18 and actual_stream == stream, (kind, actual_stream, body)
        zeros += body.count(b'\x00')
        tail = (tail + body)[-65536:]
    return tail, zeros


# bind_terminal 通过控制授权绑定终端，保证 Bound 先于 Data。
def bind_terminal(peer, directory, session_id, stream):
    result = peer.call('AttachTerminal', {'sessionID': session_id, 'streamID': stream})
    port = connect(directory, 'terminal')
    port.sendall(encode_frame(16, stream, {'sessionID': session_id, 'epoch': peer.epoch, 'connectionID': peer.connection, 'bindToken': result['bindToken']}))
    kind, actual, body = read_frame(port, True)
    assert kind == 17 and actual == stream and 'droppedBytes' in json.loads(body)
    return port, result


# wait_exit 从 GetState 读取稳定退出状态，不依赖跨端口事件顺序。
def wait_exit(peer, session_id):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        state = peer.call('GetState')
        if state.get('session', {}).get('sessionID') == session_id and state['session']['exit'] is not None:
            return state['session']['exit']
        time.sleep(0.02)
    raise AssertionError('未观察到固定退出状态')


# sha256 计算共享基础盘摘要。
def sha256(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


# wait_console 使用实验控制台确认 Guest 已处理异常，不将睡眠当作断言。
def wait_console(output, marker, before):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if (output / 'console.log').read_bytes().count(marker) > before:
            return
        time.sleep(0.01)
    raise AssertionError(('未观察到 Guest 诊断', marker))


# close_control 确认当前代次的 Guest 循环已退出并关闭设备，再允许重连。
def close_control(peer, output):
    peer.close()
    wait_console(output, ('CONTROL_CLOSED:' + peer.connection).encode(), 0)


# host_response_checks 用同一 Host 适配器验证重复、未知、迟到响应与多等待者断线。
def host_response_checks(checks):
    for case in ('duplicate', 'unknown', 'late', 'disconnect'):
        left, right = socket.socketpair()
        left.settimeout(3)
        right.settimeout(3)
        peer = Peer(left, 'fixture-epoch')
        try:
            first = peer.send('GetState')
            read_frame(right)
            response = {'requestID': first, 'epoch': peer.epoch, 'connectionID': peer.connection, 'result': {}}
            if case == 'disconnect':
                second = peer.send('GetState')
                read_frame(right)
                right.shutdown(socket.SHUT_RDWR)
                for request_id in (first, second):
                    try:
                        peer.wait(request_id)
                    except EOFError:
                        continue
                    raise AssertionError('断线未唤醒等待者')
                checks['host_adapter_disconnect_wakes_all_waiters'] = True
                continue
            if case == 'late':
                peer.abandon(first)
            if case == 'unknown':
                response['requestID'] = '99'
            right.sendall(encode_frame(2, 0, response))
            if case != 'unknown':
                deadline = time.monotonic() + 2
                while not peer.requests[first]['delivered'] and time.monotonic() < deadline:
                    time.sleep(0.01)
                assert peer.requests[first]['delivered']
            if case == 'late':
                assert peer.error is None and peer.requests[first]['queue'].empty()
                second = peer.send('GetState')
                read_frame(right)
                right.sendall(encode_frame(2, 0, dict(response, requestID=second)))
                assert peer.wait(second)['result'] == {}
                checks['host_adapter_first_late_response_discarded'] = True
            if case != 'unknown':
                right.sendall(encode_frame(2, 0, response))
            deadline = time.monotonic() + 2
            while peer.error is None and time.monotonic() < deadline:
                time.sleep(0.01)
            assert peer.error is not None, case
            checks['host_adapter_reject_' + case + '_response'] = True
        finally:
            peer.close()
            right.close()


# invalid_controls 在真实设备上验证畸形输入和旧连接身份被拒绝。
def invalid_controls(directory, output, epoch, checks):
    cases = {
        'unknown_kind': HEADER.pack(b'SAGP', 0, 1, 255, 0, 0, 0),
        'oversize_header': HEADER.pack(b'SAGP', 0, 1, 1, 0, 65537, 0),
        'half_header': b'SAGP\x00\x01',
        'half_payload': HEADER.pack(b'SAGP', 0, 1, 1, 0, 2, 0) + b'{',
        'duplicate_json_key': encode_frame(1, 0, b'{"requestID":"2","requestID":"3"}'),
    }
    for name in list(cases) + ['duplicate_request_id', 'old_epoch', 'old_connection']:
        peer = Peer(connect(directory, 'control'), epoch)
        try:
            peer.hello()
            waiting = peer.send('StopSession', {'sessionID': 'session-4', 'graceMs': 2000, 'forceAfterTimeout': False})
            before = (output / 'console.log').read_bytes().count(b'CONTROL_PROTOCOL_ERROR')
            if name in cases:
                partial_marker = b'CONTROL_READ_BYTES: ' + (b'6' if name == 'half_header' else b'1') + b'\r\n'
                partial_before = (output / 'console.log').read_bytes().count(partial_marker)
                peer.port.sendall(cases[name])
                if name.startswith('half_'):
                    wait_console(output, partial_marker, partial_before)
                    peer.port.shutdown(socket.SHUT_WR)
            else:
                message = {'requestID': '1' if name == 'duplicate_request_id' else '3', 'epoch': 'old-epoch' if name == 'old_epoch' else epoch, 'connectionID': 'old-connection' if name == 'old_connection' else peer.connection, 'method': 'GetState', 'timeoutMs': 1000, 'params': {}}
                peer.port.sendall(encode_frame(1, 0, message))
            deadline = time.monotonic() + 3
            while peer.error is None and time.monotonic() < deadline:
                time.sleep(0.01)
            expected = (ConnectionError, EOFError) if name.startswith('half_') else (ConnectionError,)
            assert isinstance(peer.error, expected), (name, peer.error)
            try:
                peer.wait(waiting)
            except (ConnectionError, EOFError):
                pass
            else:
                raise AssertionError('异常连接未唤醒已在途请求')
            wait_console(output, b'CONTROL_PROTOCOL_ERROR', before)
            checks['real_channel_reject_' + name] = True
        finally:
            close_control(peer, output)
    peer = Peer(connect(directory, 'control'), epoch)
    try:
        peer.hello()
        assert peer.call('GetState')['session']['exit'] is None
        peer.call('StopSession', {'sessionID': 'session-4', 'graceMs': 50, 'forceAfterTimeout': True})
        checks['real_channel_protocol_close_wakes_pending_without_stopping_session'] = True
    finally:
        close_control(peer, output)


# run_checks 在同一次 boot 中验证业务生命周期和跨连接去重。
def run_checks(directory, output, checks):
    epoch = str(uuid.uuid4())
    peer = Peer(connect(directory, 'control'), epoch)
    terminal = None
    try:
        response = peer.wait(peer.send('GetState'))
        assert response['error']['code'] == 'FAILED_PRECONDITION'
        peer.hello()
        checks['pre_hello_rejected_and_fragmented_hello'] = True
        prepared = peer.call('PrepareAgent', {'operationID': str(uuid.uuid4()), 'manifest': 'probe-v1', 'workspaceDigest': 'sha256:probe-workspace'})
        start = {'operationID': str(uuid.uuid4()), 'sessionID': 'session-1', 'preparedID': prepared['preparedID'], 'workspaceDigest': 'sha256:probe-workspace', 'rows': 24, 'cols': 80}
        first = peer.send('StartSession', start)
        second = peer.send('StartSession', dict(reversed(list(start.items()))))
        a, b = peer.wait(first), peer.wait(second)
        assert a['result'] == b['result'] and peer.call('GetState')['starts'] == 1
        conflict = peer.wait(peer.send('StartSession', dict(start, rows=25)))
        assert conflict['error']['code'] == 'CONFLICT'
        checks['concurrent_start_same_operation_one_process_and_conflict'] = True
        terminal, binding = bind_terminal(peer, directory, 'session-1', 1)
        ready, _ = terminal_until(terminal, 1, b'WORKLOAD_READY uid=1000')
        assert b'uid=1000' in ready
        terminal.sendall(encode_frame(18, 1, b'echo\nsize\n'))
        initial, _ = terminal_until(terminal, 1, b'24 80\r\n')
        assert b'ECHO_OK\r\n' in initial
        peer.call('ResizeTerminal', {'sessionID': 'session-1', 'streamID': 1, 'rows': 40, 'cols': 120})
        terminal.sendall(encode_frame(18, 1, b'size\n'))
        terminal_until(terminal, 1, b'40 120\r\n')
        checks['nonroot_pty_echo_initial_size_and_resize_rpc'] = True
        # 关闭终端并显式 detach，工作负载必须仍可从控制面观察为运行。
        peer.call('DetachTerminal', {'sessionID': 'session-1', 'streamID': 1})
        terminal.close(); terminal = None
        state = peer.call('GetState')
        assert state['session']['exit'] is None
        terminal, _ = bind_terminal(peer, directory, 'session-1', 2)
        terminal.sendall(encode_frame(18, 2, b'echo\n'))
        terminal_until(terminal, 2, b'ECHO_OK\r\n')
        checks['detach_reattach_does_not_stop_process'] = True
        terminal.sendall(encode_frame(18, 2, b'exit7\n'))
        exited = wait_exit(peer, 'session-1')
        assert exited['code'] == 7 and exited['signal'] == ''
        # 退出事件与终端分属两个端口；Fin 前必须收到工作负载尾部。
        exit_tail = b''
        while True:
            kind, actual_stream, body = read_frame(terminal, True)
            assert actual_stream == 2
            if kind == 19:
                break
            assert kind == 18
            exit_tail = (exit_tail + body)[-4096:]
        assert b'EXIT_TAIL_READY\r\n' in exit_tail, exit_tail
        peer.call('DetachTerminal', {'sessionID': 'session-1', 'streamID': 2})
        terminal.close(); terminal = None
        checks['real_exit7_and_terminal_fin'] = True
        # 模拟 Start 响应在 Host 放弃等待后到达，重试仍必须返回原进程。
        second_start = dict(start, operationID=str(uuid.uuid4()), sessionID='session-2')
        request_id = peer.send('StartSession', second_start, abandoned=True)
        second_result = peer.call('StartSession', second_start)
        assert peer.call('GetState')['starts'] == 2
        terminal, _ = bind_terminal(peer, directory, 'session-2', 3)
        terminal_until(terminal, 3, b'WORKLOAD_READY')
        terminal.sendall(encode_frame(18, 3, b'ignoreterm\n'))
        terminal_until(terminal, 3, b'IGNORING_TERM\r\n')
        request_id = peer.send('StopSession', {'sessionID': 'session-2', 'graceMs': 2000, 'forceAfterTimeout': False})
        peer.cancel(request_id)
        response = peer.wait(request_id)
        assert response['error']['code'] == 'CANCELLED', response
        assert peer.call('GetState')['session']['exit'] is None
        checks['cancel_stop_wait_does_not_rollback_running_process'] = True
        peer.call('StopSession', {'sessionID': 'session-2', 'graceMs': 50, 'forceAfterTimeout': True})
        forced = wait_exit(peer, 'session-2')
        assert forced['code'] == -1 and forced['signal'] == 'killed', forced
        peer.call('DetachTerminal', {'sessionID': 'session-2', 'streamID': 3})
        terminal.close(); terminal = None
        checks['explicit_force_kills_fixed_workload_group'] = True
        # 控制重连仍在同 boot 范围，不能清空 operation 去重记录。
        close_control(peer, output)
        peer = Peer(connect(directory, 'control'), epoch)
        peer.hello()
        assert peer.call('StartSession', second_start) == second_result
        assert peer.call('GetState')['starts'] == 2
        checks['control_reconnect_preserves_operation_and_exit'] = True
        # 旧连接终端 token 不得被新控制连接接纳。
        terminal = connect(directory, 'terminal')
        terminal.sendall(encode_frame(16, 1, {'sessionID': 'session-1', 'epoch': epoch, 'connectionID': 'old-connection', 'bindToken': binding['bindToken']}))
        assert read_frame(terminal, True)[0] == 20
        terminal.close(); terminal = None
        checks['old_terminal_binding_rejected'] = True
        third_start = dict(start, operationID=str(uuid.uuid4()), sessionID='session-3')
        peer.call('StartSession', third_start)
        terminal, _ = bind_terminal(peer, directory, 'session-3', 1)
        terminal_until(terminal, 1, b'WORKLOAD_READY')
        before = (output / 'console.log').read_bytes().count(b'TERMINAL_PROTOCOL_ERROR')
        terminal.sendall(encode_frame(18, 999, b'echo\n'))
        wait_console(output, b'TERMINAL_PROTOCOL_ERROR', before)
        terminal.close(); terminal = None
        assert peer.call('GetState')['session']['exit'] is None
        terminal, _ = bind_terminal(peer, directory, 'session-3', 2)
        checks['unknown_stream_detaches_without_stopping_session'] = True
        terminal.sendall(encode_frame(18, 2, b'burst\n'))
        time.sleep(0.3)
        state = peer.call('GetState')
        assert state['session']['bufferedBytes'] <= 65536
        assert state['session']['exit'] is None
        peer.call('StopSession', {'sessionID': 'session-3', 'graceMs': 1000, 'forceAfterTimeout': True})
        assert wait_exit(peer, 'session-3')['signal'] != ''
        checks['slow_terminal_bounded_buffer_control_and_stop'] = True
        peer.call('DetachTerminal', {'sessionID': 'session-3', 'streamID': 2})
        terminal.close(); terminal = None
        checks['session_exited_event_observed'] = any(peer.events.get_nowait()['event'] == 'SessionExited' for _ in range(peer.events.qsize()))
        assert checks['session_exited_event_observed']
        # 主进程先退出而子进程忽略 TERM 时，Stop 不能提前成功。
        child_start = dict(start, operationID=str(uuid.uuid4()), sessionID='session-child')
        peer.call('StartSession', child_start)
        terminal, _ = bind_terminal(peer, directory, 'session-child', 3)
        terminal_until(terminal, 3, b'WORKLOAD_READY')
        terminal.sendall(encode_frame(18, 3, b'stubbornchild\n'))
        terminal_until(terminal, 3, b'STUBBORN_CHILD_READY\r\n')
        response = peer.wait(peer.send('StopSession', {'sessionID': 'session-child', 'graceMs': 50, 'forceAfterTimeout': False}))
        assert response['error']['code'] == 'DEADLINE_EXCEEDED', response
        peer.call('StopSession', {'sessionID': 'session-child', 'graceMs': 50, 'forceAfterTimeout': True})
        peer.call('StopSession', {'sessionID': 'session-child', 'graceMs': 50, 'forceAfterTimeout': True})
        peer.call('DetachTerminal', {'sessionID': 'session-child', 'streamID': 3})
        terminal.close(); terminal = None
        checks['stop_waits_for_stubborn_group_member_and_is_repeatable'] = True
        fourth = dict(start, operationID=str(uuid.uuid4()), sessionID='session-4')
        peer.call('StartSession', fourth)
        terminal, _ = bind_terminal(peer, directory, 'session-4', 4)
        terminal_until(terminal, 4, b'WORKLOAD_READY')
        terminal.sendall(encode_frame(18, 4, b'ignoreterm\n'))
        terminal_until(terminal, 4, b'IGNORING_TERM\r\n')
        peer.call('DetachTerminal', {'sessionID': 'session-4', 'streamID': 4})
        terminal.close(); terminal = None
    finally:
        if terminal is not None:
            terminal.close()
        close_control(peer, output)
    invalid_controls(directory, output, epoch, checks)


# main 编译固定 Guest，建立独立 VM，记录逐项结果并回收自有资源。
def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--assets', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--race-guest', action='store_true')
    args = parser.parse_args()
    assets, output = args.assets.resolve(), args.output.resolve()
    source = Path(__file__).resolve().parent
    output.mkdir(parents=True, exist_ok=False)
    base = assets / 'workspace-base.raw'
    before = sha256(base)
    directory = Path(tempfile.mkdtemp(prefix='sa-rpc-', dir='/tmp'))
    process = None
    results = {'scope': 'Linux/amd64/microvm/TCG，固定测试工作负载，非生产实现', 'checks': {}, 'passed': False}
    try:
        check_fixtures()
        results['checks']['python_shared_golden_frames'] = True
        host_response_checks(results['checks'])
        rootfs = output / 'rootfs'
        shutil.copytree(assets / 'rootfs', rootfs, symlinks=True)
        environment = dict(os.environ, CGO_ENABLED='1' if args.race_guest else '0', GOOS='linux', GOARCH='amd64')
        build = ['go', 'build'] + (['-race'] if args.race_guest else [])
        subprocess.run(build + ['-o', str(rootfs / 'bin/probe-guest'), '.'], cwd=source, env=environment, check=True)
        for filename, destination in [('init.sh', rootfs / 'init'), ('workload.sh', rootfs / 'bin/probe-workload')]:
            shutil.copyfile(source / filename, destination)
            destination.chmod(0o755)
        with (output / 'initramfs.cpio.gz').open('wb') as target:
            subprocess.run(['bash', '-o', 'pipefail', '-c', 'find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip -1'], cwd=rootfs, stdout=target, check=True)
        subprocess.run(['qemu-img', 'create', '-f', 'qcow2', '-F', 'raw', '-b', str(base), str(output / 'overlay.qcow2')], capture_output=True, check=True)
        command = ['qemu-system-x86_64', '-machine', 'microvm', '-accel', 'tcg', '-cpu', 'max', '-m', '512M', '-smp', '2', '-nodefaults', '-no-user-config', '-display', 'none', '-no-reboot', '-nic', 'none', '-kernel', str(assets / 'guest-kernel/boot/vmlinuz-6.8.0-147-generic'), '-initrd', str(output / 'initramfs.cpio.gz'), '-append', 'console=ttyS0 reboot=t panic=-1', '-serial', 'file:' + str(output / 'console.log'), '-device', 'virtio-serial-device,id=serial0', '-drive', 'id=workspace,file=' + str(output / 'overlay.qcow2') + ',format=qcow2,if=none', '-device', 'virtio-blk-device,drive=workspace']
        for number, name in enumerate(('control', 'terminal'), 1):
            command += ['-chardev', f'socket,id={name},path={directory / (name + ".sock")},server=on,wait=off', '-device', f'virtserialport,bus=serial0.0,nr={number},chardev={name},name=safeagent.{name}']
        (output / 'qemu-argv.json').write_text(json.dumps(command, indent=2) + '\n')
        with (output / 'qemu-stderr.log').open('wb') as stderr:
            process = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=stderr)
        deadline = time.monotonic() + 30
        while not (output / 'console.log').exists() or b'RPC_SPIKE_BOOTED' not in (output / 'console.log').read_bytes():
            assert process.poll() is None, (output / 'qemu-stderr.log').read_text()
            assert time.monotonic() < deadline, '等待 Guest 启动超时'
            time.sleep(0.02)
        run_checks(directory, output, results['checks'])
        results['race_guest'] = args.race_guest
        assert b'WARNING: DATA RACE' not in (output / 'console.log').read_bytes(), 'Guest race 检查失败'
        results['checks_completed'] = True
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        results['base_disk_unchanged'] = before == sha256(base)
        results['owned_process_reaped'] = process is None or process.poll() is not None
        results['passed'] = results.get('checks_completed', False) and results['base_disk_unchanged'] and results['owned_process_reaped']
        (output / 'results.json').write_text(json.dumps(results, ensure_ascii=False, indent=2) + '\n')
        shutil.rmtree(directory)
    assert results['passed'], results
    print(json.dumps(results, ensure_ascii=False, indent=2))


if __name__ == '__main__':
    main()
