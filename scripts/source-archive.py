"""Create a source-only ZIP; refuse overwrite and fail on public-copy checks."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import zipfile
from source_manifest import ROOT, source_files

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    output=args.output.resolve()
    if output.exists():parser.error('Output exists; choose a new archive name')
    if output.is_relative_to(ROOT):parser.error('Place the archive outside the source tree')
    spec=importlib.util.spec_from_file_location('check_source',ROOT/'scripts/check-source.py')
    check=importlib.util.module_from_spec(spec);spec.loader.exec_module(check)
    result=check.check()
    if not result['passed']:
        print(json.dumps(result,ensure_ascii=False));return 1
    output.parent.mkdir(parents=True,exist_ok=True)
    manifest=[]
    # Use a clean build placeholder even if the developer has built the Web UI.
    placeholder=b'<!doctype html><meta charset="utf-8"><title>uart2llm</title><p>Build Web assets with scripts/build-ui.ps1 before building the daemon.</p>\n'
    with zipfile.ZipFile(output,'x',compression=zipfile.ZIP_DEFLATED,compresslevel=9) as archive:
        for path in sorted(source_files()):
            rel=path.relative_to(ROOT).as_posix()
            data=placeholder if rel=='internal/ui/assets/index.html' else path.read_bytes()
            manifest.append(hashlib.sha256(data).hexdigest()+'  '+rel)
            archive.writestr('uart2llm/'+rel,data)
        archive.writestr('uart2llm/SOURCE_CHECKSUMS.sha256','\n'.join(manifest)+'\n')
    print(json.dumps({'files':len(manifest),'bytes':output.stat().st_size,
                      'sha256':hashlib.sha256(output.read_bytes()).hexdigest(),'archive':str(output)}))
    return 0

if __name__=='__main__':raise SystemExit(main())
