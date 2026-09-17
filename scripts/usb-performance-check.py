"""Bounded real-model SSE/management performance evidence, without payloads or secrets."""
import argparse
import concurrent.futures
import hashlib
import json
import subprocess
import threading
import time
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from hardware_check_support import default_executable, sanitize


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--exe', type=Path, default=default_executable())
    p.add_argument('--report', type=Path, required=True)
    p.add_argument('--parallel', type=int, choices=(1, 4), default=1)
    p.add_argument('--tokens', type=int, default=256)
    p.add_argument('--prompt-bytes', type=int, default=0)
    args = p.parse_args()
    keys = {k: subprocess.check_output([str(args.exe), 'token', k], text=True).strip() for k in ('api', 'admin')}
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    def request(path, data=None, api=False):
        raw = None if data is None else json.dumps(data).encode()
        req = urllib.request.Request(('http://localhost:8765' if api else 'http://localhost:8766/admin/v1') + path,
                                     data=raw, headers={'Authorization': 'Bearer '+keys['api' if api else 'admin'], 'Content-Type': 'application/json'})
        return opener.open(req, timeout=180)
    def state():
        with request('/state') as r: return json.load(r)
    report = {'started_utc': datetime.now(timezone.utc).isoformat(), 'host_sha256': hashlib.sha256(args.exe.read_bytes()).hexdigest(),
              'parallel': args.parallel, 'output_token_limit': args.tokens, 'requests': [], 'management_ms': [], 'before': sanitize(state())}
    stop = threading.Event()
    def monitor():
        while not stop.is_set():
            start = time.monotonic()
            try:
                with request('/diagnostics', {'action': 'health'}) as r: json.load(r)
                report['management_ms'].append(round((time.monotonic()-start)*1000, 2))
            except Exception as e: report.setdefault('management_errors', []).append(type(e).__name__)
            stop.wait(1)
    def chat(index):
        body = {'model': 'deepseek-flash', 'stream': True, 'stream_options': {'include_usage': True},
                'thinking': {'type': 'disabled'}, 'max_tokens': args.tokens,
                'messages': [{'role': 'user', 'content': 'Write a detailed numbered guide to caring for indoor plants. '+
                              ('Context only, ignore: abcdefghijklmnopqrstuvwxyz. ' * (args.prompt_bytes//48))}]}
        out = {'index': index, 'request_bytes': len(json.dumps(body).encode()), 'wire_bytes': 0, 'events': 0, 'done': False}
        start = time.monotonic(); last = None; gaps = []
        try:
            with request('/v1/chat/completions', body, True) as r:
                out['status'] = r.status; out['headers_ms'] = round((time.monotonic()-start)*1000, 2)
                for line in r:
                    now = time.monotonic(); out['wire_bytes'] += len(line)
                    if now-start > 180: raise TimeoutError('test deadline')
                    if not line.startswith(b'data:'): continue
                    if last is not None: gaps.append((now-last)*1000)
                    last = now; out['events'] += 1
                    payload = line[5:].strip()
                    if payload == b'[DONE]': out['done'] = True; continue
                    event = json.loads(payload)
                    if event.get('usage'): out['usage'] = event['usage']
                    for choice in event.get('choices', []):
                        if choice.get('delta', {}).get('content') and 'first_content_ms' not in out:
                            out['first_content_ms'] = round((now-start)*1000, 2)
                        if choice.get('finish_reason'): out['finish_reason'] = choice['finish_reason']
            out['passed'] = out['status'] == 200 and out['done'] and bool(out.get('usage'))
        except Exception as e: out['passed'] = False; out['error_type'] = type(e).__name__
        out['duration_ms'] = round((time.monotonic()-start)*1000, 2)
        out['wire_bytes_per_second'] = round(out['wire_bytes']*1000/max(1, out['duration_ms']), 2)
        if gaps:
            gaps.sort(); out['event_gap_p95_ms'] = round(gaps[min(len(gaps)-1, int(len(gaps)*.95))], 2); out['event_gap_max_ms'] = round(max(gaps), 2)
        return out
    worker = threading.Thread(target=monitor); worker.start()
    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.parallel) as pool:
            report['requests'] = list(pool.map(chat, range(args.parallel)))
    finally: stop.set(); worker.join()
    for _ in range(100):
        after = state()
        if after.get('llm', {}).get('active') == 0: break
        time.sleep(.1)
    report['after'] = sanitize(after)
    report['passed'] = all(r['passed'] for r in report['requests']) and not report.get('management_errors') and after.get('llm', {}).get('active') == 0
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
    print(json.dumps({k: report[k] for k in ('passed', 'requests', 'management_ms')}, ensure_ascii=False))
    return 0 if report['passed'] else 1


if __name__ == '__main__': raise SystemExit(main())
