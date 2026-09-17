#!/usr/bin/env python3
"""Run real OpenCode agent workloads through the configured physical ESP32.

The loopback observation server forwards unmodified Chat Completions bodies to
the production gateway. It never contacts a model provider directly. Public
evidence contains metadata, usage and synthetic fixture output, not credentials
or model reasoning. This is a client acceptance runner, not a product runtime.
"""
import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import threading
import time

from hardware_check_support import default_executable, sanitize

ROOT = Path(__file__).resolve().parent.parent
MODEL = 'deepseek-flash'
FIXTURE = {
    'price.py': '''def total_cents(unit_cents, quantity, discount_percent=0):
    return int(unit_cents * quantity * (1 - discount_percent / 100))
''',
    'requirements.md': '''# Synthetic shop contract
Return integer cents. Inputs must be integers (booleans excluded).
Unit cents and quantity must be nonnegative; discount is an integer from 0 to 100.
Raise ValueError for invalid inputs. Calculate entirely with integer arithmetic.
Round a fractional cent half up. Zero quantity returns zero.
Examples: (101, 1, 50) = 51; (199, 3, 15) = 507.
''',
    'verify.py': '''from price import total_cents
checks = [(101,1,50,51),(199,3,15,507),(100,0,20,0),(17,2,100,0),(100000000000000001,3,0,300000000000000003)]
for u,q,d,want in checks:
    assert total_cents(u,q,d) == want, (u,q,d,want)
for args in [(-1,2,0),(1,-1,0),(1,1,101),(1,1,-1),(True,1,0),(1,1.5,0)]:
    try: total_cents(*args)
    except ValueError: pass
    else: raise AssertionError(('must reject',args))
print('VERIFIED: 11 price and validation cases')
''',
    '.opencode/skills/money-review/SKILL.md': '''---
name: money-review
description: Audit and repair the synthetic shop integer-money calculation.
---
Read requirements.md, locate total_cents with grep, and inspect price.py.
Do not use floating point. Work in integer cents, round half up, and reject
booleans and other non-integers. Include the marker MONEY-SKILL-LOADED in your
summary so the verifier can distinguish loading this file from guessing.
In plan mode propose changes without editing. In build mode only edit price.py
and run verify.py. Never read external directories or credentials.
'''
}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def read_json_local(port, path, token):
    conn = http.client.HTTPConnection('127.0.0.1', port, timeout=12)
    try:
        conn.request('GET', path, headers={'Authorization': 'Bearer ' + token})
        response = conn.getresponse()
        if response.status != 200:
            raise RuntimeError('management HTTP ' + str(response.status))
        return json.loads(response.read(2 * 1024 * 1024))
    finally:
        conn.close()


