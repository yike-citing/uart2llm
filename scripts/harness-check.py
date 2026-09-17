#!/usr/bin/env python3
"""Bounded installed DeepSeek Harness Web acceptance (not the SDK or OpenCode).

Uses the already authenticated local Web RPC and /api/remote.mux event carrier.
No credentials, model reasoning or message/tool bodies are written to reports.
Only sessions created by this run may be cancelled by this run.
"""
import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import http.client
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import threading
import time
import urllib.request
import uuid

import websocket
from harness_support import Harness

ROOT = Path(__file__).resolve().parent.parent
PROVIDER, MODEL = 'uart2llm', 'deepseek-flash'
REASONING = 'off'
MARKERS = ('UART2LLM_HARNESS_SMOKE_OK', 'MONEY-SKILL-LOADED', 'SUBAGENT-AUDIT-DONE',
           'VERIFIED: 11', 'HARNESS-WORKFLOW-DONE', 'HARNESS-PARALLEL-DONE',
           'HARNESS-ADVANCED-DONE', 'HARNESS-OWN-JOB-READY', 'HARNESS-ADVANCED-VERIFIED',
           'HARNESS-CHILD-READY', 'HARNESS-CHILD-RECOVERED', 'HARNESS-FORK-VERIFIED',
           'HARNESS-JS-VERIFIED', 'HARNESS-RALPH-VERIFIED', 'HARNESS-COORDINATION-DONE',
           'HARNESS-REASONING-VERIFIED: 7321')

COORDINATION_SCRIPT = ("const values = [2,3,5,7]; const sum = values.reduce((a,b)=>a+b,0); "
    "const squares = values.reduce((a,b)=>a+b*b,0); "
    "return {marker:'HARNESS-JS-VERIFIED',sum,squares};")


def sha(data):
    return hashlib.sha256(data).hexdigest()


def code(value):
    return value if isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9_./:-]{1,100}', value) else None


def safe_error(error):
    # Server exception messages can contain request headers/content; never emit
    # them. Stable error codes and class names are sufficient for this artifact.
    result = {'class': type(error).__name__}
    try:
        value = json.loads(str(error))
        if isinstance(value, dict) and code(value.get('code')):
            result['code'] = value['code']
    except ValueError:
        pass
    return result


def text_blocks(content):
    for block in content if isinstance(content, list) else []:
        if not isinstance(block, dict):
            continue
        if block.get('type') == 'text' and isinstance(block.get('text'), str):
            yield block['text']
        elif block.get('type') == 'tool-result':
            yield from text_blocks(block.get('content'))


