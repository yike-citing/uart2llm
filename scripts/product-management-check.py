"""Verify live configuration clock/freshness and recovery without persisting changes."""
import argparse
import json
import subprocess
import time
import urllib.request
from pathlib import Path
from hardware_check_support import default_executable


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--report', required=True, type=Path)
    args = p.parse_args()
    key = subprocess.check_output([str(default_executable()), 'token', 'admin'], text=True).strip()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    def request(path, method='GET', body=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request('http://localhost:8766/admin/v1'+path, method=method, data=data,
                                     headers={'Authorization': 'Bearer '+key, 'Content-Type': 'application/json'})
        with opener.open(req, timeout=35) as r: return json.load(r)
    report = {'checks': {}, 'passed': False}
    original = None
    applied = False
    try:
        assert request('/metrics')['active'] == 0
        original = request('/config')['device']; assert not original['pending']
        applied = True  # A lost apply response may still have changed the device.
        request('/config', 'PATCH', {'device': {'telemetry.interval_ms': 60000}})
        c1 = request('/config')['device']
        first = request('/diagnostics', 'POST', {'action': 'health'})
        time.sleep(1.5)
        c2 = request('/config')['device']
        second = request('/diagnostics', 'POST', {'action': 'health'})
        checks = report['checks']
        checks['pending_applied'] = c1['pending'] and c1['effective']['telemetry.interval_ms'] == 60000
        checks['device_clock_advances'] = c2['current_time_ms']-c1['current_time_ms'] >= 1400
        checks['deadline_stable'] = c1['confirm_deadline_ms'] == c2['confirm_deadline_ms']
        checks['candidate_state_is_live_despite_60s_sampling'] = second['sample_time_ms']-first['sample_time_ms'] >= 1400
        checks['configuration_and_state_session_match'] = c1['session'] == c2['session'] == first['session'] == second['session']
        checks['still_unpersisted'] = c2['persisted'] == original['persisted']
        report['clock_delta_ms'] = c2['current_time_ms']-c1['current_time_ms']
        report['state_delta_ms'] = second['sample_time_ms']-first['sample_time_ms']
        report['remaining_confirm_seconds'] = (c2['confirm_deadline_ms']-c2['current_time_ms'])/1000
    except Exception as e: report['error_type'] = type(e).__name__
    finally:
        if applied:
            try:
                request('/config/rollback', 'POST')
                for _ in range(40):
                    time.sleep(.25)
                    final = request('/config')['device']
                    if not final['pending']: break
                report['checks']['rolled_back_and_preserved'] = not final['pending'] and final['effective'] == original['effective'] and final['persisted'] == original['persisted']
            except Exception as e: report['cleanup_error_type'] = type(e).__name__
        report['passed'] = len(report['checks']) == 7 and all(report['checks'].values()) and 'error_type' not in report and 'cleanup_error_type' not in report
        args.report.write_text(json.dumps(report, indent=2), encoding='utf-8')
        print(json.dumps(report))
    return 0 if report['passed'] else 1


if __name__ == '__main__': raise SystemExit(main())
