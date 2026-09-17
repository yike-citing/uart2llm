export class APIError extends Error { constructor(public status: number, message: string) { super(message); } }
// Keep connection checks compatible with browsers that have AbortController
// but do not yet implement AbortSignal.any() or AbortSignal.timeout().
export async function withDeadline<T>(action: (signal: AbortSignal) => Promise<T>, parent?: AbortSignal, milliseconds = 10000): Promise<T> {
  const controller = new AbortController();
  let rejectAbort!: (error: Error) => void;
  const aborted = new Promise<never>((_, reject) => {rejectAbort = reject;});
  const cancel = (timeout = false) => {
    controller.abort();
    const error = new Error(timeout ? '连接本机管理服务超时，请重试。' : '连接已取消。');
    error.name = timeout ? 'TimeoutError' : 'AbortError';
    rejectAbort(error);
  };
  const onAbort = () => cancel();
  const timer = setTimeout(() => cancel(true), milliseconds);
  parent?.addEventListener('abort', onAbort, {once:true});
  try {
    if (parent?.aborted) cancel();
    return await Promise.race([aborted, parent?.aborted ? aborted : action(controller.signal)]);
  } finally {
    clearTimeout(timer);
    parent?.removeEventListener('abort', onAbort);
  }
}
let sessionCreation: Promise<unknown> | undefined;
let sessionGeneration = 0;
async function createSession(): Promise<unknown> {
  if (!sessionCreation) {
    sessionCreation = withDeadline(signal => send('/session', 'POST', {}, signal))
      .then(value => { sessionGeneration++; return value; })
      .finally(() => { sessionCreation = undefined; });
  }
  return sessionCreation;
}
async function send(path: string, method: string, body?: unknown, signal?: AbortSignal): Promise<unknown> {
  const raw = body instanceof File || body instanceof Blob;
  const response = await fetch('/admin/v1' + path, {method, credentials: 'same-origin', signal,
    headers: body === undefined ? {} : {'Content-Type': raw ? 'application/octet-stream' : 'application/json'},
    body: body === undefined ? undefined : raw ? body : JSON.stringify(body)});
  const text = await response.text();
  let data: unknown = text;
  try { data = text ? JSON.parse(text) : null; } catch { if (response.ok) throw new APIError(502, '管理服务返回了无效数据。请确认正在访问代理后台，而不是静态文件服务器。'); }
  if (!response.ok) throw new APIError(response.status, typeof data === 'string' ? data : JSON.stringify(data));
  return data;
}
export async function request(path: string, method = 'GET', body?: unknown, signal?: AbortSignal): Promise<unknown> {
  const generation = sessionGeneration;
  try { return await send(path, method, body, signal); }
  catch (error) {
    // A 401 is an explicit refusal before an operation is executed. Never
    // replay transport failures or other responses, especially for mutations.
    if (!(error instanceof APIError) || error.status !== 401 || path === '/session') throw error;
    if (generation === sessionGeneration) await createSession();
    return send(path, method, body, signal);
  }
}
export function parsePatch(text: string): Record<string, unknown> {
  const patch: unknown = JSON.parse(text);
  if (!patch || Array.isArray(patch) || typeof patch !== 'object') throw new Error('配置必须为 JSON 对象。');
  const object = patch as Record<string, unknown>;
  if (Object.keys(object).some(k => k !== 'host' && k !== 'device')) throw new Error('顶层字段只允许 host 和 device。');
  if ('host' in object && 'device' in object) throw new Error('电脑配置和设备配置具有独立事务，请分别提交。');
  for (const value of Object.values(object)) if (!value || Array.isArray(value) || typeof value !== 'object') throw new Error('host 和 device 必须为配置对象。');
  return object;
}