class Follow:
    def __init__(self, harness, session_id, address=None):
        self.harness, self.session_id = harness, session_id
        self.address = address or {'kind': 'session', 'sessionId': session_id}
        self.stream_id = str(uuid.uuid4())
        self.events, self.tools, self.attempts = [], {}, {}
        self.counts, self.usage, self.markers = Counter(), Counter(), set()
        self.cursor = -1
        self.turn_ends = []
        self.children = []
        self.available_tools = set()
        self.gaps = []
        self.text_bytes = 0
        self.received_bytes = 0
        self.frame_count = 0
        self.ws = None
        self.interaction_stream_id = None
        self.event_client_id = None
        self.review_guard = None
        self.plan_reviews = []
        self.fixture_question = None
        self.question_answers = []
        self.goal_states = []
        self.todo_states = []
        self.owned_jobs = set()

    def subscribe_reviews(self, guard, deadline):
        self.review_guard = guard
        self.interaction_stream_id = str(uuid.uuid4())
        self.ws.send(json.dumps({'type': 'open', 'streamId': self.interaction_stream_id,
                                'endpoint': '$events', 'payload': {'args': {}}}))
        while not self.event_client_id:
            self.receive(deadline)

    def interaction(self, value):
        if value.get('type') == 'ready':
            self.event_client_id = value['clientId']
            return
        if value.get('type') != 'waterfall':
            return
        outcome = {'kind': 'next'}
        owned = value.get('agentId') == self.session_id
        request = value.get('request') or {}
        questions = request.get('questions', [])
        review = (owned and value.get('event') == 'user-questions/request'
                  and len(questions) == 1 and questions[0].get('id') == 'plan-review'
                  and questions[0].get('intent', {}).get('kind') == 'plan-review'
                  and questions[0].get('intent', {}).get('approve') == 'Approve'
                  and any(o.get('label') == 'Approve' for o in questions[0].get('options', [])))
        fixture_question = (owned and self.fixture_question is not None
            and value.get('event') == 'user-questions/request' and len(questions) == 1
            and questions[0].get('id') == self.fixture_question['id']
            and questions[0].get('question') == self.fixture_question['question']
            and [o.get('label') for o in questions[0].get('options', [])] == ['Blue', 'Green']
            and not questions[0].get('intent') and not questions[0].get('multiSelect')
            and not self.question_answers)
        if fixture_question:
            outcome = {'kind': 'result', 'value': {'answers': [
                {'id': self.fixture_question['id'], 'selected': ['Blue']}]}}
            self.question_answers.append({'id': self.fixture_question['id'], 'selected': 'Blue',
                                          'exact_question_matched': True})
        if review:
            detail = questions[0].get('detail', '')
            # The only automated human decision is the user-authorized synthetic
            # plan review. No other session, permission, or question is approved.
            unchanged = self.review_guard() if self.review_guard else False
            scoped = (isinstance(detail, str) and detail.lstrip().startswith('#')
                      and 'price.py' in detail and 'verify.py' in detail)
            allowed = unchanged and scoped and not self.plan_reviews
            self.plan_reviews.append({'fixture_unchanged': unchanged, 'plan_scope_valid': scoped,
                'plan_sha256': sha(str(detail).encode()), 'plan_bytes': len(str(detail).encode()),
                'approved': allowed})
            if allowed:
                outcome = {'kind': 'result', 'value': {'answers': [{'id': 'plan-review', 'selected': ['Approve']}]}}
        self.harness.rpc('$events/result', clientId=self.event_client_id,
                         eventId=value['eventId'], outcome=outcome)
        if owned and not fixture_question and (not review or not self.plan_reviews[-1]['approved']):
            raise RuntimeError('unexpected_or_unsafe_fixture_interaction')

    def open(self, deadline):
        cookie_request = urllib.request.Request(self.harness.base + '/api/remote.mux')
        self.harness.cookies.add_cookie_header(cookie_request)
        self.ws = websocket.create_connection(
            self.harness.base.replace('http://', 'ws://', 1) + '/api/remote.mux',
            cookie=cookie_request.get_header('Cookie'), origin=self.harness.base,
            timeout=min(10, max(1, deadline-time.monotonic())),
            http_no_proxy=['127.0.0.1', 'localhost'])
        self.ws.settimeout(1)
        self.ws.send(json.dumps({'type': 'open', 'streamId': self.stream_id,
                                'endpoint': 'session/follow', 'payload': {'args': {'request': {
                                    'address': self.address, 'maxMessages': 100, 'assistantStream': True}}}}))
        while self.cursor < 0:
            self.receive(deadline)

    def consume(self, value):
        kind = value.get('type')
        if kind == 'snapshot':
            if value.get('hasMore'):
                raise RuntimeError('opening_history_exceeds_bound')
            for item in value.get('records', []):
                if item.get('type') == 'event':
                    self.event(item['event'], opening=True)
            self.cursor = value['cursor']
            if value.get('assistantStream', {}).get('activeAttempt'):
                raise RuntimeError('unexpected_active_attempt_at_open')
        elif kind == 'event':
            self.event(value['event'])
        elif kind == 'assistant-stream':
            f = value['frame']
            attempt = self.attempts.setdefault(f['attemptId'], {'chunks': 0, 'started': False, 'ended': False})
            if f['type'] == 'start':
                attempt['started'] = True
            elif f['type'] == 'chunk':
                if f.get('index') != attempt['chunks']:
                    self.gaps.append('assistant_chunk_index')
                attempt['chunks'] += 1
                self.counts['live_assistant_chunks'] += 1
            elif f['type'] == 'end':
                if f.get('index') != attempt['chunks']:
                    self.gaps.append('assistant_end_index')
                attempt['ended'] = True
                attempt['outcome'] = f.get('outcome', {}).get('kind')

    def event(self, event, opening=False):
        seq, kind, data = event['seq'], event['type'], event.get('data', {})
        if seq <= self.cursor:
            return
        if not opening and self.cursor >= 0 and seq != self.cursor+1:
            self.gaps.append('durable_event_sequence')
        self.cursor = seq
        self.counts[kind] += 1
        brief = {'seq': seq, 'type': kind, 'time': event.get('time')}
        if kind == 'request/header':
            conf = data.get('header', {}).get('config', {})
            brief['route'] = {k: conf[k] for k in ('provider', 'model', 'reasoningEffort', 'maxTokens') if k in conf}
            self.available_tools.update(t['name'] for t in data.get('header', {}).get('tools', [])
                                        if isinstance(t, dict) and code(t.get('name')))
        elif kind == 'subagent/catalog':
            child = {'child_id': data.get('childId'), 'mode': data.get('mode')}
            if child['child_id'] and child not in self.children:
                self.children.append(child)
        elif kind == 'plan/mode':
            brief['active'] = data.get('active')
        elif kind == 'tool-workflow/run-end':
            brief['stop_reason'] = code(data.get('stopReason'))
        elif kind == 'goal/change':
            goal = data.get('goal') or {}
            state = {k: goal.get(k) for k in ('id', 'revision', 'phase', 'maxGoalRounds')}
            self.goal_states.append(state)
        elif kind == 'todo/write':
            self.todo_states.append({'count': len(data.get('todos', [])),
                'statuses': [code(t.get('status')) for t in data.get('todos', [])]})
        elif kind == 'turn/end':
            reason = data.get('reason', {})
            brief['reason'] = code(reason.get('kind'))
            brief['error_code'] = code(reason.get('error', {}).get('code'))
            self.turn_ends.append(brief)
        elif kind == 'assistant/message':
            self.counts['reasoning_blocks'] += sum(b.get('type') == 'reasoning'
                for b in data.get('message', {}).get('content', []) if isinstance(b, dict))
            for text in text_blocks(data.get('message', {}).get('content')):
                self.text_bytes += len(text.encode())
                self.markers.update(m for m in MARKERS if m in text)
            for k, v in (data.get('usage') or {}).items():
                if type(v) in (int, float) and v >= 0:
                    self.usage[k] += v
            brief['interrupted'] = data.get('interrupted', False)
        elif kind == 'tool/call':
            arguments = data.get('arguments', '')
            try:
                parsed = json.loads(arguments)
                valid = isinstance(parsed, dict)
            except (ValueError, TypeError):
                parsed, valid = {}, False
            tool = {'call_id': data.get('callId'), 'name': code(data.get('name')),
                    'arguments_sha256': sha(str(arguments).encode()), 'arguments_valid_json': valid,
                    'result_received': False}
            # Argument keys show real capability usage without copying paths,
            # commands, model text, credentials, or file content into the report.
            tool['argument_keys'] = [k for k in parsed if code(k)]
            if data.get('name') == 'structured_output' and parsed.get('status') == 'complete':
                # Ralph completes through this real tool, not an assistant text
                # message. Retain only known fixture markers from its evidence.
                evidence = json.dumps(parsed.get('evidence', ''), ensure_ascii=False)
                self.markers.update(m for m in MARKERS if m in evidence)
            if tool['name'] == 'create_goal':
                tool['goal_round_cap'] = parsed.get('max_goal_rounds')
            if tool['name'] == 'update_goal':
                tool['goal_action'] = code(parsed.get('action'))
            if tool['name'] == 'ralph':
                tool['ralph_round_cap'] = parsed.get('maxRounds')
            if tool['name'] == 'workflow':
                tool['exact_pure_js_script'] = parsed.get('script') == COORDINATION_SCRIPT
            if tool['name'] in ('send_message', 'interrupt_agent'):
                tool['targets_own_child'] = any(c['child_id'] == parsed.get('agent_id')
                                              and c['mode'] == 'continuable' for c in self.children)
            if tool['name'] == 'pwsh':
                tool['background'] = parsed.get('run_in_background') is True
            if tool['name'] in ('job_output', 'job_kill'):
                tool['targets_own_job'] = parsed.get('job_id') in self.owned_jobs
            self.tools[data['callId']] = tool
        elif kind == 'tool/result':
            for block in data.get('message', {}).get('content', []):
                if block.get('type') != 'tool-result':
                    continue
                tool = self.tools.setdefault(block.get('toolCallId'), {'call_id': block.get('toolCallId')})
                tool['result_received'], tool['is_error'] = True, bool(block.get('isError'))
                texts = '\n'.join(text_blocks(block.get('content')))
                tool['result_text_bytes'], tool['result_sha256'] = len(texts.encode()), sha(texts.encode())
                tool['markers'] = [m for m in MARKERS if m in texts]
                self.markers.update(tool['markers'])
                if tool.get('name') == 'pwsh' and tool.get('background') and not tool['is_error']:
                    match = re.fullmatch(r'\s*started background job ([A-Za-z0-9_-]+)\s*', texts)
                    if match:
                        self.owned_jobs.add(match[1])
                if tool.get('name') in ('job_output', 'job_kill'):
                    tool['job_status_killed'] = '[status: killed' in texts
                    tool['cancellation_requested'] = 'requested cancellation of job ' in texts
                if tool.get('name') in ('create_goal', 'get_goal', 'update_goal'):
                    try:
                        result_goal = (json.loads(texts).get('goal') or {})
                        tool['goal_result_phase'] = code(result_goal.get('phase'))
                    except (ValueError, AttributeError):
                        pass
                if tool.get('name') == 'workflow':
                    try:
                        value = json.loads(texts.split('\nReturn value:\n', 1)[1])
                        tool['pure_js_result_verified'] = value == {
                            'marker': 'HARNESS-JS-VERIFIED', 'sum': 17, 'squares': 87}
                    except (ValueError, IndexError):
                        tool['pure_js_result_verified'] = False
                if tool.get('name') == 'ralph':
                    tool['one_round_completion'] = texts.startswith('Ralph worker reported completion after 1 round.')
                    try:
                        value = json.loads(texts.split('\nFinal report:\n', 1)[1])
                        tool['structured_report_verified'] = (value.get('status') == 'complete'
                            and value.get('nextSteps') == [] and value.get('blocker') == ''
                            and 'HARNESS-RALPH-VERIFIED' in json.dumps(value.get('evidence', [])))
                    except (ValueError, IndexError, AttributeError):
                        tool['structured_report_verified'] = False
            brief['error_code'] = code(data.get('error', {}).get('code'))
        self.events.append(brief)
        if len(self.events) > 15000 or self.counts['step/start'] > 48:
            raise RuntimeError('session_event_or_model_request_bound_exceeded')

    def receive(self, deadline):
        if time.monotonic() >= deadline:
            raise TimeoutError('harness_phase_deadline')
        try:
            raw = self.ws.recv()
        except websocket.WebSocketTimeoutException:
            return
        if not raw:
            raise ConnectionError('harness_websocket_closed')
        self.received_bytes += len(raw)
        self.frame_count += 1
        if self.received_bytes > 32*1024*1024 or self.frame_count > 100000:
            raise RuntimeError('websocket_observation_bound_exceeded')
        frame = json.loads(raw)
        if frame.get('streamId') not in (self.stream_id, self.interaction_stream_id):
            return
        if frame['type'] == 'error':
            raise RuntimeError(json.dumps({'code': code(frame.get('error', {}).get('code'))}))
        if frame['type'] == 'end':
            raise ConnectionError('harness_follow_ended')
        if frame['type'] == 'item':
            if frame.get('streamId') == self.interaction_stream_id:
                self.interaction(frame.get('value', {}))
            else:
                self.consume(frame.get('value', {}))

    def wait_turn(self, before, deadline):
        while len(self.turn_ends) <= before:
            self.receive(deadline)
        # The durable assistant terminal and live end can be adjacent frames.
        while any(not a['ended'] for a in self.attempts.values()):
            self.receive(deadline)
        return self.turn_ends[-1]['reason']

    def summary(self):
        return {'session_id': self.session_id, 'cursor': self.cursor, 'event_counts': dict(self.counts),
                'events': self.events, 'tools': list(self.tools.values()), 'attempts': self.attempts,
                'usage': dict(self.usage), 'markers': sorted(self.markers), 'text_bytes': self.text_bytes,
                'sequence_errors': self.gaps, 'websocket_frames': self.frame_count,
                'websocket_bytes': self.received_bytes, 'turn_ends': self.turn_ends,
                'children': self.children, 'available_tools': sorted(self.available_tools),
                'plan_reviews': self.plan_reviews, 'question_answers': self.question_answers,
                'goal_states': self.goal_states, 'todo_states': self.todo_states,
                'owned_background_jobs': len(self.owned_jobs)}

    def close(self):
        if self.ws:
            try:
                self.ws.send(json.dumps({'type': 'cancel', 'streamId': self.stream_id}))
            except Exception:
                pass
            self.ws.close()


