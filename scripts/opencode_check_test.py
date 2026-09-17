"""Local-only checks that the observer does not invent successful evidence."""
import importlib.util
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import threading
import unittest

from hardware_check_support import sanitize

spec = importlib.util.spec_from_file_location('agent_check', Path(__file__).with_name('opencode-check.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class EvidenceTests(unittest.TestCase):
    def test_usage_and_credentials(self):
        out = sanitize({'api_token': 'secret', 'password': 'secret',
                        'usage': {'prompt_tokens': 17, 'completion_tokens': 3, 'total_tokens': 20,
                                  'cached_tokens': 'not-a-counter', 'unknown_token': 12}})
        self.assertEqual(out['usage']['total_tokens'], 20)
        self.assertEqual(out['usage']['cached_tokens'], '[redacted]')
        self.assertNotIn('secret', json.dumps(out))
        self.assertEqual(out['usage']['unknown_token'], '[redacted]')

    def test_fragmented_unicode_tool_call_and_request_identity(self):
        captured = []
        parts = [
            {'choices': [{'delta': {'tool_calls': [{'index': 0, 'id': 'call_a', 'function': {'name': 'grep', 'arguments': '{"pattern":"'}}]}}]},
            {'choices': [{'delta': {'tool_calls': [{'index': 0, 'function': {'arguments': '金额"}'}}]}}]},
            {'choices': [{'delta': {}, 'finish_reason': 'tool_calls'}], 'usage': {'total_tokens': 20}}]
        payload = b''.join(b'data: ' + json.dumps(p, ensure_ascii=False).encode() + b'\n\n' for p in parts) + b'data: [DONE]\n\n'

        class Upstream(BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def do_POST(self):
                captured.append(self.rfile.read(int(self.headers['Content-Length'])))
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.send_header('Content-Length', str(len(payload)))
                self.end_headers()
                for b in payload:
                    self.wfile.write(bytes([b]))
                    self.wfile.flush()

        upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
        observer = module.Observer('test-local-secret', limit=1, forward_port=upstream.server_port)
        server = ThreadingHTTPServer(('127.0.0.1', 0), observer.handler())
        for s in (upstream, server):
            threading.Thread(target=s.serve_forever, daemon=True).start()
        request = json.dumps({'model': module.MODEL, 'max_tokens': 100, 'stream': True, 'messages': []}).encode()
        def post():
            c = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=3)
            try:
                c.request('POST', '/v1/chat/completions', body=request,
                          headers={'Authorization': 'Bearer ' + observer.client_token})
                r = c.getresponse()
                return r.status, r.read()
            finally:
                c.close()
        try:
            status, body = post()
            self.assertEqual((status, body), (200, payload))
            self.assertEqual(captured, [request])
            # Reading the terminating chunk can race with metadata finalization.
            server.shutdown()
            server.server_close()
            item = observer.requests[0]
            self.assertTrue(item['done'])
            self.assertTrue(item['tool_calls']['0']['valid_json'])
            self.assertEqual(item['tool_calls']['0']['fragments'], 2)
            self.assertEqual(item['usage']['total_tokens'], 20)
            self.assertNotIn('arguments', item['tool_calls']['0'])
        finally:
            upstream.shutdown()
            upstream.server_close()
            server.server_close()

    def test_request_budget_rejects_without_forwarding(self):
        observer = module.Observer('test-secret', limit=0)
        server = ThreadingHTTPServer(('127.0.0.1', 0), observer.handler())
        threading.Thread(target=server.serve_forever, daemon=True).start()
        conn = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=3)
        try:
            conn.request('POST', '/v1/chat/completions', body=json.dumps({'model': module.MODEL, 'max_tokens': 100}),
                         headers={'Authorization': 'Bearer ' + observer.client_token})
            response = conn.getresponse()
            self.assertEqual(response.status, 429)
            response.read()
            self.assertEqual(observer.requests, [])
        finally:
            conn.close()
            server.shutdown()
            server.server_close()


if __name__ == '__main__':
    unittest.main()
