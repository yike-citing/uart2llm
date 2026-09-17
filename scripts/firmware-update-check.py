"""Upload an authorized matching firmware image and verify boot/config recovery."""
import argparse
import hashlib
import json
import subprocess
import time
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from hardware_check_support import default_executable, sanitize


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--image', type=Path, required=True)
    p.add_argument('--report', type=Path, required=True)
    args = p.parse_args()
    key = subprocess.check_output([str(default_executable()), 'token', 'admin'], text=True).strip()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    def request(path, data=None, binary=False):
        raw = data if binary else (None if data is None else json.dumps(data).encode())
        req = urllib.request.Request('http://localhost:8766/admin/v1'+path, data=raw,
            headers={'Authorization': 'Bearer '+key, 'Content-Type': 'application/octet-stream' if binary else 'application/json'})
        with opener.open(req, timeout=180) as r: return json.load(r)
    report = {'started_utc': datetime.now(timezone.utc).isoformat(), 'checks': {}, 'passed': False}
    try:
        image = args.image.read_bytes()
        report['image_sha256'] = hashlib.sha256(image).hexdigest()
        report['image_bytes'] = len(image)
        # A ready HTTP listener can precede automatic device pairing and the
        # first fresh snapshot. Wait before performing any maintenance write.
        for _ in range(100):
            before = request('/state')
            device = before.get('device') or {}
            if (before.get('connection', {}).get('paired') and device.get('session')
                == before.get('connection', {}).get('link', {}).get('session') and device.get('network', {}).get('connected')):
                break
            time.sleep(.5)
        else: raise TimeoutError('device did not become ready')
        configuration = request('/config')['device']
        assert before['llm']['active'] == 0 and not configuration['pending']
        report['before'] = sanitize(before)
        start = time.monotonic()
        report['upload'] = request('/firmware', image, True)
        report['upload_seconds'] = round(time.monotonic()-start, 3)
        # ota.end schedules the candidate reboot itself; do not send another
        # lifecycle command during the transition.
        for _ in range(120):
            time.sleep(.5)
            state = request('/state'); device = state.get('device') or {}
            if (state['connection'].get('paired') and device.get('session') == state['connection'].get('link', {}).get('session')
                and device.get('session') != before['connection']['link']['session'] and device.get('network', {}).get('connected')):
                break
        after = request('/config')['device']
        report['after'] = sanitize(state)
        report['checks'] = {'new_session': device.get('session') != before['connection']['link']['session'],
            'network_ready': device.get('network', {}).get('connected') is True,
            'config_preserved': after['persisted'] == configuration['persisted'] and not after['pending'],
            'no_coredump': not device.get('crash', {}).get('available'),
            'wifi_power_save_disabled': device.get('network', {}).get('power_save') == 0}
        report['passed'] = all(report['checks'].values())
    except Exception as e: report['error_type'] = type(e).__name__
    args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
    print(json.dumps({'passed': report['passed'], 'checks': report['checks'], 'upload_seconds': report.get('upload_seconds'), 'error_type': report.get('error_type')}))
    return 0 if report['passed'] else 1


if __name__ == '__main__': raise SystemExit(main())
