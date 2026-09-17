"""Verify tray metrics against real upstream usage without recording credentials/bodies."""
import argparse
import hashlib
import json
import subprocess
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from hardware_check_support import default_executable


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--exe', type=Path, default=default_executable())
    p.add_argument('--api', default='http://localhost:8765')
    p.add_argument('--admin', default='http://localhost:8766/admin/v1')
    p.add_argument('--model', default='deepseek-flash')
    p.add_argument('--agent-report', type=Path)
    p.add_argument('--reconcile-only', action='store_true')
    p.add_argument('--report', type=Path, required=True)
    args = p.parse_args()
    tokens = {kind: subprocess.check_output([str(args.exe), 'token', kind], text=True).strip()
              for kind in ('api', 'admin')}
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(base, path, token=None, body=None):
        headers = {'Content-Type': 'application/json'}
        if token:
            headers['Authorization'] = 'Bearer ' + token
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(base + path, data=data, headers=headers)
        try:
            response = opener.open(req, timeout=120)
        except urllib.error.HTTPError as e:
            response = e
        with response:
            return response.status, dict(response.headers), json.load(response)

    def metrics():
        code, _, value = request(args.admin, '/metrics', tokens['admin'])
        assert code == 200
        return value

    def settled(completed):
        for _ in range(100):
            v = metrics()
            if v['active'] == 0 and v['completed'] >= completed:
                return v
            time.sleep(.1)
        raise AssertionError('request cleanup did not settle')

    checks = {}
    report = {'started_utc': datetime.now(timezone.utc).isoformat(),
              'host_sha256': hashlib.sha256(args.exe.read_bytes()).hexdigest(),
              'physical_transport': 'not independently identified; inspect device telemetry', 'checks': checks}
    try:
        before = metrics()
        report['before'] = before
        if args.agent_report:
            agent = json.loads(args.agent_report.read_text(encoding='utf-8'))
            usage = agent['usage_totals']
            checks['agent_passed'] = agent['passed']
            checks['same_binary'] = report['host_sha256'] == agent['host_sha256']
            checks['agent_requests_exact'] = before['completed'] == len(agent['requests']) == before['usage_reported']
            for key in ('prompt_tokens', 'completion_tokens', 'total_tokens'):
                checks['agent_' + key + '_exact'] = before[key] == usage[key]
            checks['agent_cached_tokens_exact'] = before['cached_tokens'] == usage['prompt_cache_hit_tokens']
            checks['agent_no_failures_or_missing_usage'] = before['failed'] == before['usage_missing'] == 0
            report['agent_usage_totals'] = usage
        if not args.reconcile_only:
            assert before['active'] == 0 and not before['paused'], 'requires an idle, resumed daemon'
            checks['metrics_authenticated'] = request(args.admin, '/metrics')[0] == 401
            checks['pause_authenticated'] = request(args.admin, '/proxy/pause', body={'paused': True})[0] == 401
            checks['pause_strict_validation'] = request(args.admin, '/proxy/pause', tokens['admin'], {})[0] == 400
            try:
                checks['pause_applied'] = request(args.admin, '/proxy/pause', tokens['admin'], {'paused': True})[0] == 200
                code, headers, body = request(args.api, '/v1/models', tokens['api'])
                checks['pause_rejects_with_retry_hint'] = (code == 503 and headers.get('Retry-After') == '1'
                                                        and body['error']['code'] == 'proxy_paused')
            finally:
                checks['resume_applied'] = request(args.admin, '/proxy/pause', tokens['admin'], {'paused': before['paused']})[0] == 200
            base = settled(before['completed'] + 1)
            code, _, body = request(args.api, '/v1/chat/completions', tokens['api'],
                                    {'model': args.model, 'stream': False, 'max_tokens': 96,
                                     'thinking': {'type': 'disabled'},
                                     'messages': [{'role': 'user', 'content': 'Reply with the word READY.'}]})
            checks['real_json_completion'] = code == 200 and bool(body.get('choices'))
            usage = body.get('usage', {})
            report['json_usage'] = usage
            after = settled(base['completed'] + 1)
            report['after'] = after
            for key in ('prompt_tokens', 'completion_tokens', 'total_tokens'):
                checks['json_' + key + '_exact'] = key in usage and after[key] - base[key] == usage[key]
            cached = usage.get('prompt_cache_hit_tokens', usage.get('prompt_tokens_details', {}).get('cached_tokens', 0))
            checks['json_cached_tokens_exact'] = after['cached_tokens'] - base['cached_tokens'] == cached
            checks['json_usage_reported_once'] = after['usage_reported'] - base['usage_reported'] == 1
            checks['pause_counted_as_rejected'] = after['rejected'] - before['rejected'] == 1
            checks['request_outcomes'] = (after['requests'] - before['requests'] == 2
                                         and after['succeeded'] - before['succeeded'] == 1
                                         and after['failed'] - before['failed'] == 1)
            _, _, state = request(args.admin, '/state', tokens['admin'])
            checks['state_and_metrics_consistent'] = all(state['llm'][key] == after[key]
                                                        for key in ('requests', 'completed', 'total_tokens', 'paused'))
            checks['device_ready_after_test'] = state['connection']['paired'] and not after['paused'] and after['active'] == 0
        report['passed'] = bool(checks) and all(checks.values())
    except Exception as e:
        # Never serialize HTTP request headers or response bodies on failure.
        report['error_type'] = type(e).__name__
        report['passed'] = False
    report['finished_utc'] = datetime.now(timezone.utc).isoformat()
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
    print(json.dumps({'passed': report['passed'], 'checks': checks}, ensure_ascii=False))
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
