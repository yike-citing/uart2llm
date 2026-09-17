#!/usr/bin/env python3
"""Negative acceptance: prove native Harness search cannot use the phase-1 API.

Temporarily point only the search namespace at the local endpoint and restore
the exact changed user fields. Never send the search through the PC network.
An observed unsupported endpoint is evidence of failure, never acceptance.
"""
import argparse
from datetime import datetime, timezone
import importlib.util
import json
from pathlib import Path
import time
import uuid
from harness_support import Harness

spec = importlib.util.spec_from_file_location('harness_check', Path(__file__).with_name('harness-check.py'))
hc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hc)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--report', required=True, type=Path)
    args = parser.parse_args()
    if args.report.exists(): parser.error('Preserve existing report; choose a new path')
    h = Harness()
    namespaces = {n['ns']: n for n in h.rpc('settings/describe')['namespaces']}
    n = namespaces['web-search-deepseek']
    if any(s['set'] for s in n.get('secrets', [])):
        raise RuntimeError('Literal search credential exists; preserve it and stop this probe')
    if any(s['running'] for s in h.rpc('session/list', _request={})['items']):
        raise RuntimeError('Wait for existing Harness tasks before temporary search configuration')
    provider = namespaces['llm-pi-ai']['value']['providers']['uart2llm']
    ref = provider.get('apiKeyEnv')
    if provider.get('baseURL') != 'http://127.0.0.1:8765/v1' or not ref:
        raise RuntimeError('Expected the configured local provider; preserve search configuration')
    route = {'baseURL': 'http://127.0.0.1:8765/anthropic/v1', 'apiKeyEnv': ref}
    before = n.get('user', {})
    restore = [{'op': 'set', 'path': [k], 'value': before[k]} if k in before else
               {'op': 'unset', 'path': [k]} for k in route]
    w = hc.ROOT / '.local/harness-acceptance' / ('search-' + uuid.uuid4().hex[:8])
    w.mkdir(parents=True)
    (w/'AGENTS.md').write_text('Synthetic search transport probe. Do not read files, write files, run commands, '
        'change settings or use web_fetch. Only the single requested web_search call is allowed.\n', encoding='utf-8')
    runner = hc.Runner(h, w, time.monotonic()+120)
    report = {'client': 'installed DeepSeek Harness Web', 'phase': 'search', 'passed': False,
        'full_feature_acceptance': False, 'started_utc': datetime.now(timezone.utc).isoformat(),
        'search_base': route['baseURL'], 'checks': {}}
    changed = False
    try:
        h.rpc('settings/mutate', ns=n['ns'], expectedRevision=n['revision'],
            ops=[{'op': 'set', 'path': [k], 'value': v} for k, v in route.items()])
        changed = True
        f = runner.create()
        reason = runner.prompt(f, 'Test the installed web_search tool exactly once: search for '
            'ESP32-S3 official technical documentation. Do not answer from memory and do not use '
            'web_fetch, shell, another provider or a fallback. If it fails, report that failure briefly. '
            'Do not change endpoint settings or ask me a question; this is a controlled transport test.')
        page = h.rpc('session/page', request={'address': f.address, 'throughSeq': f.cursor, 'maxMessages': 100})
        errors = []
        for record in page['records']:
            e = record.get('event', {})
            if e.get('type') != 'tool/result': continue
            text = '\n'.join(hc.text_blocks(e.get('data', {}).get('message', {}).get('content')))
            errors.extend(word for word in ('unsupported_endpoint','404','WEB_PROVIDER_ERROR') if word in text)
        search = [t for t in f.tools.values() if t.get('name') == 'web_search']
        report['checks'].update({'model_turn_completed': reason == 'completed',
            'native_search_tool_called': len(search) == 1,
            'native_search_failed': bool(search) and all(t.get('is_error') for t in search),
            'unsupported_local_route_confirmed': 'unsupported_endpoint' in errors,
            'no_fetch_or_shell_fallback': all(t.get('name') == 'web_search' for t in f.tools.values())})
        report['error_codes'] = sorted(set(errors))
    except Exception as error: report['error'] = hc.safe_error(error)
    finally:
        report['cleanup_errors'] = runner.cleanup()
        if changed:
            try:
                current = next(n for n in h.rpc('settings/describe')['namespaces'] if n['ns'] == 'web-search-deepseek')
                if all(current['value'].get(k) == v for k,v in route.items()):
                    h.rpc('settings/mutate', ns=n['ns'], expectedRevision=current['revision'], ops=restore)
                    restored = next(n for n in h.rpc('settings/describe')['namespaces'] if n['ns'] == 'web-search-deepseek')
                    actual = restored.get('user', {})
                    report['checks']['configuration_restored'] = all(
                        (k in actual) == (k in before) and actual.get(k) == before.get(k) for k in route)
                else:
                    report['checks']['configuration_restored'] = False
                    report['restore_conflict'] = True
            except Exception as error:
                report['checks']['configuration_restored'] = False
                report['restore_error'] = hc.safe_error(error)
        report['sessions'] = [f.summary() for f in runner.follows]
        report['finished_utc'] = datetime.now(timezone.utc).isoformat()
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')
        print(json.dumps({k:v for k,v in report.items() if k != 'sessions'}, ensure_ascii=True))
    return 1


if __name__ == '__main__': raise SystemExit(main())
