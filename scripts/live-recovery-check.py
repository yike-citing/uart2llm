#!/usr/bin/env python3
"""Check persistence and request capacity on an already configured local daemon."""
import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime,timezone
import json
from pathlib import Path
import subprocess
import threading
import time
import urllib.error
import urllib.request
from hardware_check_support import sanitize, default_executable


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--report',type=Path,required=True)
    p.add_argument('--exe',type=Path,default=default_executable())
    a=p.parse_args()
    if a.report.exists():p.error('report exists')
    report={'started_utc':datetime.now(timezone.utc).isoformat(),'cases':[],'passed':False}
    tokens={kind:subprocess.run([str(a.exe),'token',kind],capture_output=True,text=True,check=True).stdout.strip() for kind in ['api','admin']}
    def request(path,method='GET',value=None,admin=True):
        op=urllib.request.build_opener(urllib.request.ProxyHandler({}))
        req=urllib.request.Request(('http://localhost:8766/admin/v1' if admin else 'http://localhost:8765')+path,
            method=method,data=json.dumps(value).encode() if value is not None else None,
            headers={'Authorization':'Bearer '+tokens['admin' if admin else 'api'],'Content-Type':'application/json'})
        try:r=op.open(req,timeout=45)
        except urllib.error.HTTPError as e:r=e
        with r:return r.status,json.load(r),r.headers.get('Retry-After')
    def admin(path,method='GET',value=None):
        status,b,_=request(path,method,value);assert status==200, f'{path}: HTTP {status}';return b
    def case(name,action):
        item={'name':name};start=time.monotonic()
        try:
            value=action();item.update(status='passed',evidence=sanitize(value));print(name+': PASS',flush=True);return value
        except Exception as e:item.update(status='failed',error=str(e));print(name+': FAIL - '+str(e),flush=True);raise
        finally:item['seconds']=round(time.monotonic()-start,3);report['cases'].append(item)
    try:
        before=admin('/state');original=admin('/config')['device']['persisted']
        old_session=before['connection']['link']['session']
        def reboot():
            admin('/device/action','POST',{'action':'reboot'})
            deadline=time.monotonic()+50
            while time.monotonic()<deadline:
                s=admin('/state');c=s.get('connection',{});d=s.get('device') or {}
                if c.get('paired') and d.get('session')==c.get('link',{}).get('session') and d.get('session')!=old_session and d.get('network',{}).get('connected'):
                    assert not d['crash']['available'],'new coredump'
                    cfg=admin('/config')['device'];assert not cfg['pending'] and cfg['persisted']==original,'persisted configuration changed'
                    return s
                time.sleep(.5)
            raise TimeoutError('Wi-Fi and secure management did not recover')
        case('restart-retains-wifi-and-reconnects',reboot)
        def capacity():
            barrier=threading.Barrier(6)
            def one(_):
                barrier.wait(timeout=10);start=time.monotonic()
                status,b,retry=request('/v1/models',admin=False)
                return {'http_status':status,'retry_after':retry,'seconds':round(time.monotonic()-start,3),'model_count':len(b.get('data',[]))}
            with ThreadPoolExecutor(max_workers=5) as pool:
                tasks=[pool.submit(one,i) for i in range(5)]
                barrier.wait(timeout=10)
                results=[f.result() for f in tasks]
            assert sorted(v['http_status'] for v in results)==[200,200,200,200,503],results
            assert next(v for v in results if v['http_status']==503)['retry_after'],'missing Retry-After'
            return results
        case('four-real-requests-and-fifth-503',capacity)
        case('final-live-device-state',lambda:admin('/state'))
        report['passed']=True
    except Exception as e:report['failure']=str(e)
    finally:
        report['finished_utc']=datetime.now(timezone.utc).isoformat()
        a.report.parent.mkdir(parents=True,exist_ok=True)
        a.report.write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
        print('Report: '+str(a.report),flush=True)
    return 0 if report['passed'] else 1


if __name__=='__main__':raise SystemExit(main())
