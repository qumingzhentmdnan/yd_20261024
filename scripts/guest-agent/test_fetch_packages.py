"""验证下载边界不读取 curlrc，也不依赖宿主网络或凭据。"""
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('fetch_packages', Path(__file__).with_name('fetch-packages.py'))
fetch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fetch)


# FetchPackagesTest 覆盖下载命令的认证隔离与摘要校验。
class FetchPackagesTest(unittest.TestCase):
    # test_curl_disables_default_config_first 验证所有真实锁定下载均以 curl -q 开始，且校验后才发布文件。
    def test_curl_disables_default_config_first(self):
        lock = json.loads((Path(__file__).resolve().parents[2] / 'agents/packages.lock.json').read_text())
        with tempfile.TemporaryDirectory() as name:
            calls = []

            # download 只模拟网络写入，测试不启动 curl 或读取宿主文件。
            def download(command, check):
                self.assertEqual(command[:2], ['curl', '-q'])
                self.assertTrue(check)
                calls.append(command)
                Path(command[command.index('--output') + 1]).write_bytes(b'fixture')

            with patch.object(sys, 'argv', ['fetch-packages.py', '--agent', 'codex', '--output', name]), patch.object(fetch.subprocess, 'run', side_effect=download), patch.object(fetch.hashlib, 'sha256') as digest:
                digest.return_value.hexdigest.side_effect = [entry['sha256'] for entry in [lock['agents']['codex'], *lock['runtime']] for _ in range(2)]
                fetch.main()
            self.assertEqual(len(calls), 1 + len(lock['runtime']))
            self.assertFalse(list(Path(name).glob('*.partial')))


if __name__ == '__main__':
    unittest.main()
