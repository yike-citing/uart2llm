"""Check public source links and common secret patterns without printing values."""
import json
import re
from urllib.parse import unquote, urlsplit
from source_manifest import ROOT, source_files


def check():
    errors = []
    files = list(source_files())
    patterns = [r'\bsk-[A-Za-z0-9_-]{24,}', r'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----',
                r'(?i)[?&](?:token|auth)=[A-Za-z0-9_.-]{24,}', r'(?i)[A-Z]:[/\\]Users[/\\](?!Public\b|YOUR_USER\b)[^/\\\s]+']
    for path in files:
        if path.name.endswith('.tar.gz'): continue
        text = path.read_text(encoding='utf-8', errors='replace')
        rel = path.relative_to(ROOT).as_posix()
        if any(re.search(p, text) for p in patterns): errors.append({'file':rel,'kind':'sensitive-pattern'})
        links = []
        if path.suffix == '.md':
            links += re.findall(r'\]\(([^)\s]+)(?:\s+"[^"]*")?\)',text)
        if path.suffix == '.html':
            links += re.findall(r'(?:href|src)=["\']([^"\']+)',text)
        for target in links:
            target = target.strip('<>'); parts=urlsplit(target)
            if parts.scheme or parts.netloc or not parts.path: continue
            # Vite's HTML entry uses a root-relative /src/main.tsx URL.
            if path == ROOT/'web/index.html' and parts.path.startswith('/'):
                resolved=(ROOT/'web'/unquote(parts.path).lstrip('/')).resolve()
            elif path == ROOT/'internal/ui/assets/index.html' and parts.path.startswith('/'):
                resolved=(ROOT/'internal/ui/assets'/unquote(parts.path).lstrip('/')).resolve()
            else:
                resolved=(path.parent/unquote(parts.path)).resolve()
            if not resolved.is_relative_to(ROOT) or not resolved.exists():
                errors.append({'file':rel,'kind':'broken-local-link','target':target})
    return {'passed':not errors,'files_checked':len(files),'errors':errors,
            'scope':'Allowlisted source only; heuristic scan is not a security audit.'}


if __name__ == '__main__':
    result=check();print(json.dumps(result,ensure_ascii=False,indent=2))
    raise SystemExit(0 if result['passed'] else 1)
