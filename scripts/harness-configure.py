#!/usr/bin/env python3
"""Add an independent local Harness provider through its authenticated API."""
import json
from pathlib import Path
import subprocess
import uuid
from harness_support import Harness

ROOT = Path(__file__).resolve().parent.parent


def main():
    h = Harness()
    token = subprocess.check_output([str(ROOT / 'dist/uart2llm.exe'), 'token', 'api'],
                                    text=True).strip()
    sections = {n['ns']: n for n in h.rpc('settings/describe')['namespaces']}
    models = h.rpc('llm/discoverModels', settingsNs='llm-pi-ai', request={
        'provider': 'uart2llm', 'baseURL': 'http://127.0.0.1:8765/v1',
        'api': 'openai-completions', 'apiKey': token})
    print('discovery:', json.dumps(models, ensure_ascii=False))
    profile = {
        'displayName': 'uart2llm 本地代理', 'api': 'openai-completions',
        'baseURL': 'http://127.0.0.1:8765/v1', 'apiKeyEnv': 'UART2LLM_API_KEY',
        'models': [{'id': m, 'contextWindow': 1000000, 'maxTokens': 8192,
                    'input': ['text','image'] if m == 'deepseek-flash' else ['text'],
                    'reasoningEfforts': {'off': 'off', 'high': 'high', 'max': 'max'}}
                   for m in ['deepseek-flash', 'deepseek-v4-pro']],
        'reasoning': 'off', 'defaultMaxTokens': 8192,
        'compat': {'thinkingFormat': 'deepseek',
                   'requiresReasoningContentOnAssistantMessages': True,
                   'supportsDeveloperRole': False, 'maxTokensField': 'max_tokens',
                   'supportsStore': False, 'supportsUsageInStreaming': True,
                   'supportsReasoningEffort': True},
        'retryPolicy': {'mode': 'normal', 'maxRetries': 0},
    }
    existing = sections['llm-pi-ai']['value'].get('providers', {}).get('uart2llm')
    if existing is not None:
        if existing.get('baseURL') != profile['baseURL']:
            raise RuntimeError('Existing uart2llm provider points elsewhere; preserved')
        ref = existing.get('apiKeyEnv')
        if not ref or not h.rpc('credentials/describe', refs=[ref])[ref]['configured']:
            raise RuntimeError('Existing provider credential is missing; repair it in Harness settings')
        print('Existing local provider and credential preserved')
        return
    # A revision conflict must never overwrite a credential already used by
    # another provider. A failed commit may leave this private, unreferenced
    # entry; it cannot change an existing provider's credential.
    ref = 'UART2LLM_API_KEY_' + uuid.uuid4().hex.upper()
    profile['apiKeyEnv'] = ref
    h.rpc('credentials/set', ref=ref, value=token)
    h.rpc('settings/mutate', ns='llm-pi-ai', expectedRevision=sections['llm-pi-ai']['revision'],
          ops=[{'op': 'set', 'path': ['providers', 'uart2llm'], 'value': profile}])
    status = h.rpc('credentials/describe', refs=[ref])
    print('credential:', json.dumps(status))
    updated = next(n for n in h.rpc('settings/describe')['namespaces'] if n['ns'] == 'llm-pi-ai')
    print('configured:', json.dumps(updated['value']['providers']['uart2llm'], ensure_ascii=False))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # RPC failures can contain server-supplied request details.
        print('Harness configuration failed (' + type(error).__name__ + '); existing providers were preserved')
        raise SystemExit(1)
