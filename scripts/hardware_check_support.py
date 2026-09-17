"""Redaction for public hardware evidence; never include raw request bodies."""
from pathlib import Path


def default_executable():
    root=Path(__file__).resolve().parent.parent
    source_build=root/'dist'/'uart2llm.exe'
    return source_build if source_build.exists() else root/'uart2llm.exe'


def sanitize(value):
    if isinstance(value,dict):
        # Usage counters are measurements, not credentials. Keep only explicitly
        # named numeric counters; a string in the same field is still redacted.
        counters = {'prompt_tokens','completion_tokens','total_tokens','cached_tokens',
                    'reasoning_tokens','prompt_cache_hit_tokens','prompt_cache_miss_tokens'}
        return {k:(v if k in counters and type(v) is int and v >= 0 else
                   '[redacted]' if any(s in k.lower() for s in ('password','token','ssid','bssid','username','upstream_key'))
                   else sanitize(v)) for k,v in value.items()}
    if isinstance(value,list): return [sanitize(v) for v in value]
    return value