class Observer:
    def __init__(self, token, limit=64, forward_port=8765, output_cap=1536):
        self.token, self.client_token, self.limit = token, secrets.token_urlsafe(32), limit
        self.forward_port = forward_port
        self.output_cap = output_cap
        self.lock = threading.RLock()
        self.requests, self.rejected = [], []
        self.active = self.peak = 0
        self.phase = 'prepare'
        self.deadline = time.monotonic() + 1200

    def handler(self):
        owner = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'

            def log_message(self, *_):
                pass

            def reject(self, code, reason):
                with owner.lock:
                    owner.rejected.append({'phase': owner.phase, 'status': code, 'reason': reason})
                data = json.dumps({'error': {'message': reason, 'type': 'acceptance_limit'}}).encode()
                self.send_response(code)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(data)))
                self.send_header('Connection', 'close')
                self.end_headers()
                self.wfile.write(data)
                self.close_connection = True

            def do_POST(self):
                if self.path != '/v1/chat/completions':
                    return self.reject(404, 'only Chat Completions accepted')
                if self.headers.get('Authorization') != 'Bearer ' + owner.client_token:
                    return self.reject(401, 'observer authentication required')
                length = int(self.headers.get('Content-Length', '0'))
                if self.headers.get('Transfer-Encoding') or not 0 < length <= 1024 * 1024:
                    return self.reject(413, 'bounded Content-Length required')
                self.connection.settimeout(150)
                raw = self.rfile.read(length)
                try:
                    payload = json.loads(raw)
                    cap = payload.get('max_tokens', payload.get('max_completion_tokens', 0))
                    if payload.get('model') != MODEL or type(cap) is not int or not 0 < cap <= owner.output_cap:
                        return self.reject(400, 'expected configured model and bounded integer output cap')
                except (ValueError, TypeError):
                    return self.reject(400, 'invalid JSON or output cap')
                with owner.lock:
                    if len(owner.requests) >= owner.limit or time.monotonic() >= owner.deadline:
                        return self.reject(429, 'run request/time budget exhausted')
                    item = {'id': len(owner.requests) + 1, 'phase': owner.phase,
                            'started': time.monotonic(), 'request_bytes': length,
                            'started_utc': datetime.now(timezone.utc).isoformat(),
                            'request_sha256': digest(raw), 'model': payload['model'],
                            'stream': payload.get('stream', False), 'output_cap': cap,
                            'message_roles': [m.get('role') for m in payload.get('messages', [])],
                            'offered_tools': [t.get('function', {}).get('name') for t in payload.get('tools', [])],
                            'tool_result_ids': [m.get('tool_call_id') for m in payload.get('messages', []) if m.get('role') == 'tool'],
                            'events': 0, 'response_bytes': 0, 'done': False, 'tool_calls': {},
                            'reasoning_bytes': 0}
                    owner.requests.append(item)
                    owner.active += 1
                    owner.peak = max(owner.peak, owner.active)
                print('model request ' + str(item['id']) + ': ' + owner.phase, flush=True)
                conn = http.client.HTTPConnection('127.0.0.1', owner.forward_port, timeout=150)
                received, error_body, response_hash, headers_sent = bytearray(), bytearray(), hashlib.sha256(), False
                try:
                    conn.request('POST', self.path, body=raw, headers={
                        'Authorization': 'Bearer ' + owner.token, 'Content-Type': 'application/json',
                        'Accept': self.headers.get('Accept', 'text/event-stream')})
                    response = conn.getresponse()
                    item['status'] = response.status
                    retry_after = response.getheader('Retry-After')
                    if retry_after and retry_after.isdigit():
                        item['retry_after_seconds'] = int(retry_after)
                    item['header_seconds'] = time.monotonic() - item['started']
                    self.send_response(response.status)
                    for key, value in response.getheaders():
                        if key.lower() not in ('content-length', 'transfer-encoding', 'connection'):
                            self.send_header(key, value)
                    self.send_header('Transfer-Encoding', 'chunked')
                    self.send_header('Connection', 'close')
                    self.end_headers()
                    self.close_connection = True
                    headers_sent = True
                    while chunk := response.read1(8192):
                        item['response_bytes'] += len(chunk)
                        if item['response_bytes'] > 4 * 1024 * 1024:
                            raise RuntimeError('response observation bound exceeded')
                        response_hash.update(chunk)
                        if response.status != 200 and len(error_body) < 65536:
                            error_body.extend(chunk[:65536-len(error_body)])
                        self.wfile.write(f'{len(chunk):x}\r\n'.encode() + chunk + b'\r\n')
                        self.wfile.flush()
                        received.extend(chunk)
                        while b'\n' in received:
                            line, _, tail = received.partition(b'\n')
                            received = bytearray(tail)
                            if not line.startswith(b'data:'):
                                continue
                            value = line[5:].strip()
                            if value == b'[DONE]':
                                item['done'] = True
                                continue
                            event = json.loads(value)
                            item['events'] += 1
                            item.setdefault('first_event_seconds', time.monotonic() - item['started'])
                            if event.get('usage'):
                                item['usage'] = event['usage']
                            for choice in event.get('choices', []):
                                if choice.get('finish_reason'):
                                    item['finish_reason'] = choice['finish_reason']
                                delta = choice.get('delta', {})
                                item['reasoning_bytes'] += len((delta.get('reasoning_content') or '').encode())
                                for tool in delta.get('tool_calls', []):
                                    acc = item['tool_calls'].setdefault(str(tool['index']), {'id': '', 'name': '', 'arguments': '', 'fragments': 0})
                                    acc['id'] += tool.get('id') or ''
                                    acc['name'] += tool.get('function', {}).get('name') or ''
                                    acc['arguments'] += tool.get('function', {}).get('arguments') or ''
                                    acc['fragments'] += 1
                    self.wfile.write(b'0\r\n\r\n')
                    self.wfile.flush()
                    if response.status != 200:
                        item.update(error_metadata(error_body))
                    for acc in item['tool_calls'].values():
                        arguments = acc.pop('arguments')
                        acc['argument_bytes'] = len(arguments.encode())
                        acc['argument_sha256'] = digest(arguments.encode())
                        try:
                            json.loads(arguments)
                            acc['valid_json'] = True
                        except ValueError:
                            acc['valid_json'] = False
                except Exception as exc:
                    item['error'] = type(exc).__name__
                    if not headers_sent:
                        self.reject(502, 'observation forwarding failed')
                    self.close_connection = True
                finally:
                    conn.close()
                    item['response_sha256'] = response_hash.hexdigest()
                    item['seconds'] = time.monotonic() - item.pop('started')
                    item['finished_utc'] = datetime.now(timezone.utc).isoformat()
                    # An interrupted tool call must not leave raw model output.
                    for acc in item['tool_calls'].values():
                        acc.pop('arguments', None)
                    with owner.lock:
                        owner.active -= 1

        return Handler