class Runner:
    def __init__(self, h, workspace, deadline):
        self.h, self.workspace, self.deadline = h, workspace, deadline
        self.sessions, self.follows = [], []
        self.stop_files = []
        self.cleanup_children = False

    def create(self):
        value = self.h.rpc('session/create', request={'cwd': str(self.workspace), 'agentPreset': 'standard'})
        sid = value['sessionId']
        self.sessions.append(sid)
        self.h.rpc('session/selectModel', request={'sessionId': sid, 'provider': PROVIDER,
                                                   'model': MODEL, 'reasoningEffort': REASONING})
        f = Follow(self.h, sid)
        self.follows.append(f)
        f.open(self.deadline)
        return f

    def prompt(self, f, text):
        before = len(f.turn_ends)
        self.enqueue(f, text)
        return f.wait_turn(before, self.deadline)

    def enqueue(self, f, text):
        self.h.rpc('session/prompt', request={'requestId': str(uuid.uuid4()), 'sessionId': f.session_id,
                    'mode': 'queue', 'clientTimeZone': 'Asia/Shanghai', 'content': [{'type': 'text', 'text': text}]})

    def command(self, f, line):
        result = self.h.rpc('commands/execute', agentId=f.session_id, line=line, submittedAttachments=[])
        if not isinstance(result, dict) or result.get('result', {}).get('kind') != 'success':
            raise RuntimeError('harness_command_failed')
        return result

    def inspect_children(self, parent):
        children = []
        for item in parent.children:
            f = Follow(self.h, item['child_id'], {'kind': 'subagent', 'parentSessionId': parent.session_id,
                         'childSessionId': item['child_id'], 'mode': item['mode']})
            self.follows.append(f)
            f.open(self.deadline)
            children.append(f)
        return children

    def cleanup(self):
        errors = []
        for stop in self.stop_files:
            try:
                stop.write_text('stop', encoding='ascii')
            except Exception as error:
                errors.append({'fixture_worker_stop': safe_error(error)})
        for sid in self.sessions:
            try:
                self.h.rpc('session/cancel', request={'sessionId': sid})
            except Exception as error:
                errors.append({'session_id': sid, **safe_error(error)})
            if self.cleanup_children:
                try:
                    catalog = self.h.rpc('subagents/list', parentSessionId=sid)
                    for child in catalog.get('entries', []):
                        if child.get('kind') == 'child' and child.get('mode') == 'continuable':
                            self.h.rpc('subagents/interruptByParent', childSessionId=child['id'],
                                       parentSessionId=sid, mode='continuable')
                except Exception as error:
                    errors.append({'parent_session_id': sid, 'child_cleanup': safe_error(error)})
        for f in self.follows:
            f.close()
        return errors


def fixture(workspace):
    source = ROOT / 'scripts/opencode-check.py'
    spec = importlib.util.spec_from_file_location('fixture_source', source)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    for name, value in module.FIXTURE.items():
        name = name.replace('.opencode/skills/', '.dsh/skills/')
        path = workspace / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value, encoding='utf-8')
    (workspace / 'AGENTS.md').write_text(
        'Synthetic acceptance workspace. Work only inside this directory. Never read credentials, '
        'home directories or external repositories. Only price.py may be changed. '
        'Read requirements.md and verify.py; use integer arithmetic. '
        'Do not install packages, contact networks or spawn background commands. '
        'A requested read-only subagent may read only requirements.md and price.py.\n', encoding='utf-8')


