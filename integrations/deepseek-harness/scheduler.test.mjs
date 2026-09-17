import test from 'node:test';
import assert from 'node:assert/strict';
import {Admission, apply} from './scheduler.mjs';

const consume = async stream => { const chunks = []; for await (const c of stream) chunks.push(c); return chunks; };
test('close and throw retain admission while the provider yields cleanup or recovery', async () => {
  for (const method of ['return', 'throw']) {
    const pool = new Admission({maxActive: 1}); let finished = false;
    const stream = pool.stream({}, async function* () {
      try { yield 'body'; }
      catch { yield 'recovered'; }
      finally { yield 'cleanup'; finished = true; }
    });
    assert.equal((await stream.next()).value, 'body');
    const result = await stream[method](new Error('test interruption'));
    assert.equal(result.done, false);
    assert.equal(pool.snapshot().active, 1);
    assert.equal(finished, false);
    let next;
    do { next = await stream.next(); } while (!next.done);
    assert.equal(finished, true);
    assert.equal(pool.snapshot().active, 0);
    assert.equal(pool.snapshot().released, 1);
  }
});
test('throw closes providers without a throw method', async () => {
  const pool = new Admission(); let closed = false;
  const stream = pool.stream({}, () => ({
    [Symbol.asyncIterator]() { return this; },
    async next() { return {done: false, value: 1}; },
    async return() { closed = true; return {done: true}; },
  }));
  await stream.next();
  await assert.rejects(stream.throw(new Error('original')), /original/);
  assert.equal(closed, true); assert.equal(pool.snapshot().active, 0);
});
test('eight mixed requests stay within four; unchanged chunks, one dispatch each', async () => {
  const pool = new Admission(); let active = 0, peak = 0, calls = 0;
  const result = await Promise.all(Array.from({length: 8}, (_, i) => consume(pool.stream(
    Object.freeze({purpose: i % 2 ? 'session-title' : undefined}), async function* () {
      calls++; active++; peak = Math.max(peak, active);
      try { yield i; await new Promise(r => setTimeout(r, 5)); yield {finish: i}; }
      finally { active--; }
    }))));
  assert.equal(calls, 8); assert.equal(peak, 4);
  assert.deepEqual(result, Array.from({length: 8}, (_, i) => [i, {finish: i}]));
  assert.equal(pool.snapshot().queued, 0); assert.equal(pool.snapshot().active, 0);
});
test('queue overflow and queued cancellation never dispatch', async () => {
  const pool = new Admission({maxActive: 1, maxQueued: 1});
  const release = await pool.acquire(); const abort = new AbortController();
  let calls = 0;
  const queued = consume(pool.stream({signal: abort.signal}, async function* () { calls++; yield 1; }));
  await assert.rejects(pool.acquire(), {code: 'UART2LLM_QUEUE_FULL'});
  abort.abort(); await assert.rejects(queued); release();
  assert.equal(calls, 0); assert.equal(pool.snapshot().active, 0); assert.equal(pool.snapshot().queued, 0);
});
test('FIFO admission prevents titles or ordinary requests starving', async () => {
  const pool = new Admission({maxActive: 1}); const release = await pool.acquire(); const order = [];
  const requests = Array.from({length: 4}, (_, i) => pool.acquire(undefined, i % 2 === 0).then(r => {order.push(i); r();}));
  release(); await Promise.all(requests); assert.deepEqual(order, [0, 1, 2, 3]);
});
test('timeout removes waiter; late release cannot dispatch it', async () => {
  const pool = new Admission({maxActive: 1, waitMs: 10}); const release = await pool.acquire();
  await assert.rejects(pool.acquire(), {code: 'UART2LLM_QUEUE_TIMEOUT'});
  release(); release(); assert.equal(pool.snapshot().active, 0); assert.equal(pool.snapshot().timedOut, 1);
});
test('consumer cancellation and provider errors release admission without replay', async () => {
  const pool = new Admission({maxActive: 1}); let closed = 0, calls = 0;
  const stream = pool.stream({}, async function* () {calls++; try {yield 1; yield 2;} finally {closed++;}});
  assert.equal((await stream.next()).value, 1); await stream.return();
  assert.equal(closed, 1); assert.equal(pool.snapshot().active, 0);
  await assert.rejects(consume(pool.stream({}, async function* () {calls++; throw new Error('test');})));
  assert.equal(calls, 2); assert.equal(pool.snapshot().active, 0);
});
test('abort in granted microtask does not dispatch and releases its lease', async () => {
  const pool = new Admission(); const abort = new AbortController(); let calls = 0;
  const pending = consume(pool.stream({signal: abort.signal}, async function* () {calls++; yield 1;}));
  abort.abort(); await assert.rejects(pending); assert.equal(calls, 0); assert.equal(pool.snapshot().active, 0);
});
test('live loader replacement shares admission; unrelated providers are unchanged', async () => {
  const root = {}, handlers = [], pools = [];
  const ctx = () => ({root, provide: (_, p) => pools.push(p), on: (_, fn) => handlers.push(fn)});
  apply(ctx()); apply(ctx()); assert.equal(pools[0], pools[1]);
  const release = await pools[0].acquire(); assert.equal(pools[1].snapshot().active, 1); release();
  assert.deepEqual(await consume(handlers[0]({provider: 'other'}, async function* () {yield 'unchanged';})), ['unchanged']);
  assert.throws(() => apply(ctx(), {maxActive: 3}), /Restart Harness/);
});
test('return while queued never dispatches an abandoned request', async () => {
  const pool = new Admission({maxActive: 1}); const release = await pool.acquire(); let calls = 0;
  const stream = pool.stream({}, async function* () {calls++; yield 'must not send';});
  const pending = stream.next(); const rejected = assert.rejects(pending);
  await stream.return(); await rejected; release();
  assert.equal(calls, 0); assert.equal(pool.snapshot().active, 0); assert.equal(pool.snapshot().queued, 0);
});
test('delayed event-loop timers cannot admit an expired request', async () => {
  const pool = new Admission({maxActive: 1, waitMs: 10}); const release = await pool.acquire();
  const pending = pool.acquire(); const rejected = assert.rejects(pending, {code: 'UART2LLM_QUEUE_TIMEOUT'});
  const until = performance.now()+30; while (performance.now() < until) { /* deliberate timer delay */ }
  release(); await rejected; assert.equal(pool.snapshot().admitted, 1); assert.equal(pool.snapshot().timedOut, 1);
});
