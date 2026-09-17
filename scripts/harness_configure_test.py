"""Regression checks for preserving existing local Harness configuration."""
import importlib.util
import io
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('harness_configure', Path(__file__).with_name('harness-configure.py'))
configure = importlib.util.module_from_spec(spec)
spec.loader.exec_module(configure)


class Client:
    def __init__(self, existing=None, conflict=False):
        self.calls, self.existing, self.conflict = [], existing, conflict

    def rpc(self, method, **args):
        self.calls.append((method, args))
        if method == 'settings/describe':
            return {'namespaces': [{'ns': 'llm-pi-ai', 'revision': 7,
                     'value': {'providers': {} if self.existing is None else {'uart2llm': self.existing}}}]}
        if method == 'llm/discoverModels': return [{'id': 'deepseek-flash'}]
        if method == 'credentials/describe': return {r: {'configured': True} for r in args['refs']}
        if method == 'settings/mutate' and self.conflict: raise RuntimeError('test revision conflict')


class ConfigureTest(unittest.TestCase):
    def run_configure(self, client):
        with patch.object(configure, 'Harness', return_value=client), \
             patch.object(configure.subprocess, 'check_output', return_value='synthetic-local-token'), \
             patch('sys.stdout', new=io.StringIO()):
            configure.main()

    def test_existing_customization_and_key_are_not_overwritten(self):
        client = Client({'baseURL': 'http://127.0.0.1:8765/v1', 'apiKeyEnv': 'EXISTING_REF',
                         'models': [{'id': 'custom-model'}], 'defaultMaxTokens': 1234})
        self.run_configure(client)
        self.assertFalse(any(m in ['settings/mutate', 'credentials/set'] for m, _ in client.calls))

    def test_revision_conflict_does_not_change_shared_credential(self):
        client = Client(conflict=True)
        with self.assertRaises(RuntimeError): self.run_configure(client)
        writes = [a for m, a in client.calls if m == 'credentials/set']
        self.assertEqual(len(writes), 1)
        self.assertRegex(writes[0]['ref'], r'^UART2LLM_API_KEY_[A-F0-9]{32}$')
        mutations = [a for m, a in client.calls if m == 'settings/mutate']
        self.assertEqual(mutations[0]['expectedRevision'], 7)
        self.assertEqual(mutations[0]['ops'][0]['value']['apiKeyEnv'], writes[0]['ref'])


if __name__ == '__main__': unittest.main()
