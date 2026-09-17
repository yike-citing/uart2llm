"""Use the installed Harness's normal authenticated Web RPC (no auth bypass)."""
import http.cookiejar
import json
import os
from pathlib import Path
import re
import urllib.request
import uuid


class Harness:
    def __init__(self, base='http://127.0.0.1:3080'):
        self.base = base.rstrip('/')
        self.private = Path(os.environ['LOCALAPPDATA']) / 'uart2llm/harness-validation'
        self.cookies = http.cookiejar.MozillaCookieJar(str(self.private / 'cookies.txt'))
        self.cookies.load(ignore_discard=True, ignore_expires=True)
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(self.cookies))

    def rpc(self, method, **args):
        envelope = {'type': 'client-request', 'rpcId': str(uuid.uuid4()),
                    'method': method, 'payload': {'args': args}}
        request = urllib.request.Request(self.base + '/api/' + method,
            data=json.dumps(envelope).encode(), headers={
                'Content-Type': 'application/json', 'Origin': self.base})
        with self.opener.open(request, timeout=120) as response:
            reply = json.load(response)
        result = reply.get('result', {})
        if not result.get('ok'):
            # The caller must redact any server-supplied details before publishing.
            raise RuntimeError(json.dumps(result.get('error', reply), ensure_ascii=False))
        return result.get('value')
