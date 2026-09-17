#!/usr/bin/env python3
"""Real-device management checks. Requires an explicitly selected COM port and pairing file."""
import argparse
import base64
import hashlib
import http.client
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone


def sanitize(value):
    if isinstance(value, dict):
        return {k: ('[redacted]' if any(s in k.lower() for s in ('password', 'token', 'ssid', 'bssid', 'username', 'upstream_key')) else sanitize(v)) for k, v in value.items()}
    if isinstance(value, list):
        return [sanitize(v) for v in value]
    return value


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--exe', type=Path, default=Path('dist/uart2llm.exe'))
    p.add_argument('--port', required=True)
    p.add_argument('--pairing', required=True, type=Path)
    p.add_argument('--state-dir', required=True, type=Path)
    p.add_argument('--report', required=True, type=Path)
    p.add_argument('--phase', choices=('smoke', 'config', 'crash', 'maintenance', 'rollback', 'soak'), default='smoke')
    p.add_argument('--image', type=Path, help='Matching USB validation application for OTA checks')
    p.add_argument('--seconds', type=int, default=300, help='Soak duration (60..86400)')
    p.add_argument('--upload-timeout', type=int, default=1200, help='HTTP response inactivity timeout for the OTA test')
    args = p.parse_args()
    if args.phase in ('maintenance', 'rollback') and not args.image:
        p.error('--image is required for maintenance/rollback')
    if not 60 <= args.seconds <= 86400:
        p.error('--seconds must be 60..86400')
    if args.report.exists():
        p.error('--report already exists; choose a new evidence filename')
    exe = str(args.exe.resolve())
    args.state_dir.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env['UART2LLM_DATA_DIR'] = str(args.state_dir.resolve())
    run = lambda *a: subprocess.run([exe, *a], env=env, capture_output=True, text=True, timeout=20, check=True).stdout
    run('init')
    cfg_path = args.state_dir / 'config.json'
    cfg = json.loads(cfg_path.read_text(encoding='utf-8'))
    cfg.update(api_listen='localhost:18965', admin_listen='localhost:18966', serial_port='')
    cfg_path.write_text(json.dumps(cfg), encoding='utf-8')
    token = run('token', 'admin').strip()
    pairing = json.loads(args.pairing.read_text(encoding='utf-8'))
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    report = {'started_utc': datetime.now(timezone.utc).isoformat(), 'port': args.port,
              'phase': args.phase, 'physical_transport': 'pending capability check',
              'gpio_uart_verified': False, 'wifi_api_verified': False, 'cases': [],
              'host_binary_sha256': hashlib.sha256(args.exe.read_bytes()).hexdigest(),
              'harness_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
    if args.image:
        report['ota_image']={'bytes':args.image.stat().st_size,'sha256':hashlib.sha256(args.image.read_bytes()).hexdigest()}
    log = (args.state_dir / 'daemon.log').open('w', encoding='utf-8')
    daemon = subprocess.Popen([exe, 'serve'], env=env, stdout=log, stderr=subprocess.STDOUT,
                              creationflags=0x08000000 if os.name == 'nt' else 0)

    def request(path, method='GET', data=None, expected=200, timeout=40):
        req = urllib.request.Request('http://localhost:18966/admin/v1'+path,
                                     data=json.dumps(data).encode() if data is not None else None,
                                     method=method, headers={'Authorization': 'Bearer '+token, 'Content-Type': 'application/json'})
        try:
            response = opener.open(req, timeout=timeout)
        except urllib.error.HTTPError as e:
            response = e
        with response:
            body = response.read(2*1024*1024)
            status = response.status
        result = json.loads(body)
        if status != expected:
            raise AssertionError(f'{method} {path}: HTTP {status}, expected {expected}; {sanitize(result)}')
        return result

    def check(name, action):
        started = time.monotonic()
        item = {'name': name}
        try:
            value = action()
            item.update(status='passed', evidence=sanitize(value))
            print(name+': PASS', flush=True)
            return value
        except Exception as e:
            item.update(status='failed', error=str(e))
            print(name+': FAIL - '+str(e), flush=True)
            raise
        finally:
            item['seconds'] = round(time.monotonic()-started, 3)
            report['cases'].append(item)

    def wait_ready(paired=False, old_session=None, predicate=None):
        deadline = time.monotonic()+35
        while time.monotonic() < deadline:
            if daemon.poll() is not None:
                raise RuntimeError('daemon exited; inspect private daemon log')
            try:
                state = request('/state')
                conn = state.get('connection', {})
                session = conn.get('link', {}).get('session')
                if not paired or (conn.get('paired') and (state.get('device') or {}).get('session') == session
                                  and (old_session is None or session != old_session)):
                    if predicate is None or predicate(state):
                        return state
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(0.5)
        raise TimeoutError('device/daemon readiness deadline')

    def configuration():
        nonlocal config_restore
        original = request('/config')['device']['persisted']['telemetry.interval_ms']
        config_restore = original
        target = 1300 if original != 1300 else 1400
        def apply(value):
            request('/config', 'PATCH', {'device': {'telemetry.interval_ms': value}})
            time.sleep(1.5)
        def confirm(value):
            apply(value)
            request('/config/confirm', 'POST')
            result = request('/config')['device']
            assert result['persisted']['telemetry.interval_ms'] == value
            return result
        check('config-confirm', lambda: confirm(target))
        def restart():
            old = request('/state')['connection']['link']['session']
            request('/device/action', 'POST', {'action': 'reboot'})
            time.sleep(2)
            wait_ready(True, old)
            value = request('/config')['device']
            assert value['persisted']['telemetry.interval_ms'] == target
            return value
        check('restart-persists-confirmed-config', restart)
        def rollback():
            apply(target+200)
            request('/config/rollback', 'POST')
            time.sleep(1.5)
            result = request('/config')['device']
            assert result['effective']['telemetry.interval_ms'] == target
            return result
        check('explicit-rollback', rollback)
        def automatic():
            apply(target+300)
            time.sleep(33)
            result = request('/config')['device']
            assert not result['pending'] and result['effective']['telemetry.interval_ms'] == target
            return result
        check('confirmation-timeout-rollback', automatic)
        check('restore-original-configuration', lambda: confirm(original))
        check('invalid-setting-rejected', lambda: request('/config', 'PATCH', {'device': {'telemetry.interval_ms': 1}}, expected=400))
        check('valid-setting-immediately-after-rejection', lambda: confirm(original))
        check('usb-uart-change-rejected', lambda: request('/config', 'PATCH', {'device': {'uart.baud': 230400}}, expected=400))
        check('valid-setting-after-unsupported-uart', lambda: confirm(original))
        def bad_wifi():
            before=request('/config')['device']['persisted']
            # Never change an already configured network without explicit test credentials.
            assert not before['wifi.ssid'], 'bad-Wi-Fi test requires an unconfigured board'
            request('/config','PATCH',{'device':{'wifi.ssid':'u2-no-ap-'+os.urandom(6).hex(),'wifi.password':'not-a-real-network-key'}})
            time.sleep(2)
            request('/config/confirm','POST',expected=400)
            time.sleep(33)
            after=request('/config')['device']
            assert not after['pending'] and after['persisted']==before
            assert after['effective']['wifi.ssid']==before['wifi.ssid']
            return after
        check('bad-wifi-cannot-confirm-and-auto-rolls-back',bad_wifi)

    def maintenance():
        nonlocal daemon
        image = args.image.read_bytes()
        assert image[0] == 0xE9 and len(image) <= 4*1024*1024
        def upload(data, expected):
            req = urllib.request.Request('http://localhost:18966/admin/v1/firmware', data=data, method='POST',
                    headers={'Authorization': 'Bearer '+token, 'Content-Type': 'application/octet-stream'})
            try:
                response = opener.open(req, timeout=args.upload_timeout)
            except urllib.error.HTTPError as e:
                response = e
            with response:
                value = json.loads(response.read(65536))
                assert response.status == expected, (response.status, sanitize(value))
            return value
        if args.phase == 'maintenance':
            check('corrupt-image-rejected', lambda: upload(bytes(1024), 502))
        def interrupted():
            conn = http.client.HTTPConnection('localhost', 18966, timeout=15)
            conn.putrequest('POST', '/admin/v1/firmware')
            conn.putheader('Authorization', 'Bearer '+token)
            conn.putheader('Content-Type', 'application/octet-stream')
            conn.putheader('Content-Length', str(len(image)))
            conn.endheaders()
            conn.send(image[:4096])
            time.sleep(2)
            conn.close()
            time.sleep(3)
            state = request('/state')
            assert not state['host']['firmware_upload']
            return state
        if args.phase == 'maintenance':
            check('interrupted-upload-releases-maintenance', interrupted)
        original = request('/config')['device']['persisted']
        before = request('/state')
        old_session = before['connection']['link']['session']
        old_slot = before['device']['ota']['running_partition']
        check('valid-image-upload', lambda: upload(image, 200))
        if args.phase == 'rollback':
            def unconfirmed_candidate():
                nonlocal daemon
                request('/device/action', 'POST', {'action': 'reboot'})
                request('/shutdown', 'POST')
                daemon.wait(timeout=10)
                for elapsed in range(0,140,20):
                    time.sleep(20)
                    print(f'OTA rollback: host offline for {elapsed+20} seconds',flush=True)
                daemon=subprocess.Popen([exe,'serve'],env=env,stdout=log,stderr=subprocess.STDOUT,
                                        creationflags=0x08000000 if os.name=='nt' else 0)
                # /connect persisted this port; the restarted daemon reconnects
                # automatically. A second /connect would race that operation.
                state=wait_ready(True,old_session)
                assert state['device']['ota']['running_partition']==old_slot, 'unconfirmed candidate did not roll back'
                assert state['device']['ota']['image_state']==2, 'fallback image is not valid'
                assert not state['device']['crash']['available'], 'rollback produced a coredump'
                assert request('/config')['device']['persisted']==original, 'rollback changed persisted configuration'
                return state
            check('unconfirmed-candidate-rolls-back-with-host-offline',unconfirmed_candidate)
            return
        def activation():
            request('/device/action', 'POST', {'action': 'reboot'})
            time.sleep(2)
            state = wait_ready(True, old_session, lambda s: s['device']['ota']['image_state'] == 2)
            assert state['device']['ota']['running_partition'] != old_slot, 'OTA did not select the other slot'
            assert state['device']['ota']['image_state'] == 2, 'candidate image was not confirmed valid'
            assert request('/config')['device']['persisted'] == original, 'OTA changed persisted configuration'
            return state
        check('ota-new-slot-self-test-and-config-preservation', activation)

    def soak():
        if wait_ready(True)['device']['crash']['available']:
            check('archive-coredump-before-clearing', export_crash)
        check('clear-preserved-coredump', lambda: request('/device/action','POST',{'action':'clear_crash'}))
        for index in range(5):
            def reboot():
                old = request('/state')['connection']['link']['session']
                request('/device/action','POST',{'action':'reboot'})
                time.sleep(2)
                state=wait_ready(True,old)
                assert not state['device']['crash']['available']
                assert state['device']['memory']['internal_free'] >= 32768, 'low internal heap after restart'
                assert state['device']['memory']['largest_internal_block'] >= 16384, 'fragmented internal heap after restart'
                return state
            check(f'repeated-startup-{index+1}',reboot)
        def load():
            start=time.monotonic(); deadline=start+args.seconds
            samples=[]; count=0; session=request('/state')['connection']['link']['session']
            report['partial_soak']={'session':session,'rounds_completed':0,'samples':samples}
            while time.monotonic()<deadline:
                state=request('/state')
                assert state['connection']['link']['session']==session, 'unexpected session restart'
                assert not state['device']['crash']['available'], 'new coredump'
                assert state['device']['memory']['internal_free'] >= 32768, 'internal heap exhausted'
                tasks=request('/tasks')['device']
                ids=[t['id'] for t in tasks]
                assert len(set(ids))==len(ids), 'duplicate task pagination'
                request('/config');request('/logs');request('/diagnostics','POST',{'action':'health'})
                samples.append({'elapsed_seconds':round(time.monotonic()-start,1),'memory':state['device']['memory']})
                count+=1
                report['partial_soak']['rounds_completed']=count
                if count%20==0:
                    print(f'soak: {round(time.monotonic()-start)} seconds, {count} RPC rounds',flush=True)
                time.sleep(1)
            final=request('/state')
            report.pop('partial_soak',None)
            return {'duration_seconds':round(time.monotonic()-start,1),'rounds':count,'samples':samples,'final':final}
        check('management-soak-no-restart-or-new-coredump',load)

    def tasks_snapshot():
        value=request('/tasks')
        ids=[item['id'] for item in value['device']]
        assert len(ids)==len(set(ids)), 'duplicate task IDs across pages'
        return value

    def export_crash():
        value=request('/diagnostics','POST',{'action':'crash_export'})
        data=base64.b64decode(value['data'],validate=True)
        path=args.state_dir/('crash-export-'+datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S%f')+'.bin')
        with path.open('xb') as output:
            output.write(data)
        return {'bytes':len(data),'sha256':hashlib.sha256(data).hexdigest(),'private_file':str(path)}

    failed = False
    config_restore = None
    try:
        check('daemon-ready', wait_ready)
        connected = check('serial-handshake', lambda: request('/connect', 'POST', {'port': args.port, 'baud': 115200, 'flow_control': False}))
        if not connected.get('paired'):
            check('security2-pairing', lambda: request('/pair', 'POST', pairing))
        else:
            report['cases'].append({'name': 'security2-auto-pairing', 'status': 'passed', 'evidence': 'Stored device credential successfully authenticated'})
        caps = check('actual-device-capabilities', lambda: request('/capabilities'))
        device = caps['device']
        assert device['flash_bytes'] == 16*1024*1024 and device['psram_bytes'] == 8*1024*1024
        report['physical_transport'] = device['transport']
        if args.phase in ('config', 'maintenance', 'rollback', 'soak'):
            assert device['transport'] == 'usb_serial_jtag_validation' and not device['uart_configuration_supported'], 'This test phase requires the USB validation build'
        check('real-telemetry', lambda: wait_ready(True))
        check('configuration-registry', lambda: request('/config/schema'))
        check('state-registry', lambda: request('/state/schema'))
        check('task-stack-diagnostics', tasks_snapshot)
        check('encrypted-logs-channel', lambda: request('/logs'))
        check('health-diagnostics', lambda: request('/diagnostics', 'POST', {'action': 'health'}))
        check('configuration-snapshot', lambda: request('/config'))
        if args.phase == 'config':
            configuration()
        if args.phase == 'crash':
            check('private-coredump-export',export_crash)
        if args.phase in ('maintenance', 'rollback'):
            maintenance()
        if args.phase == 'soak':
            soak()
    except Exception as e:
        failed = True
        report['failure'] = str(e)
    finally:
        if config_restore is not None:
            def restore():
                snapshot=request('/config')['device']
                if snapshot['pending']:
                    request('/config/rollback','POST')
                    time.sleep(2)
                    snapshot=request('/config')['device']
                if snapshot['persisted']['telemetry.interval_ms'] != config_restore:
                    request('/config','PATCH',{'device':{'telemetry.interval_ms':config_restore}})
                    time.sleep(1.5)
                    request('/config/confirm','POST')
                restored=request('/config')['device']
                assert not restored['pending'] and restored['persisted']['telemetry.interval_ms']==config_restore
                return {'original_telemetry_interval_ms':config_restore,'restored':True}
            try:
                check('configuration-finally-cleanup',restore)
            except Exception:
                failed=True
        try:
            request('/shutdown', 'POST')
        except Exception:
            pass
        try:
            daemon.wait(timeout=10)
        except subprocess.TimeoutExpired:
            daemon.terminate()
            daemon.wait(timeout=5)
        log.close()
        report['finished_utc'] = datetime.now(timezone.utc).isoformat()
        report['passed'] = not failed
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
        print('Report: '+str(args.report), flush=True)
    return 1 if failed else 0


if __name__ == '__main__':
    raise SystemExit(main())
