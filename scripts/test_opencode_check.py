"""Offline regression checks; no model provider or physical device is used."""
import importlib.util
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import threading
import unittest

spec = importlib.util.spec_from_file_location('opencode_check', Path(__file__).with_name('opencode-check.py'))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class Diagnostics(unittest.TestCase):
    def test_observer_preserves_rejection_and_records_only_machine_code(self):
        class Reject(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_POST(self):
                self.rfile.read(int(self.headers['Content-Length']))
                body = b'{"error":{"code":"capacity_exceeded","type":"proxy_error","message":"private detail"}}\n'
                self.send_response(503)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Retry-After', '1')
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        upstream = ThreadingHTTPServer(('127.0.0.1', 0), Reject)
        observer = runner.Observer('local-test', forward_port=upstream.server_port)
        server = ThreadingHTTPServer(('127.0.0.1', 0), observer.handler())
        for instance in (upstream, server):
            threading.Thread(target=instance.serve_forever, daemon=True).start()
        connection = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=5)
        try:
            connection.request('POST', '/v1/chat/completions',
                               body=json.dumps({'model': runner.MODEL, 'max_tokens': 1536, 'stream': True}),
                               headers={'Authorization': 'Bearer ' + observer.client_token})
            response = connection.getresponse()
            self.assertEqual(response.status, 503)
            self.assertIn(b'capacity_exceeded', response.read())
        finally:
            connection.close()
            server.shutdown()
            server.server_close()
            upstream.shutdown()
            upstream.server_close()
        self.assertEqual(observer.requests[0]['response_error_code'], 'capacity_exceeded')
        self.assertEqual(observer.requests[0]['retry_after_seconds'], 1)
        self.assertFalse(observer.requests[0]['done'])
        self.assertNotIn('private detail', json.dumps(observer.requests))

    def test_capacity_is_not_token_truncation(self):
        rows = [
            {'id': 1, 'status': 200, 'stream': True, 'done': True, 'finish_reason': 'stop'},
            {'id': 2, 'status': 503, 'stream': True, 'done': False,
             'response_error_code': 'capacity_exceeded'},
            {'id': 3, 'status': 200, 'stream': True, 'done': True, 'finish_reason': 'length'},
            {'id': 4, 'status': 200, 'stream': True, 'done': False, 'error': 'IncompleteRead'},
        ]
        result = runner.request_diagnostics(rows)
        self.assertEqual(result['capacity_rejected_ids'], [2])
        self.assertEqual(result['output_limit_ids'], [3])
        self.assertEqual(result['incomplete_success_stream_ids'], [4])
        self.assertEqual(result['forwarding_error_ids'], [4])
        self.assertEqual(result['missing_success_terminal_ids'], [4])

    def test_error_metadata_never_exports_message_or_body(self):
        data = json.dumps({'error': {'code': 'capacity_exceeded', 'type': 'proxy_error',
                                   'message': 'private prompt and credentials'}}).encode() + b'\n'
        self.assertEqual(runner.error_metadata(data), {
            'response_error_code': 'capacity_exceeded', 'response_error_type': 'proxy_error'})
        self.assertEqual(runner.error_metadata(b'{"error":{"code":"has spaces or private data"}}'), {})
        self.assertEqual(runner.error_metadata(b'not json'), {})

    def test_original_failure_has_no_generation_truncation(self):
        path = Path(__file__).resolve().parents[1] / 'docs/test-reports/2026-09-16-usb-host-agent.json'
        if not path.exists():
            self.skipTest('historical hardware evidence not included')
        report = json.loads(path.read_text(encoding='utf-8'))
        result = runner.request_diagnostics(report['requests'])
        self.assertEqual(result['http_status_counts'], {'200': 15, '503': 6})
        self.assertEqual(result['output_limit_ids'], [])
        self.assertEqual(result['incomplete_success_stream_ids'], [])
        self.assertEqual(result['forwarding_error_ids'], [])
        self.assertFalse(report['passed'])


if __name__ == '__main__':
    unittest.main()
