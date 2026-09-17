#!/usr/bin/env python3
"""Exercise the user's configured Files API through localhost using synthetic PNG data."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import random
import re
import struct
import subprocess
import time
import urllib.error
import urllib.request
import uuid
import zlib
from hardware_check_support import default_executable


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--report',type=Path,required=True)
    p.add_argument('--exe',type=Path,default=default_executable())
    args=p.parse_args()
    if args.report.exists():p.error('report exists')
    report={'started_utc':datetime.now(timezone.utc).isoformat(),'cases':[],
            'http_destination':'http://localhost:8765','synthetic_data_only':True,'passed':False}
    token=subprocess.run([str(args.exe),'token','api'],capture_output=True,text=True,check=True).stdout.strip()
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
    file_id=None
    def case(name,action):
        start=time.monotonic();item={'name':name}
        try:
            value=action();item.update(status='passed',evidence=value);print(name+': PASS',flush=True);return value
        except Exception as e:
            item.update(status='failed',error=str(e));print(name+': FAIL - '+str(e),flush=True);raise
        finally:item['seconds']=round(time.monotonic()-start,3);report['cases'].append(item)
    def request(path,method='GET',data=None,extra=None):
        headers={'Authorization':'Bearer '+token};headers.update(extra or {})
        req=urllib.request.Request('http://localhost:8765/v1/files'+path,method=method,data=data,headers=headers)
        try:response=opener.open(req,timeout=150)
        except urllib.error.HTTPError as e:response=e
        with response:return response.status,response.read(1024*1024)
    try:
        def png_chunk(kind,data):return struct.pack('!I',len(data))+kind+data+struct.pack('!I',zlib.crc32(kind+data)&0xffffffff)
        rng=random.Random(20260916)
        raw=b''.join(b'\0'+rng.randbytes(256*3) for _ in range(256))
        png=b'\x89PNG\r\n\x1a\n'+png_chunk(b'IHDR',struct.pack('!2I5B',256,256,8,2,0,0,0))+png_chunk(b'IDAT',zlib.compress(raw))+png_chunk(b'IEND',b'')
        report['synthetic_file']={'bytes':len(png),'sha256':hashlib.sha256(png).hexdigest()}
        boundary='u2-'+uuid.uuid4().hex
        fields={'purpose':'user_data','expires_after[anchor]':'created_at','expires_after[seconds]':'3600'}
        prefix=b''.join(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode() for k,v in fields.items())
        prefix+=f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="uart2llm-synthetic-{uuid.uuid4().hex}.png"\r\nContent-Type: image/png\r\n\r\n'.encode()
        suffix=f'\r\n--{boundary}--\r\n'.encode()
        def body():
            yield prefix
            for i in range(0,len(png),4096):yield png[i:i+4096]
            yield suffix
        def upload():
            nonlocal file_id
            status,b=request('', 'POST',body(),{'Content-Type':'multipart/form-data; boundary='+boundary,'Content-Length':str(len(prefix)+len(png)+len(suffix))})
            assert status==200, f'upload status {status}, response bytes {len(b)}'
            value=json.loads(b);file_id=value['id']
            assert re.fullmatch(r'file-api-[A-Za-z0-9_-]+',file_id),'unexpected file identifier'
            assert value['bytes']==len(png),'stored size mismatch'
            return {'http_status':status,'stored_bytes':value['bytes'],'expires_at':value.get('expires_at')}
        case('streamed-multipart-upload',upload)
        def metadata():
            status,b=request('/'+file_id);value=json.loads(b)
            assert status==200 and value['bytes']==len(png)
            return {'http_status':status,'stored_bytes':value['bytes']}
        case('retrieve-uploaded-file-metadata',metadata)
        def content():
            status,b=request('/'+file_id+'/content')
            if status==200:assert hashlib.sha256(b).digest()==hashlib.sha256(png).digest(),'download checksum mismatch'
            return {'http_status':status,'download_verified':status==200,'bytes':len(b),'sha256':hashlib.sha256(b).hexdigest()}
        case('content-download-support-probe',content)
        report['passed']=True
    except Exception as e:report['failure']=str(e)
    finally:
        if file_id and re.fullmatch(r'file-api-[A-Za-z0-9_-]+',file_id):
            def delete_created():
                status,b=request('/'+file_id,'DELETE')
                assert status==200 and json.loads(b).get('deleted') is True,'test file cleanup failed'
                return {'deleted':True}
            try:case('delete-only-created-test-file',delete_created)
            except Exception:report['passed']=False
        report['finished_utc']=datetime.now(timezone.utc).isoformat()
        args.report.parent.mkdir(parents=True,exist_ok=True)
        args.report.write_text(json.dumps(report,indent=2),encoding='utf-8')
        print('Report: '+str(args.report),flush=True)
    return 0 if report['passed'] else 1


if __name__=='__main__':raise SystemExit(main())
