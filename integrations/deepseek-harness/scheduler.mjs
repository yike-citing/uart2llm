/** Client-side admission only. Never changes or retries a provider request. */
export const name = 'uart2llm-harness-scheduler';
export const inject = ['llm'];

function failure(code, message) {
  const error = new Error(message);
  error.code = code;
  return error;
}
function aborted(signal) {
  return signal?.reason ?? failure('UART2LLM_CANCELLED', 'Request cancelled before dispatch');
}

export class Admission {
  constructor({maxActive = 4, maxQueued = 16, waitMs = 120000} = {}) {
    if (!Number.isInteger(maxActive) || maxActive < 1 || maxActive > 4 ||
        !Number.isInteger(maxQueued) || maxQueued < 1 || maxQueued > 32 ||
        !Number.isInteger(waitMs) || waitMs < 1 || waitMs > 300000) {
      throw new Error('Invalid uart2llm admission limits');
    }
    Object.assign(this, {maxActive, maxQueued, waitMs, active: 0, queue: []});
    this.counts = {admitted: 0, released: 0, rejected: 0, cancelled: 0, timedOut: 0, peakActive: 0, peakQueued: 0};
  }
  snapshot() {
    return {...this.counts, active: this.active, queued: this.queue.length,
      titleQueued: this.queue.filter(q => q.title).length,
      maxActive: this.maxActive, maxQueued: this.maxQueued, waitMs: this.waitMs};
  }
  lease() {
    this.active++;
    this.counts.admitted++;
    this.counts.peakActive = Math.max(this.counts.peakActive, this.active);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.active--;
      this.counts.released++;
      this.drain();
    };
  }
  drain() {
    while (this.active < this.maxActive && this.queue.length) {
      const item = this.queue.shift();
      item.clear();
      if (item.signal?.aborted) {
        this.counts.cancelled++;
        item.reject(aborted(item.signal));
      } else if (performance.now() >= item.deadline) {
        this.counts.timedOut++;
        item.reject(failure('UART2LLM_QUEUE_TIMEOUT', 'Harness local endpoint queue wait expired; request was not sent'));
      } else item.resolve(this.lease());
    }
  }
  acquire(signal, title = false) {
    if (signal?.aborted) {
      this.counts.cancelled++;
      return Promise.reject(aborted(signal));
    }
    if (this.active < this.maxActive && !this.queue.length) return Promise.resolve(this.lease());
    if (this.queue.length >= this.maxQueued) {
      this.counts.rejected++;
      return Promise.reject(failure('UART2LLM_QUEUE_FULL', 'Harness local endpoint queue is full; request was not sent'));
    }
    return new Promise((resolve, reject) => {
      const item = {resolve, reject, signal, title, deadline: performance.now() + this.waitMs};
      const remove = (counter, error) => {
        const index = this.queue.indexOf(item);
        if (index < 0) return;
        this.queue.splice(index, 1);
        item.clear();
        this.counts[counter]++;
        reject(error);
      };
      const onAbort = () => remove('cancelled', aborted(signal));
      const timer = setTimeout(() => remove('timedOut', failure('UART2LLM_QUEUE_TIMEOUT',
        'Harness local endpoint queue wait expired; request was not sent')), this.waitMs);
      item.clear = () => { clearTimeout(timer); signal?.removeEventListener('abort', onAbort); };
      this.queue.push(item);
      this.counts.peakQueued = Math.max(this.counts.peakQueued, this.queue.length);
      signal?.addEventListener('abort', onAbort, {once: true});
      if (signal?.aborted) onAbort();
    });
  }
  stream(options, next) {
    const wait = new AbortController();
    const onAbort = () => wait.abort(aborted(options.signal));
    options.signal?.addEventListener('abort', onAbort, {once: true});
    if (options.signal?.aborted) onAbort();
    let pending, iterator, release, closed = false;
    const cleanup = () => { release?.(); release = undefined; options.signal?.removeEventListener('abort', onAbort); };
    const begin = () => pending ??= (async () => {
      release = await this.acquire(wait.signal, options.purpose === 'session-title');
      if (closed || wait.signal.aborted) { cleanup(); throw aborted(wait.signal); }
      iterator = next()[Symbol.asyncIterator]();
      return iterator;
    })();
    // An async generator queues return() behind a pending next(). This explicit
    // iterator lets close remove queued admission immediately, before dispatch.
    return {
      [Symbol.asyncIterator]() { return this; },
      async next(value) {
        if (closed) return {done: true, value: undefined};
        try {
          const inner = await begin();
          if (closed) return {done: true, value: undefined};
          const result = await inner.next(value);
          if (result.done) { closed = true; cleanup(); }
          return result;
        } catch (error) { closed = true; cleanup(); throw error; }
      },
      async return(value) {
        if (!iterator) {
          closed = true; wait.abort();
          try { await pending?.catch(() => {}); return {done: true, value}; }
          finally { cleanup(); }
        }
        try {
          const result = await iterator.return?.(value) ?? {done: true, value};
          if (result.done) { closed = true; cleanup(); }
          return result;
        } catch (error) { closed = true; cleanup(); throw error; }
      },
      async throw(error) {
        if (!iterator) {
          closed = true; wait.abort(error);
          try { await pending?.catch(() => {}); throw error; }
          finally { cleanup(); }
        }
        try {
          if (iterator.throw) {
            const result = await iterator.throw(error);
            if (result.done) { closed = true; cleanup(); }
            return result;
          }
          // A provider without throw still needs its normal close hook.
          // If close yields, keep the lease until the consumer drains it.
          const result = await iterator.return?.() ?? {done: true};
          if (!result.done) return result;
          throw error;
        } catch (error) { closed = true; cleanup(); throw error; }
      },
    };
  }
}

// Shared across loader generations so a live patch reload cannot reset active
// admission while the old generation still owns in-flight streams.
const registryKey = Symbol.for('uart2llm.harness.admission.v1');
const registry = globalThis[registryKey] ??= new WeakMap();
export function apply(ctx, config = {}) {
  const provider = config.provider ?? 'uart2llm';
  if (typeof provider !== 'string' || !/^[a-zA-Z0-9_-]+$/.test(provider)) throw new Error('Invalid provider route');
  let pools = registry.get(ctx.root);
  if (!pools) registry.set(ctx.root, pools = new Map());
  const requested = new Admission(config);
  let pool = pools.get(provider);
  if (pool && ['maxActive', 'maxQueued', 'waitMs'].some(k => pool[k] !== requested[k])) {
    throw new Error('Restart Harness to change admission limits');
  }
  if (!pool) pools.set(provider, pool = requested);
  ctx.provide('uart2llmAdmission', pool);
  ctx.on('llm/stream', function (options, next) {
    if (options.provider !== provider) return next();
    return pool.stream(options, next);
  }, {prepend: true});
}
