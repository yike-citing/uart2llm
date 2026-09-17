#!/usr/bin/env python3
"""Configure the authorized device through the real daemon and test its upstream.

All HTTP client traffic here is loopback. Credentials come only from a private
input file; upstream access must go through the production serial tunnel.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.request

from hardware_check_support import sanitize, default_executable


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--input',type=Path,required=True)
    p.add_argument('--exe',type=Path,default=default_executable())
    p.add_argument('--pairing',type=Path,required=True)
    p.add_argument('--port',default='COM4')
    p.add_argument('--report',type=Path,required=True)
    p.add_argument('--chat',action='store_true',help='Run two small generation requests with a 32-token output limit')
    p.add_argument('--soak-seconds',type=int,default=0)
    args=p.parse_args()
    if args.report.exists(): p.error('report already exists')
    if not 0<=args.soak_seconds<=3600: p.error('soak must be 0..3600 seconds')
    report={'started_utc':datetime.now(timezone.utc).isoformat(),'cases':[],
            'host_sha256':hashlib.sha256(args.exe.read_bytes()).hexdigest(),
            'physical_transport':'native USB validation','host_network_isolated':False,
            'all_test_http_destinations':'localhost','passed':False}
    exe=str(args.exe.resolve())
    def cli(*params):
        return subprocess.run([exe,*params],capture_output=True,text=True,check=True,timeout=45).stdout.strip()
    def case(name,action):
        item={'name':name};start=time.monotonic()
        try:
            value=action();item.update(status='passed',evidence=sanitize(value));print(name+': PASS',flush=True);return value
        except Exception as e:
            item.update(status='failed',error=str(e));print(name+': FAIL - '+str(e),flush=True);raise
        finally:
            item['seconds']=round(time.monotonic()-start,3);report['cases'].append(item)
    try:
        private=json.loads(args.input.read_text(encoding='utf-8'))
        cli('init');case('final-windows-daemon-starts',lambda:cli('start'))
        admin_token=cli('token','admin');api_token=cli('token','api')
        opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
        def http(path,method='GET',data=None,admin=True,expected=200):
            req=urllib.request.Request(('http://localhost:8766/admin/v1' if admin else 'http://localhost:8765')+path,
                data=json.dumps(data).encode() if data is not None else None,method=method,
                headers={'Authorization':'Bearer '+(admin_token if admin else api_token),'Content-Type':'application/json'})
            try: response=opener.open(req,timeout=150)
            except urllib.error.HTTPError as e: response=e
            if expected is not None and response.status!=expected:
                status=response.status;body=response.read(4096);response.close()
                try: detail=sanitize(json.loads(body))
                except ValueError: detail={'body_bytes':len(body),'sha256':hashlib.sha256(body).hexdigest()}
                raise AssertionError(f'{method} {path}: HTTP {status}, expected {expected}; {detail}')
            return response
        def admin(path,method='GET',data=None):
            with http(path,method,data) as response: return json.load(response)
        def ready(predicate=lambda s:True,seconds=35):
            end=time.monotonic()+seconds
            while time.monotonic()<end:
                s=admin('/state');d=s.get('device') or {};c=s.get('connection') or {}
                if c.get('paired') and d.get('session')==c.get('link',{}).get('session') and predicate(s): return s
                time.sleep(.5)
            raise TimeoutError('device state did not meet readiness condition')
        state=admin('/state')
        if not state.get('connection',{}).get('connected'):
            case('connect-device',lambda:admin('/connect','POST',{'port':args.port,'baud':115200,'flow_control':False}))
        if not admin('/state').get('connection',{}).get('paired'):
            case('security2-pair',lambda:admin('/pair','POST',json.loads(args.pairing.read_text(encoding='utf-8'))))
        case('fresh-authenticated-device',ready)
        case('configure-upstream',lambda:admin('/config','PATCH',{'host':{'upstream_url':private['upstream_url']}}))
        case('save-upstream-credential',lambda:admin('/credentials','POST',{'upstream_key':private['upstream_key']}))
        def wifi():
            snapshot=admin('/config')['device']
            if snapshot['pending']:
                admin('/config/rollback','POST');time.sleep(2)
            admin('/config','PATCH',{'device':{'wifi.ssid':private['wifi_ssid'],'wifi.password':private['wifi_password']}})
            try:
                state=ready(lambda s:s['device']['network']['connected'],seconds=24)
                admin('/config/confirm','POST')
                return state
            except Exception:
                admin('/config/rollback','POST');raise
        case('wifi-IP-and-confirmed-configuration',wifi)
        def models():
            with http('/v1/models',admin=False) as response: value=json.load(response)
            ids=[m['id'] for m in value['data']]
            assert ids,'empty model list'
            return {'models':ids}
        found=case('models-via-ESP32-TLS',models)['models']
        def parallel_models():
            with ThreadPoolExecutor(max_workers=4) as pool:
                values=list(pool.map(lambda _:models(),range(4)))
            return {'completed':len(values),'same_models':all(v==values[0] for v in values)}
        case('four-concurrent-model-queries',parallel_models)
        if args.chat:
            model='deepseek-flash' if 'deepseek-flash' in found else found[0]
            payload={'model':model,'messages':[{'role':'user','content':'Reply with only OK.'}],
                     'max_tokens':32,'thinking':{'type':'disabled'},'stream':False}
            def normal_chat():
                with http('/v1/chat/completions','POST',payload,admin=False) as response: value=json.load(response)
                assert value.get('choices') and value['choices'][0].get('message')
                return {'model':value.get('model'),'usage':value.get('usage'),'finish_reason':value['choices'][0].get('finish_reason')}
            case('bounded-nonstream-chat',normal_chat)
            def stream_chat():
                body=dict(payload,stream=True,stream_options={'include_usage':True})
                start=time.monotonic();first=None;events=0;done=False;usage=None;content_bytes=0
                with http('/v1/chat/completions','POST',body,admin=False) as response:
                    assert 'text/event-stream' in response.headers.get('Content-Type','')
                    for line in response:
                        if not line.startswith(b'data:'): continue
                        data=line[5:].strip()
                        if data==b'[DONE]': done=True;break
                        event=json.loads(data);events+=1
                        if first is None:first=time.monotonic()-start
                        if event.get('usage'):usage=event['usage']
                        for choice in event.get('choices',[]):
                            content_bytes+=len(choice.get('delta',{}).get('content','').encode())
                assert done and events and content_bytes,'incomplete SSE response'
                return {'events':events,'done':done,'content_bytes':content_bytes,'first_event_seconds':first,'usage':usage}
            case('bounded-SSE-chat',stream_chat)
        for path in ['/v1/files','/v1/fine_tuning/jobs']:
            def probe(path=path):
                with http(path,admin=False,expected=None) as response:
                    b=response.read(65536)
                    return {'http_status':response.status,'content_type':response.headers.get('Content-Type'),
                            'bytes':len(b),'body_sha256':hashlib.sha256(b).hexdigest()}
            case('upstream-resource-probe-'+path.rsplit('/',1)[-1],probe)
        if args.soak_seconds:
            def soak():
                start=time.monotonic();initial=ready();session=initial['device']['session'];samples=[];rounds=0
                report['partial_soak']={'samples':samples}
                while time.monotonic()-start<args.soak_seconds:
                    s=admin('/state');d=s.get('device') or {}
                    assert d.get('session')==session and s['connection']['paired'],'unexpected session loss'
                    assert d['network']['connected'],'Wi-Fi lost'
                    assert not d['crash']['available'],'new coredump'
                    assert d['memory']['internal_free']>=32768,'internal memory exhausted'
                    tasks=admin('/tasks')['device'];ids=[t['id'] for t in tasks]
                    assert len(ids)==len(set(ids)),'duplicate task enumeration'
                    admin('/config');admin('/logs');admin('/diagnostics','POST',{'action':'health'})
                    samples.append({'seconds':round(time.monotonic()-start,2),'memory':d['memory']})
                    rounds+=1
                    if rounds%20==0:print(f'management soak: {round(time.monotonic()-start)}s, {rounds} rounds',flush=True)
                    time.sleep(1)
                report.pop('partial_soak',None)
                return {'seconds':round(time.monotonic()-start,3),'rounds':rounds,'samples':samples}
            case('network-connected-management-soak',soak)
        case('final-device-state',ready)
        report['passed']=True
    except Exception as e:
        report['failure']=str(e)
    finally:
        report['finished_utc']=datetime.now(timezone.utc).isoformat()
        args.report.parent.mkdir(parents=True,exist_ok=True)
        args.report.write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
        print('Report: '+str(args.report),flush=True)
    return 0 if report['passed'] else 1


if __name__=='__main__': raise SystemExit(main())
