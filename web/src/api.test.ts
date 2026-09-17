import {describe, it, expect, vi, afterEach} from 'vitest';
import {parsePatch, request, APIError, withDeadline} from './api';
afterEach(() => {vi.unstubAllGlobals();vi.restoreAllMocks();vi.useRealTimers();});
describe('configuration boundaries', () => {
  it('accepts only explicitly supplied scopes and does not materialize redacted secrets', () => expect(parsePatch('{"device":{"wifi":{"ssid":"lab"}}}')).toEqual({device:{wifi:{ssid:'lab'}}}));
  it.each(['null','[]','{"password":"x"}','{"device":null}','{"host":[]}','{"host":{},"device":{}}'])('rejects invalid patch %s', value => expect(() => parsePatch(value)).toThrow());
});
describe('management requests', () => {
  it('bootstraps without the newer AbortSignal static helpers', async () => {
    vi.stubGlobal('AbortSignal', {});
    const fetch = vi.fn().mockResolvedValueOnce(new Response('expired',{status:401}))
      .mockResolvedValueOnce(new Response('{}')).mockResolvedValueOnce(new Response('{"connected":true}'));
    vi.stubGlobal('fetch', fetch);
    expect(await withDeadline(signal => request('/state','GET',undefined,signal))).toEqual({connected:true});
    expect(fetch).toHaveBeenCalledTimes(3);
  });
  it('uses same-origin cookie authentication without persisting tokens', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response('{"connected":false}'));
    vi.stubGlobal('fetch', fetch);
    expect(await request('/state')).toEqual({connected:false});
    expect(fetch.mock.calls[0][0]).toBe('/admin/v1/state');
    expect(fetch.mock.calls[0][1].credentials).toBe('same-origin');
  });
  it('surfaces failed device operations', async () => { vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('unpaired',{status:409}))); await expect(request('/pair','POST',{})).rejects.toBeInstanceOf(APIError); });
  it('does not mistake an SPA fallback page for a successful authenticated snapshot', async () => {vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('<!doctype html>')));await expect(request('/state')).rejects.toBeInstanceOf(APIError);});
  it('automatically establishes a passwordless session on first access and retries the refused request', async () => {
    const fetch = vi.fn().mockResolvedValueOnce(new Response('unauthorized', {status:401}))
      .mockResolvedValueOnce(new Response('{"authenticated":true}'))
      .mockResolvedValueOnce(new Response('{"connected":true}'));
    vi.stubGlobal('fetch', fetch);
    expect(await request('/state')).toEqual({connected:true});
    expect(fetch.mock.calls.map(call => call[0])).toEqual(['/admin/v1/state','/admin/v1/session','/admin/v1/state']);
    expect(fetch.mock.calls[1][1]).toMatchObject({method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json'},body:'{}'});
  });
  it('shares session renewal between concurrent refused requests', async () => {
    let release!: (response: Response) => void;
    const session = new Promise<Response>(resolve => {release = resolve;});
    const calls = new Map<string, number>();
    const fetch = vi.fn((path: string) => {
      const count = (calls.get(path) ?? 0) + 1; calls.set(path, count);
      if (path.endsWith('/session')) return session;
      return Promise.resolve(count === 1 ? new Response('expired',{status:401}) : new Response('{}'));
    });
    vi.stubGlobal('fetch', fetch);
    const responses = [request('/state'), request('/config'), request('/capabilities')];
    await vi.waitFor(() => expect(calls.get('/admin/v1/session')).toBe(1));
    release(new Response('{}'));
    await Promise.all(responses);
    expect(calls.get('/admin/v1/session')).toBe(1);
    expect(calls.get('/admin/v1/state')).toBe(2);
  });
  it.each([400,403,409,500,503])('never replays a mutation after status %i', async status => {
    const fetch = vi.fn().mockResolvedValue(new Response('operation failed',{status}));
    vi.stubGlobal('fetch', fetch);
    await expect(request('/device/action','POST',{action:'reboot'})).rejects.toMatchObject({status});
    expect(fetch).toHaveBeenCalledTimes(1);
  });
  it('reuses an already renewed session when another old request returns a late 401', async () => {
    let release!: (response: Response) => void;
    const delayed = new Promise<Response>(resolve => {release = resolve;});
    const calls = new Map<string, number>();
    const fetch = vi.fn((path: string) => {
      const count = (calls.get(path) ?? 0) + 1; calls.set(path, count);
      if (path.endsWith('/config') && count === 1) return delayed;
      return Promise.resolve(path.endsWith('/state') && count === 1 ? new Response('expired',{status:401}) : new Response('{}'));
    });
    vi.stubGlobal('fetch', fetch);
    const config = request('/config');
    await request('/state');
    release(new Response('expired',{status:401}));
    await config;
    expect(calls.get('/admin/v1/session')).toBe(1);
    expect(calls.get('/admin/v1/config')).toBe(2);
  });
  it('never replays a mutation after a network failure', async () => {
    const fetch = vi.fn().mockRejectedValue(new TypeError('connection lost'));
    vi.stubGlobal('fetch', fetch);
    await expect(request('/config/confirm','POST')).rejects.toThrow('connection lost');
    expect(fetch).toHaveBeenCalledTimes(1);
  });
  it('does not loop when the replacement session is rejected too', async () => {
    const fetch = vi.fn().mockResolvedValueOnce(new Response('expired',{status:401}))
      .mockResolvedValueOnce(new Response('{}'))
      .mockResolvedValueOnce(new Response('still unauthorized',{status:401}));
    vi.stubGlobal('fetch', fetch);
    await expect(request('/state')).rejects.toMatchObject({status:401});
    expect(fetch).toHaveBeenCalledTimes(3);
  });
  it('clears a failed session attempt so a later explicit retry can succeed', async () => {
    const fetch = vi.fn().mockResolvedValueOnce(new Response('expired',{status:401}))
      .mockResolvedValueOnce(new Response('forbidden',{status:403}))
      .mockResolvedValueOnce(new Response('expired',{status:401}))
      .mockResolvedValueOnce(new Response('{}'))
      .mockResolvedValueOnce(new Response('{"connected":true}'));
    vi.stubGlobal('fetch', fetch);
    await expect(request('/state')).rejects.toMatchObject({status:403});
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(await request('/state')).toEqual({connected:true});
    expect(fetch).toHaveBeenCalledTimes(5);
  });
});
describe('connection deadlines', () => {
  it('cancels a stalled connection at the deadline and clears its timer and parent listener', async () => {
    vi.useFakeTimers();
    const parent = new AbortController();
    const remove = vi.spyOn(parent.signal,'removeEventListener');
    let connectionSignal!: AbortSignal;
    const pending = withDeadline(signal => {connectionSignal = signal;return new Promise<never>(()=>{});}, parent.signal);
    const assertion = expect(pending).rejects.toMatchObject({name:'TimeoutError'});
    await vi.advanceTimersByTimeAsync(9999);
    expect(connectionSignal.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    await assertion;
    expect(connectionSignal.aborted).toBe(true);
    expect(vi.getTimerCount()).toBe(0);
    expect(remove).toHaveBeenCalledWith('abort',expect.any(Function));
  });
  it('propagates effect cleanup cancellation and clears a successful connection deadline', async () => {
    vi.useFakeTimers();
    const parent = new AbortController();
    let connectionSignal!: AbortSignal;
    const pending = withDeadline(signal => {connectionSignal = signal;return new Promise<never>(()=>{});},parent.signal);
    const assertion = expect(pending).rejects.toMatchObject({name:'AbortError'});
    parent.abort();
    await assertion;
    expect(connectionSignal.aborted).toBe(true);
    expect(vi.getTimerCount()).toBe(0);
    expect(await withDeadline(async()=> 'connected')).toBe('connected');
    expect(vi.getTimerCount()).toBe(0);
  });
});
