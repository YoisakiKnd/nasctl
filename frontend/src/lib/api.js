export class APIError extends Error {
  constructor(message, status, code) {
    super(message);
    this.name = 'APIError';
    this.status = status;
    this.code = code;
  }
}

// Bounded fetches, with the caller's cancellation propagated to the request.
// Authentication remains in the same-origin cookie; secrets are never stored
// in localStorage or appended to download URLs.
export async function api(path, options = {}) {
  const { signal, timeoutMs = 20_000, ...init } = options;
  const controller = new AbortController();
  const abort = () => controller.abort(signal?.reason);
  if (signal?.aborted) abort();
  else signal?.addEventListener('abort', abort, { once: true });
  let timedOut = false;
  const timeout = setTimeout(() => { timedOut = true; controller.abort(); }, Math.min(55_000, Math.max(1_000, timeoutMs)));
  try {
    const response = await fetch(path, { ...init, credentials: 'same-origin', signal: controller.signal });
    const body = await response.json().catch(() => null);
    if (!response.ok) {
      if (response.status === 401 && !path.startsWith('/api/v1/auth/')) window.dispatchEvent(new CustomEvent('nasctl:unauthorized'));
      throw new APIError(body?.message || `请求失败（${response.status}）`, response.status, body?.error);
    }
    return body;
  } catch (error) {
    if (timedOut) throw new APIError('请求超时，请检查连接后重试', 408, 'timeout');
    throw error;
  } finally {
    clearTimeout(timeout);
    signal?.removeEventListener('abort', abort);
  }
}

export function json(method, body, options = {}) {
  return { ...options, method, headers: { 'Content-Type': 'application/json', ...options.headers }, body: JSON.stringify(body) };
}

export function query(path, params) {
  return path + '?' + new URLSearchParams(Object.entries(params).filter(([, value]) => value !== undefined && value !== null));
}