def successful_tools(f):
    return {t.get('name') for t in f.tools.values() if t.get('result_received') and not t.get('is_error')}


def advanced_fixture(workspace):
    files = {
        'deliverable.md': '# Synthetic result\nColor: UNSET\nCount: 0\n',
        'worker.py': (
            'from pathlib import Path\nimport time\n'
            "root = Path(__file__).resolve().parent\n"
            "(root / 'worker.ready').write_text('ready', encoding='ascii')\n"
            "print('HARNESS-OWN-JOB-READY', flush=True)\n"
            'deadline = time.monotonic() + 180\n'
            "while time.monotonic() < deadline and not (root / 'worker.stop').exists():\n"
            '    time.sleep(0.1)\n'),
        'verify_advanced.py': (
            'from pathlib import Path\n'
            "root = Path(__file__).resolve().parent\n"
            "assert (root / 'deliverable.md').read_text(encoding='utf-8') == '# Synthetic result\\nColor: Blue\\nCount: 3\\n'\n"
            "print('HARNESS-ADVANCED-VERIFIED')\n"),
        'AGENTS.md': (
            'This is an isolated synthetic acceptance workspace. Work only here. '
            'Only deliverable.md may be edited, using the edit tool. Never read credentials, '
            'home directories or external repositories. Do not contact networks, install packages, '
            'change global security settings, launch subagents, or modify worker.py, verify_advanced.py or AGENTS.md. '
            'Explicitly permitted for this test only: start exactly one worker.py with background pwsh, '
            'list this session jobs, read and kill only that returned job ID, and run verify_advanced.py. '
            'The worker self-expires after 180 seconds and the test owner can stop it via worker.stop. '
            'The human authorizes a single same-session goal, capped at max_goal_rounds=1, '
            'to complete and verify the synthetic deliverable. Mark complete only after verification.\n')}
    for name, value in files.items():
        (workspace/name).write_text(value, encoding='utf-8')


def coordination_fixture(workspace):
    (workspace/'facts.txt').write_text('VALUES=2,3,5,7\nSUM=17\nSQUARES=87\n', encoding='utf-8')
    (workspace/'AGENTS.md').write_text(
        'Isolated coordination acceptance workspace. All files are read-only. '
        'Read only facts.txt and this AGENTS.md. Never inspect credentials, global configuration, '
        'home directories or external repositories. No network, shell, file changes or permissions changes. '
        'The parent may create exactly one continuable subagent, one subagent_fork, and one Ralph worker '
        'with maxRounds=1; workers must never delegate. All workers inherit the local provider/model. '
        'Only the parent may message or interrupt the continuable child created in this test. '
        'Workflow is authorized only for the requested pure JavaScript arithmetic, with no agents or I/O.\n',
        encoding='utf-8')


def coordination(runner, report):
    runner.cleanup_children = True
    workspace = runner.workspace
    before = {p.name: sha(p.read_bytes()) for p in workspace.iterdir() if p.is_file()}
    f = runner.create()
    # Subscribe to reject unknown own-session prompts, never approving security
    # permissions. This phase requires neither question answers nor plan review.
    f.subscribe_reviews(None, runner.deadline)
    first = runner.prompt(f,
        'Run a bounded read-only coordination acceptance in this synthetic workspace. '
        'You are authorized to use native subagent, list_agents, send_message and interrupt_agent. '
        'Do not use shell, edit files, network, goals, questions, or change any permissions. '
        '1. Create exactly one continuable child via subagent, run_in_background:true, '
        'description "fixture coordinator". Its prompt: Read only facts.txt in this current directory, '
        'verify its sum, do not delegate or contact networks, and finish briefly with HARNESS-CHILD-READY. '
        'Inherit the local provider/model.\n'
        '2. Call list_agents to discover the exact ID of that child. Wait for its HARNESS-CHILD-READY '
        'completion notice before assigning the counting task; you may independently read facts.txt meanwhile. '
        'Use send_message to this child only: '
        '"Cancellation test: do not use tools or delegate. Print integers 1 through 2000 one per line." '
        'Then in your next step immediately interrupt_agent targeting this same child, stopping the counting.\n'
        '3. Use send_message to that same child to start recovery: "The counting task is cancelled. '
        'Read only facts.txt, confirm SUM=17 and SQUARES=87, then finish with HARNESS-CHILD-RECOVERED. '
        'No further agents, shell, file writes or networking." '
        'Call list_agents again. Do not repeatedly poll. Finish briefly; the next task will perform '
        'independent work while recovery settles. Never message or interrupt an unrelated agent.')
    second = runner.prompt(f,
        'Continue the read-only synthetic test. Do not create any further continuable agents. '
        'I explicitly request one subagent_fork, one JavaScript workflow and one Ralph loop.\n'
        '1. Call subagent_fork with run_in_background:false for this sole task: read only facts.txt, '
        'verify SUM=17 and SQUARES=87, then finish HARNESS-FORK-VERIFIED. Do not inherit the parent task '
        'as work to repeat: no delegation, workflow, Ralph, shell, questions, changes or networking.\n'
        '2. Call workflow with meta {"name":"fixture-arithmetic","description":"Read-only pure arithmetic"}, '
        'and this EXACT plain JavaScript script, without changing whitespace or adding code:\n' + COORDINATION_SCRIPT + '\n'
        'Verify the returned sum is 17 and squares 87.\n'
        '3. Call ralph with maxRounds exactly 1. Objective: Read ONLY facts.txt in the current synthetic '
        'workspace and independently verify sum 17 and sum of squares 87. Do not write files, run shell, '
        'access networks, ask questions or delegate further. Complete this one read-only objective. '
        'The structured report must contain evidence with HARNESS-RALPH-VERIFIED and verified arithmetic, '
        'status complete, no nextSteps and empty blocker if successful.\n'
        '4. Call list_agents once to inspect the existing continuable child after its recovery. '
        'Finish with short actual results and HARNESS-COORDINATION-DONE. Do not claim failed tools succeeded.')
    # A final child message can settle just after the parent's final text. Await
    # actual inactivity through the normal catalog before opening cold history.
    child_deadline = min(runner.deadline, time.monotonic()+45)
    catalog = {}
    while time.monotonic() < child_deadline:
        catalog = runner.h.rpc('subagents/list', parentSessionId=f.session_id)
        if all(c.get('activity') != 'running' for c in catalog.get('entries', [])):
            break
        time.sleep(0.5)
    children = runner.inspect_children(f)
    continued = [c for c in children if c.address.get('mode') == 'continuable']
    one_shots = [c for c in children if c.address.get('mode') == 'one-shot']
    tool_records = list(f.tools.values())
    required = {'subagent', 'list_agents', 'send_message', 'interrupt_agent', 'subagent_fork', 'workflow', 'ralph'}
    completed = successful_tools(f)
    controls = [t for t in tool_records if t.get('name') in ('send_message', 'interrupt_agent')]
    report['coordination'] = {'required_tools': sorted(required), 'missing_tools': sorted(required-completed),
        'continuable_children': len(continued), 'one_shot_children': len(one_shots),
        'child_activity': [{'mode': c.get('mode'), 'activity': c.get('activity')}
                           for c in catalog.get('entries', [])],
        'deliberately_uncovered': ['read_image', 'PTC', 'web_search', 'web_fetch']}
    report['checks'].update({'coordination_turns_completed': first == second == 'completed',
        'coordination_required_tools_completed': required <= completed,
        'coordination_marker_received': 'HARNESS-COORDINATION-DONE' in f.markers,
        'only_own_child_controlled': bool(controls) and all(t.get('targets_own_child') for t in controls),
        'one_continuable_child': len(continued) == 1,
        'exact_child_budget': len(children) == 3 and len(one_shots) == 2,
        'child_initial_real_read': any('HARNESS-CHILD-READY' in c.markers and 'read' in successful_tools(c) for c in continued),
        'child_actual_interruption': any(any(e['reason'] in ('aborted', 'interrupted') for e in c.turn_ends) for c in continued),
        'child_recovery_completed': any('HARNESS-CHILD-RECOVERED' in c.markers
            and c.turn_ends and c.turn_ends[-1]['reason'] == 'completed' for c in continued),
        'fork_real_model_read': any('HARNESS-FORK-VERIFIED' in c.markers
            and 'read' in successful_tools(c) and c.counts['request/header'] > 0 for c in one_shots),
        'workflow_exact_pure_script': any(t.get('exact_pure_js_script') for t in tool_records),
        'workflow_computation_verified': any(t.get('pure_js_result_verified') for t in tool_records),
        'workflow_durable_completion': any(e['type'] == 'tool-workflow/run-end'
            and e.get('stop_reason') == 'completed' for e in f.events),
        'single_ralph_one_round': len([t for t in tool_records if t.get('name') == 'ralph']) == 1
            and all(t.get('ralph_round_cap') == 1 for t in tool_records if t.get('name') == 'ralph'),
        'ralph_structured_completion': any(t.get('one_round_completion')
            and t.get('structured_report_verified') for t in tool_records),
        'ralph_real_model_read': any('HARNESS-RALPH-VERIFIED' in c.markers
            and 'read' in successful_tools(c) and c.counts['request/header'] > 0 for c in one_shots),
        'all_children_settled': all(c.get('activity') == 'inactive' for c in catalog.get('entries', [])),
        'fixture_unchanged': {p.name for p in workspace.iterdir()} == set(before)
            and all(sha((workspace/n).read_bytes()) == digest for n, digest in before.items()),
        'no_mutating_or_network_tools': not ({'write','edit','pwsh','web_search','web_fetch'} &
            set().union(*(set(t.get('name') for t in c.tools.values()) for c in [f]+children)))})


