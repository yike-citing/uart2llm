"""Validate automatic browser sessions against the real local daemon; no credentials read."""
import argparse
import http.cookiejar
import json
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    jar = http.cookiejar.CookieJar()
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(jar))

    def call(path, method='GET', body=None, extra=None):
        headers = {'Content-Type': 'application/json', 'Origin': 'http://localhost:8766', 'Sec-Fetch-Site': 'same-origin'}
        headers.update(extra or {})
        req = urllib.request.Request('http://localhost:8766/admin/v1'+path, method=method, headers=headers,
                                     data=None if body is None else json.dumps(body).encode())
        try:
            result = client.open(req, timeout=15)
        except urllib.error.HTTPError as e:
            result = e
        with result:
            return result.status, json.load(result)

    checks = {}
    report = {'started_utc': datetime.now(timezone.utc).isoformat(), 'credentials_supplied': False, 'checks': checks}
    try:
        checks['fresh_browser_has_no_session'] = call('/state')[0] == 401
        checks['bootstrap_without_password_or_token'] = call('/session', 'POST', {})[0] == 200
        cookies = list(jar)
        checks['restricted_cookie'] = len(cookies) == 1 and cookies[0].path == '/admin/v1' and cookies[0].has_nonstandard_attr('HttpOnly') and cookies[0].get_nonstandard_attr('SameSite') == 'Strict'
        code, state = call('/state')
        checks['automatic_state_read'] = code == 200
        checks['configuration_without_credentials'] = call('/config')[0] == 200
        checks['cross_site_rejected'] = call('/state', extra={'Origin':'https://example.org', 'Sec-Fetch-Site':'cross-site'})[0] == 403
        checks['form_bootstrap_rejected'] = call('/session', 'POST', {}, {'Content-Type':'text/plain'})[0] == 415
        assert state['llm']['active'] == 0, 'device busy'
        paused = state['llm']['paused']
        try:
            checks['pause_with_automatic_cookie'] = call('/proxy/pause', 'POST', {'paused':True})[0] == 200
            checks['pause_visible'] = call('/metrics')[1]['paused'] is True
        finally:
            checks['original_pause_restored'] = call('/proxy/pause', 'POST', {'paused':paused})[0] == 200
        checks['clear_session'] = call('/session','DELETE')[0] == 200
        checks['cleared_session_denied'] = call('/state')[0] == 401
        checks['reestablish_without_input'] = call('/session','POST',{})[0] == 200 and call('/state')[0] == 200
        code, final = call('/state')
        checks['device_still_paired'] = code == 200 and final['connection']['paired']
        checks['device_session_stable'] = state['connection']['link']['session'] == final['connection']['link']['session']
        report['passed'] = all(checks.values())
    except Exception as e:
        report['error_type'] = type(e).__name__
        report['passed'] = False
    report['finished_utc'] = datetime.now(timezone.utc).isoformat()
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
    print(json.dumps(report, ensure_ascii=False))
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
