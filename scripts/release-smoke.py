"""Run the single EXE with isolated configuration on a clean Windows CI runner.

Never use this lifecycle check on a user session already running uart2llm.
No physical device, upstream credential or model request is used.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import urllib.request


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--exe', type=Path, required=True)
    p.add_argument('--version', required=True)
    args = p.parse_args()
    if os.environ.get('GITHUB_ACTIONS') != 'true':
        p.error('This lifecycle test is restricted to a clean GitHub Actions runner')
    exe = args.exe.resolve()
    with tempfile.TemporaryDirectory(prefix='uart2llm-release-') as tmp:
        env = {**os.environ, 'UART2LLM_DATA_DIR': tmp}
        def cli(*command):
            r = subprocess.run([str(exe), *command], env=env, capture_output=True,
                               encoding='utf-8', timeout=35)
            if r.returncode:
                raise RuntimeError('CLI command failed: ' + command[0])
            return r.stdout.strip()
        assert cli('version') == args.version
        assert 'MIT License' in cli('licenses')
        cli('init')
        # Use independent ephemeral loopback ports; no test upstream required.
        sockets = [socket.socket() for _ in range(2)]
        for s in sockets: s.bind(('127.0.0.1', 0))
        ports = [s.getsockname()[1] for s in sockets]
        cfgpath = Path(tmp) / 'config.json'
        cfg = json.loads(cfgpath.read_text(encoding='utf-8'))
        cfg['api_listen'] = '127.0.0.1:' + str(ports[0])
        cfg['admin_listen'] = '127.0.0.1:' + str(ports[1])
        cfgpath.write_text(json.dumps(cfg), encoding='utf-8')
        for s in sockets: s.close()
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        started = False
        try:
            cli('start'); started = True
            first = json.loads((Path(tmp) / 'runtime.json').read_text())['pid']
            cli('start')
            assert json.loads((Path(tmp) / 'runtime.json').read_text())['pid'] == first
            base = 'http://' + cfg['admin_listen']
            with opener.open(base, timeout=10) as response: html = response.read().decode()
            assets = re.findall(r'(?:src|href)="(/assets/[^\"]+)"', html)
            assert assets and any(x.endswith('.js') for x in assets)
            for asset in assets:
                with opener.open(base + asset, timeout=10) as response: assert len(response.read()) > 100
            state = json.loads(cli('status'))
            assert not state['connected']
            assert cli('token', 'api')
            print(json.dumps({'single_exe':True,'version':args.version,
                'embedded_ui_assets':len(assets),'daemon_reused':True,
                'management_authenticated':True,'device_not_required':True,
                'sha256':hashlib.sha256(exe.read_bytes()).hexdigest()}))
        finally:
            if started: cli('stop')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