def advanced(runner, report):
    workspace = runner.workspace
    protected = {p.name: sha(p.read_bytes()) for p in workspace.iterdir()
                 if p.is_file() and p.name != 'deliverable.md'}
    runner.stop_files.append(workspace/'worker.stop')
    f = runner.create()
    question_id = 'fixture-color-' + uuid.uuid4().hex[:12]
    f.fixture_question = {'id': question_id, 'question': 'Which synthetic fixture color should be used?'}
    f.subscribe_reviews(None, runner.deadline)
    python = "& '" + Path(sys.executable).resolve().as_posix().replace("'", "''") + "' "
    reason = runner.prompt(f,
        'Run this bounded advanced acceptance task only in the current synthetic workspace. '
        'I explicitly authorize creating one goal to complete and verify deliverable.md; '
        'set create_goal max_goal_rounds exactly 1. No subagents or network calls. '
        'Perform these actual native tools, not simulated results:\n'
        '1. Call glob to locate *.md, read deliverable.md, and todo_write with a short task list.\n'
        '2. Call ask_user_question with exactly one question: id ' + question_id + ', header Color, '
        'question exactly "Which synthetic fixture color should be used?", options in order '
        'Blue (description: Use blue in the synthetic file), Green (description: Use green in the synthetic file). '
        'Single choice, no intent or additional questions. Wait for the real answer.\n'
        '3. Call create_goal with the authorized objective and max_goal_rounds:1, then get_goal. '
        'Use edit, not write or shell, to replace Color: UNSET with the selected Color: Blue and Count: 0 '
        'with Count: 3 in deliverable.md, preserving every other byte.\n'
        '4. Start exactly one background pwsh with command ' + python + 'worker.py and run_in_background:true. '
        'Track the returned job ID; use job_list, job_output for that exact ID to collect its READY marker, '
        'then job_kill for that exact ID. Finally job_output with wait:true and timeout_ms:10000 '
        'to verify status killed. Never kill any other job. Do not wait for its natural 180-second expiry.\n'
        '5. Actually run foreground pwsh command ' + python + 'verify_advanced.py. If verification succeeds, '
        'present deliverable.md, todo_write all tasks completed, get_goal for the latest ID/revision, '
        'then update_goal action:complete using that exact ID/revision. Call get_goal again to verify complete. '
        'Never complete a failed or unfinished objective. Do not spawn background work other than this one worker. '
        'Finish with a short factual result and HARNESS-ADVANCED-DONE.')
    local = subprocess.run([sys.executable, 'verify_advanced.py'], cwd=workspace,
                           capture_output=True, timeout=15)
    completed = successful_tools(f)
    required = {'glob', 'read', 'edit', 'todo_write', 'present', 'ask_user_question',
                'create_goal', 'get_goal', 'update_goal', 'pwsh', 'job_list', 'job_output', 'job_kill'}
    tool_records = list(f.tools.values())
    jobs = [t for t in tool_records if t.get('name') in ('job_output', 'job_kill')]
    report['advanced'] = {'required_tools': sorted(required), 'missing_tools': sorted(required-completed),
        'independent_verification': {'exit_code': local.returncode, 'stdout_sha256': sha(local.stdout),
                                    'stderr_sha256': sha(local.stderr)},
        'deliberately_uncovered': ['read_image', 'PTC', 'ralph', 'workflow']}
    report['checks'].update({'advanced_turn_completed': reason == 'completed',
        'advanced_required_tools_completed': required <= completed,
        'advanced_marker_received': 'HARNESS-ADVANCED-DONE' in f.markers,
        'exact_fixture_question_answered': len(f.question_answers) == 1,
        'independent_fixture_verification': local.returncode == 0,
        'model_executed_verification': any(t.get('name') == 'pwsh' and not t.get('background')
            and 'HARNESS-ADVANCED-VERIFIED' in t.get('markers', []) for t in tool_records),
        'protected_files_unchanged': all((workspace/n).exists() and sha((workspace/n).read_bytes()) == digest
                                        for n, digest in protected.items()),
        'no_unexpected_files': {p.name for p in workspace.iterdir()} <= set(protected) | {'deliverable.md','worker.ready'},
        'todo_final_complete': bool(f.todo_states) and f.todo_states[-1]['count'] > 0
            and set(f.todo_states[-1]['statuses']) == {'completed'},
        'goal_created_with_one_round': len([t for t in tool_records if t.get('name') == 'create_goal']) == 1
            and all(t.get('goal_round_cap') == 1 for t in tool_records if t.get('name') == 'create_goal'),
        'goal_terminal_complete': bool(f.goal_states) and f.goal_states[-1]['phase'] == 'complete'
            and f.goal_states[-1]['maxGoalRounds'] == 1,
        'single_background_job_started': len(f.owned_jobs) == 1
            and len([t for t in tool_records if t.get('background')]) == 1,
        'only_own_jobs_targeted': bool(jobs) and all(t.get('targets_own_job') for t in jobs),
        'worker_actually_started': (workspace/'worker.ready').exists(),
        'job_output_marker_observed': any(t.get('name') == 'job_output'
            and 'HARNESS-OWN-JOB-READY' in t.get('markers', []) for t in tool_records),
        'job_cancellation_requested': any(t.get('name') == 'job_kill'
            and t.get('cancellation_requested') for t in tool_records),
        'job_killed_terminal_observed': any(t.get('name') == 'job_output'
            and t.get('job_status_killed') for t in tool_records)})