def error_metadata(raw):
    """Retain machine codes only; provider error messages may quote inputs."""
    try:
        error = json.loads(raw).get('error', {})
        if not isinstance(error, dict):
            return {}
    except (ValueError, AttributeError):
        return {}
    return {'response_error_' + key: value for key in ('code', 'type')
            if isinstance(value := error.get(key), str)
            and re.fullmatch(r'[A-Za-z0-9_.-]{1,64}', value)}


def request_diagnostics(requests):
    """Classify failed acceptance without turning rejected requests into passes."""
    return {
        'http_status_counts': dict(Counter(str(r.get('status', 'missing')) for r in requests)),
        'capacity_rejected_ids': [r['id'] for r in requests if r.get('status') == 503
                                  and r.get('response_error_code') == 'capacity_exceeded'],
        'output_limit_ids': [r['id'] for r in requests if r.get('finish_reason') == 'length'],
        'incomplete_success_stream_ids': [r['id'] for r in requests if r.get('status') == 200
                                         and r.get('stream') and not r.get('done')],
        'forwarding_error_ids': [r['id'] for r in requests if r.get('error')],
        'missing_success_terminal_ids': [r['id'] for r in requests if r.get('status') == 200
                                         and r.get('finish_reason') not in ('stop', 'tool_calls', 'length')],
    }


def prepare(workspace, port, output_cap=1536):
    workspace.mkdir(parents=True, exist_ok=False)
    for name, body in FIXTURE.items():
        path = workspace / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(body, encoding='utf-8')
    python_path = Path(sys.executable).resolve().as_posix()
    verify_command = ('"' + python_path + '"' if ' ' in python_path else python_path) + ' verify.py'
    permissions = {'*': 'deny', 'read': 'allow', 'grep': 'allow', 'glob': 'allow',
                   'skill': {'*': 'deny', 'money-review': 'allow'},
                   'task': {'*': 'deny', 'explore': 'allow'},
                   'external_directory': 'deny'}
    agent_options = {'thinking': {'type': 'disabled'}}
    config = {
        '$schema': 'https://opencode.ai/config.json', 'autoupdate': False,
        'share': 'disabled', 'snapshot': False, 'enabled_providers': ['uart2llm'],
        'model': 'uart2llm/' + MODEL, 'small_model': 'uart2llm/' + MODEL,
        'provider': {'uart2llm': {'npm': '@ai-sdk/openai-compatible',
            'name': 'ESP32 acceptance', 'options': {'baseURL': f'http://127.0.0.1:{port}/v1',
            'apiKey': '{env:UART2LLM_OBSERVER_TOKEN}'},
            'models': {MODEL: {'name': MODEL, 'tool_call': True,
                'limit': {'context': 65536, 'output': output_cap}, 'options': agent_options}}}},
        'permission': permissions, 'lsp': False, 'formatter': False,
        'agent': {
            'plan': {'steps': 10, 'options': agent_options, 'permission': {'edit': 'deny', 'bash': 'deny', 'plan_exit': 'deny'}},
            'build': {'steps': 10, 'options': agent_options, 'permission': {
                'edit': {'*': 'deny', 'price.py': 'allow', '**/price.py': 'allow'},
                'bash': {'*': 'deny', verify_command: 'allow'}}},
            'explore': {'steps': 8, 'model': 'uart2llm/' + MODEL, 'options': agent_options,
                'permission': {'task': 'deny', 'edit': 'deny', 'bash': 'deny'}},
            'research': {'mode': 'primary', 'steps': 4, 'options': agent_options,
                'permission': {'websearch': 'allow', 'task': 'deny', 'edit': 'deny', 'bash': 'deny'}}}}
    (workspace / 'opencode.json').write_text(json.dumps(config, indent=2), encoding='utf-8')
    return verify_command