def workflow(runner, report):
    workspace = runner.workspace
    protected = [p for p in workspace.rglob('*') if p.is_file()]
    before = {p.relative_to(workspace).as_posix(): sha(p.read_bytes()) for p in protected}
    f = runner.create()
    def unchanged():
        return (set(before) == {p.relative_to(workspace).as_posix() for p in workspace.rglob('*') if p.is_file()}
                and all((workspace/n).exists() and sha((workspace/n).read_bytes()) == value
                        for n, value in before.items()))
    f.subscribe_reviews(unchanged, runner.deadline)
    runner.command(f, '/plan')
    verify = "& '" + Path(sys.executable).resolve().as_posix().replace("'", "''") + "' verify.py"
    reason = runner.prompt(f,
        'This is an isolated synthetic acceptance task. First stay in the selected plan mode. '
        'Load money-review with the skill tool, grep for total_cents, and read requirements.md, verify.py and price.py. '
        'Use the subagent tool for exactly one synchronous read-only audit: it must read ONLY requirements.md '
        'and price.py in this working directory, verify edge cases, and finish with SUBAGENT-AUDIT-DONE. '
        'Inherit provider/model and never spawn further agents. Before approval do not edit files or execute commands. '
        'Use exit_plan_mode to submit a concise complete markdown plan headed with #. '
        'The plan must explicitly change only price.py, preserve all other files, use integer arithmetic, '
        'and run verify.py. Include MONEY-SKILL-LOADED after actually loading the skill. '
        'After the normal plan-review approves, implement the plan in the same task: use write to replace ONLY price.py '
        'with the correct implementation, then actually run this exact command with pwsh: ' + verify + '. '
        'Do not install dependencies, contact networks, read external files, or change protected files. '
        'Finish briefly with the actual verification result and HARNESS-WORKFLOW-DONE.')
    tools = successful_tools(f)
    report['checks'].update({'plan_review_approved': len(f.plan_reviews) == 1 and f.plan_reviews[0]['approved'],
        'plan_mode_event_observed': any(e['type'] == 'plan/mode' and e.get('active') is True for e in f.events),
        'plan_fixture_unchanged': bool(f.plan_reviews) and all(p['fixture_unchanged'] for p in f.plan_reviews),
        'exit_plan_mode_tool_completed': 'exit_plan_mode' in tools,
        'skill_tool_completed': 'skill' in tools, 'file_search_completed': 'grep' in tools,
        'file_read_completed': 'read' in tools, 'skill_marker_received': 'MONEY-SKILL-LOADED' in f.markers,
        'subagent_tool_completed': bool({'subagent', 'subagent_fork'} & tools),
        'subagent_result_marker': 'SUBAGENT-AUDIT-DONE' in f.markers})
    children = runner.inspect_children(f)
    report['checks']['subagent_real_model_evidence'] = bool(children) and all(
        c.counts['assistant/message'] > 0 and c.counts['request/header'] > 0 and 'read' in successful_tools(c)
        and c.turn_ends and c.turn_ends[-1]['reason'] == 'completed' for c in children)
    local = subprocess.run([sys.executable, 'verify.py'], cwd=workspace, capture_output=True, timeout=15)
    tools = successful_tools(f)
    report['verification'] = {'exit_code': local.returncode, 'stdout_sha256': sha(local.stdout),
                              'stderr_sha256': sha(local.stderr), 'price_sha256': sha((workspace/'price.py').read_bytes())}
    report['checks'].update({'build_turn_completed': reason == 'completed',
        'build_model_marker': 'HARNESS-WORKFLOW-DONE' in f.markers,
        'file_write_completed': 'write' in tools, 'shell_tool_completed': 'pwsh' in tools,
        'client_executed_fixture_tests': 'VERIFIED: 11' in f.markers,
        'independent_fixture_tests_pass': local.returncode == 0,
        'protected_files_unchanged': all((workspace/n).exists() and sha((workspace/n).read_bytes()) == value
                                       for n, value in before.items() if n != 'price.py'),
        'plan_mode_exited': any(e['type'] == 'plan/mode' and e.get('active') is False for e in f.events)})


class ProxyMonitor:
    def __init__(self):
        self.token = subprocess.check_output([str(ROOT/'dist/uart2llm.exe'), 'token', 'admin'], text=True).strip()
        self.samples, self.errors = [], []
        self.stop = threading.Event()
        self.before = self.get('metrics')
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def get(self, path):
        c = http.client.HTTPConnection('127.0.0.1', 8766, timeout=5)
        try:
            c.request('GET', '/admin/v1/'+path, headers={'Authorization': 'Bearer '+self.token})
            r = c.getresponse()
            if r.status != 200:
                raise RuntimeError('local_management_failed')
            return json.load(r)
        finally:
            c.close()

    def run(self):
        while not self.stop.is_set() and len(self.samples) < 7200:
            start = time.monotonic()
            try:
                d = self.get('state'); dev = d.get('device', {}); link = dev.get('link', {})
                self.samples.append({'active': d['host']['active_requests'],
                    'management_ms': round((time.monotonic()-start)*1000, 3),
                    'connected': d.get('connected'), 'paired': d.get('paired'),
                    'session': dev.get('session'), 'rx_bytes': link.get('rx_bytes'), 'tx_bytes': link.get('tx_bytes'),
                    'crc_errors': link.get('crc_errors'), 'retries': link.get('retries'),
                    'internal_free': dev.get('memory', {}).get('internal_free')})
            except Exception as e: self.errors.append(safe_error(e))
            self.stop.wait(0.25)

    def finish(self):
        end = time.monotonic()+15
        while time.monotonic() < end:
            if self.get('metrics').get('active') == 0: break
            time.sleep(0.1)
        self.stop.set(); self.thread.join(6)
        after = self.get('metrics')
        counters = ('requests','completed','succeeded','failed','cancelled','rejected','chat_requests',
                    'prompt_tokens','completion_tokens','total_tokens','request_bytes','response_bytes')
        return {'before': self.before, 'after': after,
            'delta': {k: after[k]-self.before[k] for k in counters},
            'samples': self.samples, 'errors': self.errors,
            'peak_active': max((s['active'] for s in self.samples), default=0),
            'management_max_ms': max((s['management_ms'] for s in self.samples), default=0)}


def parallel(runner, report):
    follows = [runner.create() for _ in range(4)]
    with ThreadPoolExecutor(max_workers=4) as pool:
        results = list(pool.map(lambda f: runner.prompt(f,
            'Do not use tools. Print numbers 1 through 120 separated by spaces and finish with HARNESS-PARALLEL-DONE.'), follows))
    report['checks'].update({'four_completed': all(x == 'completed' for x in results),
        'four_markers_received': all('HARNESS-PARALLEL-DONE' in f.markers for f in follows),
        'four_streams_observed': all(f.counts['live_assistant_chunks'] > 0 for f in follows),
        'automatic_title_requests_preserved': all(f.counts['session/title-llm-request'] > 0 for f in follows),
        'all_stream_attempts_settled': all(all(a['ended'] for a in f.attempts.values()) for f in follows)})


def cancel(runner, report):
    f = runner.create(); before = len(f.turn_ends)
    runner.enqueue(f, 'Do not use tools. Print all integers from 1 to 10000, one on each line, without abbreviations.')
    while f.counts['live_assistant_chunks'] < 20 and not f.turn_ends:
        f.receive(runner.deadline)
    started = time.monotonic()
    runner.h.rpc('session/cancel', request={'sessionId': f.session_id})
    reason = f.wait_turn(before, runner.deadline)
    report['cancel_ms'] = round((time.monotonic()-started)*1000, 3)
    resumed = runner.prompt(f, 'Cancellation test is finished. Do not continue counting or use tools. Reply exactly UART2LLM_HARNESS_SMOKE_OK.')
    report['checks'].update({'cancelled_mid_stream': f.counts['live_assistant_chunks'] >= 20,
        'cancel_end_observed': reason == 'aborted', 'followup_completed': resumed == 'completed',
        'followup_marker': 'UART2LLM_HARNESS_SMOKE_OK' in f.markers})


def image_fixture(workspace):
    (workspace/'AGENTS.md').write_text('Synthetic visual acceptance. Use only read_image on probe.png. '
        'Do not read any other file, run commands, use networks, delegate or modify anything.\n', encoding='utf-8')


def reasoning_fixture(workspace):
    (workspace/'AGENTS.md').write_text('Synthetic reasoning and tool roundtrip test. Only read solve.py '
        'and run it with the specified Python. Do not modify files, read external directories, use networks or delegate.\n', encoding='utf-8')
    (workspace/'solve.py').write_text('values = [n for n in range(1000,10000) if n%17==11 and n%23==7 and n%29==13]\n'
        'assert len(values)==1\nprint("HARNESS-REASONING-VERIFIED:", values[0])\n', encoding='utf-8')


def reasoning_check(runner, report):
    digest = sha((runner.workspace/'solve.py').read_bytes())
    f = runner.create()
    command = "& '" + Path(sys.executable).resolve().as_posix().replace("'", "''") + "' solve.py"
    reason = runner.prompt(f, 'Validate this modular arithmetic computation with the provided program. '
        'Use pwsh to run exactly '+command+'. Do not calculate the answer from memory or modify the program. '
        'After the tool completes, return the exact verification line it printed, without extra prose.')
    report['checks'].update({'thinking_mode_selected':REASONING=='high', 'thinking_tool_turn_completed':reason=='completed',
        'real_shell_result':any(t.get('name')=='pwsh' and not t.get('is_error')
            and 'HARNESS-REASONING-VERIFIED: 7321' in t.get('markers',[]) for t in f.tools.values()),
        'verified_answer_returned':'HARNESS-REASONING-VERIFIED: 7321' in f.markers,
        'reasoning_fixture_unchanged':sha((runner.workspace/'solve.py').read_bytes())==digest})
    report['reasoning_blocks_observed'] = f.counts['reasoning_blocks']


def image_check(runner, report):
    from PIL import Image, ImageDraw, ImageFont
    colors = {'red':'#ed2024', 'green':'#00a030', 'blue':'#1769e0', 'yellow':'#ffdd00',
              'purple':'#9029bd', 'orange':'#ff8800'}
    foreground = secrets.choice(list(colors)); background = secrets.choice([c for c in colors if c != foreground])
    shape = secrets.choice(['circle','square','triangle']); digits = str(1000+secrets.randbelow(9000))
    image = Image.new('RGB', (160, 140), colors[background]); draw = ImageDraw.Draw(image)
    if shape == 'circle': draw.ellipse((48, 17, 112, 81), fill=colors[foreground])
    elif shape == 'square': draw.rectangle((48, 17, 112, 81), fill=colors[foreground])
    else: draw.polygon([(80, 12), (43, 81), (117, 81)], fill=colors[foreground])
    draw.rectangle((0,100,160,140), fill='white')
    font = ImageFont.truetype(str(Path(os.environ['WINDIR'])/'Fonts/arial.ttf'), 32)
    draw.text((42,101), digits, fill='black', font=font)
    path = runner.workspace/'probe.png'; image.save(path); digest = sha(path.read_bytes())
    f = runner.create()
    reason = runner.prompt(f, 'Use read_image to inspect probe.png. Return a JSON object with keys shape, '
        'foreground, background and digits. Shape means the single central geometric shape; foreground '
        'and background are simple lowercase English color words, ignoring the white bottom text strip. '
        'digits is the exact four-digit string printed in that strip. If you cannot see the actual image, '
        'use unknown; do not guess. Do not use any other tool or change files.')
    page = runner.h.rpc('session/page', request={'address': f.address, 'throughSeq': f.cursor, 'maxMessages':100})
    answers, image_results = [], []
    def contains_image(value):
        if isinstance(value, dict): return value.get('type') == 'image' or any(contains_image(v) for v in value.values())
        return isinstance(value, list) and any(contains_image(v) for v in value)
    for record in page['records']:
        e = record.get('event', {}); data = e.get('data', {})
        if e.get('type') == 'assistant/message':
            text = '\n'.join(text_blocks(data.get('message', {}).get('content'))).strip()
            if text.startswith('```'): text = re.sub(r'^```(?:json)?\s*|\s*```$', '', text)
            try:
                value = json.loads(text)
                if isinstance(value, dict): answers.append(value)
            except ValueError: pass
        if e.get('type') == 'tool/result': image_results.append(contains_image(data))
    expected = {'shape': shape, 'foreground': foreground, 'background': background, 'digits': digits}
    observed = answers[-1] if answers else {}
    report['visual'] = {'input_sha256':digest, 'expected':expected,
        'observed':{k:v for k,v in observed.items() if k in expected and isinstance(v,str) and len(v)<20}}
    report['checks'].update({'image_turn_completed':reason=='completed', 'read_image_completed':'read_image' in successful_tools(f),
        'actual_image_result':any(image_results), 'blind_visual_and_ocr_match':observed==expected,
        'image_file_unchanged':sha(path.read_bytes())==digest,
        'no_alternative_tool':all(t.get('name')=='read_image' for t in f.tools.values())})