def summarize_events(raw):
    result = {'tools': [], 'texts': [], 'sessions': [], 'errors': [], 'steps': []}
    for line in raw.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        part = event.get('part', {})
        sid = event.get('sessionID')
        if sid and sid not in result['sessions']:
            result['sessions'].append(sid)
        if event.get('type') == 'tool_use':
            state = part.get('state', {})
            result['tools'].append({'tool': part.get('tool'), 'call_id': part.get('callID'),
                'status': state.get('status'), 'input': state.get('input'),
                'output': str(state.get('output', ''))[:6000], 'error': state.get('error'),
                'metadata': state.get('metadata')})
        elif event.get('type') == 'text':
            result['texts'].append(part.get('text', '')[:6000])
        elif event.get('type') == 'error':
            result['errors'].append(event.get('error', {}))
        elif event.get('type') == 'step_finish':
            result['steps'].append({'reason': part.get('reason'), 'cost': part.get('cost'), 'usage': part.get('tokens')})
    return result


def child_evidence(executable, workspace, env, cases):
    """Export real child session metadata without publishing model reasoning."""
    children = []
    for case in cases:
        for tool in case.get('tools', []):
            sid = (tool.get('metadata') or {}).get('sessionId') if tool['tool'] == 'task' else None
            if not sid or any(c.get('session_id') == sid for c in children):
                continue
            child = {'session_id': sid}
            children.append(child)
            try:
                run = subprocess.run([str(executable), 'export', sid, '--pure'], cwd=workspace,
                    env=env, capture_output=True, text=True, encoding='utf-8', errors='replace', timeout=30, check=True)
                exported = json.loads(run.stdout)
                child['parent_id'] = exported.get('info', {}).get('parentID')
                child['model_messages'], child['tools'] = [], []
                for message in exported.get('messages', []):
                    info = message.get('info', {})
                    if info.get('role') == 'assistant':
                        child['model_messages'].append({key: info.get(key) for key in ('id','agent','modelID','providerID','finish','tokens')})
                    for part in message.get('parts', []):
                        if part.get('type') == 'tool':
                            child['tools'].append({'tool': part.get('tool'), 'status': part.get('state', {}).get('status')})
            except Exception as exc:
                child['error'] = type(exc).__name__
    return children


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--opencode', type=Path, default=ROOT / '.tools/opencode/node_modules/opencode-windows-x64/bin/opencode.exe')
    parser.add_argument('--exe', type=Path, default=default_executable())
    parser.add_argument('--report', type=Path, required=True)
    parser.add_argument('--web-search', action='store_true', help='Also test actual hosted Exa search using the PC network; never counted as offline ESP search')
    parser.add_argument('--parallel', action='store_true', help='Run four simultaneous real model analysis tasks after plan/build')
    parser.add_argument('--output-cap', type=int, default=1536, choices=range(512, 2049),
                        metavar='512..2048', help='Per-request output cap; default 1536 (truncation still fails)')
    args = parser.parse_args()
    if args.report.exists():
        parser.error('report already exists')
    run_id = datetime.now().strftime('%Y%m%d-%H%M%S')
    run = ROOT / '.local/agent-acceptance' / run_id
    run.mkdir(parents=True, exist_ok=False)
    def token(kind):
        return subprocess.run([str(args.exe.resolve()), 'token', kind], capture_output=True, text=True, check=True, timeout=15).stdout.strip()
    api_token, admin_token = token('api'), token('admin')
    observer = Observer(api_token, output_cap=args.output_cap)
    server = ThreadingHTTPServer(('127.0.0.1', 0), observer.handler())
    threading.Thread(target=server.serve_forever, daemon=True).start()
    workspace = run / 'workspace'
    verify_command = prepare(workspace, server.server_port, args.output_cap)
    env = dict(os.environ)
    for key in list(env):
        if any(s in key.upper() for s in ('API_KEY', 'AUTH_TOKEN', 'ACCESS_TOKEN')):
            env.pop(key)
    env.update({'UART2LLM_OBSERVER_TOKEN': observer.client_token,
                'XDG_CONFIG_HOME': str(run / 'config'), 'XDG_DATA_HOME': str(run / 'data'),
                'XDG_CACHE_HOME': str(run / 'cache'), 'XDG_STATE_HOME': str(run / 'state'),
                'OPENCODE_CONFIG': str(workspace / 'opencode.json'),
                'OPENCODE_DISABLE_AUTOUPDATE': 'true', 'OPENCODE_DISABLE_MODELS_FETCH': 'true',
                'OPENCODE_DISABLE_EXTERNAL_SKILLS': 'true', 'OPENCODE_ENABLE_EXA': '1'})
    report = {'started_utc': datetime.now(timezone.utc).isoformat(), 'client': 'OpenCode',
              'client_version': subprocess.check_output([str(args.opencode.resolve()), '--version'], text=True).strip(),
              'client_sha256': digest(args.opencode.read_bytes()), 'host_sha256': digest(args.exe.read_bytes()),
              'model': MODEL, 'physical_transport': 'not independently identified; inspect device telemetry',
              'host_network_isolated': False, 'model_path': 'OpenCode -> loopback observer -> localhost:8765 -> ESP32 TCP -> DeepSeek TLS',
              'limits': {'max_requests': observer.limit, 'max_output_per_request': args.output_cap, 'run_seconds': 1200},
              'cases': [], 'samples': [], 'checks': {}, 'passed': False}
    stopped = threading.Event()
    def sample():
        while not stopped.is_set():
            start = time.monotonic()
            try:
                state = read_json_local(8766, '/admin/v1/state', admin_token)
                device = state.get('device') or {}
                report['samples'].append({'seconds': time.monotonic() - start,
                    'at': datetime.now(timezone.utc).isoformat(), 'host': state.get('host'),
                    'session': device.get('session'), 'memory': device.get('memory'),
                    'network_connected': device.get('network', {}).get('connected'),
                    'connection': state.get('connection', state.get('link'))})
            except Exception as exc:
                report['samples'].append({'error': type(exc).__name__})
            stopped.wait(1)
    monitor = threading.Thread(target=sample, daemon=True)
    monitor.start()

    def execute(name, agent, prompt, session=None):
        started = time.monotonic()
        command = [str(args.opencode.resolve()), 'run', '--pure', '--format', 'json', '--agent', agent,
                   '--model', 'uart2llm/' + MODEL, '--title', 'uart2llm synthetic ' + name]
        if session:
            command += ['--session', session]
        command += [prompt]
        process_env = dict(env)
        # Independent CLI processes need independent SQLite stores. A shared
        # OpenCode server is the alternative; four concurrent CLI initializers
        # against one store can fail before making any model request.
        if name.startswith('parallel-'):
            for key, folder in [('XDG_DATA_HOME', 'data'), ('XDG_CACHE_HOME', 'cache'),
                                ('XDG_STATE_HOME', 'state')]:
                process_env[key] = str(run / name / folder)
        try:
            done = subprocess.run(command, cwd=workspace, env=process_env, capture_output=True, text=True,
                                  encoding='utf-8', errors='replace', timeout=360)
            events = summarize_events(done.stdout)
            # Raw stdout can include reasoning or paths; retain privately only.
            (run / (name + '.jsonl')).write_text(done.stdout, encoding='utf-8')
            (run / (name + '.stderr')).write_text(done.stderr, encoding='utf-8')
            item = {'name': name, 'agent': agent, 'exit_code': done.returncode,
                    'seconds': time.monotonic() - started, **events}
            if done.returncode:
                item['stderr'] = done.stderr[:2000]
        except subprocess.TimeoutExpired:
            item = {'name': name, 'agent': agent, 'error': 'client timeout (360s)', 'seconds': time.monotonic() - started}
        report['cases'].append(item)
        print(name + ': process completed; ' + str(len(item.get('tools', []))) + ' tool events', flush=True)
        return item

    try:
        initial = read_json_local(8766, '/admin/v1/state', admin_token)
        if not (initial.get('device') or {}).get('network', {}).get('connected'):
            raise RuntimeError('configured physical ESP32 Wi-Fi is not connected')
        if (initial.get('llm') or {}).get('active', (initial.get('host') or {}).get('active_requests', 0)):
            raise RuntimeError('gateway already has active requests; run acceptance with exclusive model capacity')
        report['initial_state'] = sanitize(initial)
        before = {name: digest((workspace / name).read_bytes()) for name in FIXTURE}
        observer.phase = 'plan'
        plan = execute('plan', 'plan',
            'Plan an integer-money bug fix. First load money-review with the skill tool, then use grep to locate total_cents. '
            'Delegate one read-only audit to the explore subagent with the task tool: instruct it to read ONLY requirements.md '
            'and price.py by exact path, check invalid inputs and zero quantity, and return at most five bullets ending '
            'with SUBAGENT-AUDIT-DONE. It must not search dependencies or run further agents. '
            'Read requirements.md and price.py yourself. Produce a concrete short implementation and test plan. '
            'Do not edit files or run commands. Include the skill marker and explicitly distinguish your findings from the subagent findings.')
        after_plan = {name: digest((workspace / name).read_bytes()) for name in FIXTURE}
        report['checks']['plan_fixture_unchanged'] = before == after_plan
        tools = plan.get('tools', [])
        report['checks']['plan_skill_loaded'] = any(t['tool'] == 'skill' and t['status'] == 'completed' and 'MONEY-SKILL-LOADED' in t['output'] for t in tools)
        report['checks']['plan_code_search'] = any(t['tool'] == 'grep' and t['status'] == 'completed' and 'total_cents' in t['output'] for t in tools)
        report['checks']['subagent_completed'] = any(t['tool'] == 'task' and t['status'] == 'completed'
            and (t.get('metadata') or {}).get('sessionId') and 'SUBAGENT-AUDIT-DONE' in t['output']
            and 'Maximum steps' not in t['output'] for t in tools)
        if plan.get('errors') or not plan.get('texts'):
            raise RuntimeError('OpenCode plan failed; inspect recorded client errors')
        observer.phase = 'build'
        build = execute('build', 'build',
            'Implement the approved plan now. Only modify price.py. Load the money-review skill if needed, '
            'fix all requirements with integer arithmetic, then actually run this exact allowed command: ' + verify_command +
            '. Do not change verify.py or requirements.md. Report the actual test result.',
            session=plan['sessions'][0] if plan.get('sessions') else None)
        check = subprocess.run([sys.executable, 'verify.py'], cwd=workspace, capture_output=True, text=True, timeout=15)
        report['verification'] = {'exit_code': check.returncode, 'stdout': check.stdout, 'stderr': check.stderr[:4000],
                                  'price_sha256': digest((workspace / 'price.py').read_bytes())}
        report['checks']['build_fixture_tests_pass'] = check.returncode == 0
        report['checks']['protected_fixture_unchanged'] = all(digest((workspace / n).read_bytes()) == h for n,h in before.items() if n != 'price.py')
        report['checks']['client_executed_tests'] = any(t['tool'] == 'bash' and t['status'] == 'completed' and 'VERIFIED: 11' in t['output'] for t in build.get('tools', []))
        if args.web_search:
            observer.phase = 'web-search'
            search = execute('web-search', 'research',
                'Use the websearch tool to find the official Python documentation on decimal ROUND_HALF_UP. '
                'Perform a real search; do not answer from memory. Cite the returned official URL and briefly explain ties rounding. '
                'Do not fetch arbitrary pages, edit files, or run commands. If search fails, say so explicitly.')
            report['checks']['actual_web_search'] = any(t['tool'] == 'websearch' and t['status'] == 'completed' and 'https://docs.python.org' in t['output'] for t in search.get('tools', []))
            report['web_search_network_path'] = 'OpenCode hosted search tool uses PC network directly; NOT ESP32/offline capability'
        if args.parallel:
            observer.phase = 'four-model-load'
            def worker(i):
                return execute('parallel-' + str(i), 'plan',
                    'Read price.py and requirements.md with the read tool. Independently audit ' +
                    ['large integers and rounding', 'input validation and booleans', 'zero quantity and full discount', 'test gaps and regression risks'][i] +
                    '. Produce 400 to 600 words with concrete examples and calculations. Do not delegate, search the web, edit or run commands.')
            with ThreadPoolExecutor(max_workers=4) as pool:
                runs = list(pool.map(worker, range(4)))
            report['checks']['four_agent_runs_complete'] = all(v.get('exit_code') == 0 and v.get('texts') and not v.get('errors') for v in runs)
    except Exception as exc:
        report['error'] = str(exc)
    finally:
        report['child_sessions'] = child_evidence(args.opencode.resolve(), workspace, env, report['cases'])
        report['checks']['subagent_model_evidence'] = bool(report['child_sessions']) and all(
            c.get('parent_id') and c.get('model_messages') and c.get('tools') and not c.get('error')
            and all(m.get('providerID') == 'uart2llm' and m.get('modelID') == MODEL for m in c['model_messages'])
            for c in report['child_sessions'])
        stopped.set()
        monitor.join(timeout=15)
        server.shutdown()
        server.server_close()
        report['requests'], report['observer_rejections'] = observer.requests, observer.rejected
        report['observer_peak_requests'] = observer.peak
        report['request_diagnostics'] = request_diagnostics(observer.requests)
        try:
            # Preserve cleanup occupancy evidence; never count an HTTP stream
            # as a free device channel merely because [DONE] already arrived.
            drain_deadline = time.monotonic() + 10
            while True:
                final = read_json_local(8766, '/admin/v1/state', admin_token)
                active = (final.get('llm') or {}).get('active', 0)
                if active == 0 or time.monotonic() >= drain_deadline:
                    break
                time.sleep(0.1)
            report['final_state'] = sanitize(final)
            initial_count = ((report.get('initial_state') or {}).get('host') or {}).get('requests')
            final_count = (final.get('host') or {}).get('requests')
            if type(initial_count) is int and type(final_count) is int:
                report['request_diagnostics']['host_admitted_request_delta'] = final_count - initial_count
                report['request_diagnostics']['observer_http_success_count'] = sum(r.get('status') == 200 for r in observer.requests)
            report['checks']['gateway_idle_after_run'] = active == 0
        except Exception as exc:
            report['final_state_error'] = type(exc).__name__
            report['checks']['gateway_idle_after_run'] = False
        report['checks']['all_streams_complete'] = bool(observer.requests) and all(r.get('status') == 200 and (r['done'] if r['stream'] else True) and not r.get('error') for r in observer.requests)
        report['checks']['no_output_truncation'] = bool(observer.requests) and all(r.get('finish_reason') in ('stop','tool_calls') for r in observer.requests)
        calls = [t for r in observer.requests for t in r['tool_calls'].values()]
        report['checks']['streamed_tool_arguments_valid'] = bool(calls) and all(t.get('valid_json') for t in calls)
        returned = set(x for r in observer.requests for x in r['tool_result_ids'])
        report['checks']['tool_results_round_trip'] = bool(calls) and all(t['id'] in returned for t in calls)
        report['checks']['telemetry_available_under_load'] = bool(report['samples']) and all('error' not in s and s.get('network_connected') for s in report['samples'])
        sessions = {s.get('session') for s in report['samples'] if s.get('session') is not None}
        report['checks']['physical_session_stable'] = len(sessions) == 1
        if args.parallel:
            report['checks']['four_actual_requests_observed'] = any((s.get('host') or {}).get('active_requests') == 4 for s in report['samples'])
        totals = Counter()
        for r in observer.requests:
            for k, v in (r.get('usage') or {}).items():
                if type(v) is int:
                    totals[k] += v
        report['usage_totals'] = dict(totals)
        report['finished_utc'] = datetime.now(timezone.utc).isoformat()
        report['passed'] = bool(report['checks']) and all(report['checks'].values()) and not report.get('error') and not observer.rejected
        for case in report['cases']:
            for tool in case.get('tools', []):
                if tool['tool'] == 'websearch':
                    output = tool.pop('output')
                    tool['output_sha256'] = digest(output.encode())
                    tool['output_bytes'] = len(output.encode())
                    tool['result_urls'] = list(dict.fromkeys(re.findall(r'https?://[^\s<>]+', output)))
        # No credentials from either direction may enter the public artifact.
        public = json.dumps(sanitize(report), ensure_ascii=False, indent=2)
        for secret in (api_token, admin_token, observer.client_token):
            public = public.replace(secret, '[redacted]')
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(public, encoding='utf-8')
        print(json.dumps({'passed': report['passed'], 'checks': report['checks'], 'model_requests': len(observer.requests), 'usage': totals}, ensure_ascii=False), flush=True)
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