def main():
    global MODEL, REASONING
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--phase', choices=['smoke', 'workflow', 'parallel', 'cancel', 'search', 'advanced', 'coordination','image','reasoning'], default='smoke')
    parser.add_argument('--report', type=Path, required=True)
    parser.add_argument('--seconds', type=int, default=600)
    parser.add_argument('--model', choices=['deepseek-flash','deepseek-v4-pro'], default=MODEL)
    parser.add_argument('--reasoning', choices=['off','high','max'], default=REASONING)
    args = parser.parse_args()
    MODEL, REASONING = args.model, args.reasoning
    if args.report.exists():
        parser.error('report already exists; preserve previous evidence')
    if not 30 <= args.seconds <= 1800:
        parser.error('--seconds must be 30..1800')
    workspace = ROOT / '.local/harness-acceptance' / (datetime.now().strftime('%Y%m%d-%H%M%S')+'-'+uuid.uuid4().hex[:8])
    workspace.mkdir(parents=True)
    {'advanced': advanced_fixture, 'coordination': coordination_fixture, 'image':image_fixture,
     'reasoning':reasoning_fixture}.get(args.phase, fixture)(workspace)
    report = {'started_utc': datetime.now(timezone.utc).isoformat(), 'phase': args.phase,
              'client': 'installed DeepSeek Harness Web', 'model': MODEL, 'provider': PROVIDER, 'reasoning': REASONING,
              'transport': 'authenticated HTTP Web RPC + /api/remote.mux WebSocket',
              'workspace': str(workspace), 'scope': 'This phase only; not full-feature acceptance',
              'full_feature_acceptance': False,
              'limits': {'seconds': args.seconds, 'max_model_steps_per_session': 48,
                  'max_observed_bytes_per_session': 32*1024*1024}, 'checks': {}, 'passed': False}
    runner = None
    monitor = None
    try:
        h = Harness()
        monitor = ProxyMonitor()
        runner = Runner(h, workspace, time.monotonic()+args.seconds)
        if args.phase == 'smoke':
            f = runner.create()
            reason = runner.prompt(f, 'Reply with exactly UART2LLM_HARNESS_SMOKE_OK. Do not use tools, files, agents or reasoning text.')
            report['checks'] = {'turn_completed': reason == 'completed',
                                'exact_test_marker_received': 'UART2LLM_HARNESS_SMOKE_OK' in f.markers,
                                'live_stream_observed': f.counts['live_assistant_chunks'] > 0,
                                'no_tools_requested': not f.tools}
            if REASONING != 'off':
                report['checks']['reasoning_content_observed'] = f.counts['reasoning_blocks'] > 0
        elif args.phase == 'workflow':
            workflow(runner, report)
        elif args.phase == 'parallel':
            parallel(runner, report)
        elif args.phase == 'cancel':
            cancel(runner, report)
        elif args.phase == 'advanced':
            advanced(runner, report)
        elif args.phase == 'coordination':
            coordination(runner, report)
        elif args.phase == 'image':
            image_check(runner, report)
        elif args.phase == 'reasoning':
            reasoning_check(runner, report)
        else:
            raise NotImplementedError('phase_not_implemented_yet')
    except Exception as error:
        report['error'] = safe_error(error)
    finally:
        if monitor:
            try:
                report['proxy'] = monitor.finish()
                report['checks']['proxy_capacity_held'] = report['proxy']['peak_active'] <= 4
                report['checks']['management_responsive'] = not report['proxy']['errors']
                report['checks']['proxy_idle_after_run'] = report['proxy']['after']['active'] == 0
                if args.phase in ('cancel','coordination'):
                    delta = report['proxy']['delta']
                    report['checks']['only_expected_proxy_cancellation'] = (delta['failed'] == delta['cancelled']
                        and delta['cancelled'] <= 1 and delta['rejected'] == 0)
                else:
                    report['checks']['no_proxy_failures'] = report['proxy']['delta']['failed'] == 0
            except Exception as error: report['monitor_error'] = safe_error(error)
        if runner:
            report['cleanup_errors'] = runner.cleanup()
            report['sessions'] = [f.summary() for f in runner.follows]
            available = set().union(*(f.available_tools for f in runner.follows))
            completed = set().union(*(successful_tools(f) for f in runner.follows))
            report['coverage'] = {'available_tools': sorted(available), 'completed_tools': sorted(completed),
                                  'not_exercised_tools': sorted(available-completed),
                                  'other_required_phases': [p for p in ['smoke','workflow','parallel','cancel','search','advanced','coordination','image','reasoning'] if p != args.phase]}
            report['checks']['no_sequence_errors'] = all(not f.gaps for f in runner.follows)
            report['checks']['all_model_routes_local'] = bool(runner.follows) and all(
                e['route'].get('provider') == PROVIDER and e['route'].get('model') == MODEL
                for f in runner.follows for e in f.events if 'route' in e)
            report['checks']['cleanup_completed'] = not report['cleanup_errors']
        report['finished_utc'] = datetime.now(timezone.utc).isoformat()
        report['passed'] = bool(report['checks']) and all(report['checks'].values()) and 'error' not in report and 'monitor_error' not in report
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')
        print(json.dumps({'phase': args.phase, 'passed': report['passed'], 'checks': report['checks'],
                          'error': report.get('error'), 'report': str(args.report)}, ensure_ascii=False))
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
