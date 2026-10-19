// message is the summary; a technical detail from the server, when present,
// follows on the next lines so ErrorNotice can show it behind "查看详情".
export class APIError extends Error {
  constructor(message, status, code, detail = '') {
    super(detail ? message + '\n' + detail : message);
    this.name = 'APIError';
    this.status = status;
    this.code = code;
    this.summary = message;
    this.detail = detail;
  }
}

// Bounded fetches, with the caller's cancellation propagated to the request.
// Authentication remains in the same-origin cookie; secrets are never stored
// in localStorage or appended to download URLs.
export function api(path, options = {}) {
  return call(path, options, (response) => response.json().catch(() => null));
}

// apiText is api for an endpoint that answers with plain text: the body and
// the response headers. Errors still arrive as JSON.
export function apiText(path, options = {}) {
  return call(path, options, async (response) => ({ text: await response.text(), headers: response.headers }));
}

async function call(path, options, read) {
  const { signal, timeoutMs = 20_000, ...init } = options;
  const controller = new AbortController();
  const abort = () => controller.abort(signal?.reason);
  if (signal?.aborted) abort();
  else signal?.addEventListener('abort', abort, { once: true });
  let timedOut = false;
  const timeout = setTimeout(() => { timedOut = true; controller.abort(); }, Math.min(55_000, Math.max(1_000, timeoutMs)));
  try {
    const response = await fetch(path, { ...init, credentials: 'same-origin', signal: controller.signal });
    if (!response.ok) {
      const body = await response.json().catch(() => null);
      if (response.status === 401 && !path.startsWith('/api/v1/auth/')) window.dispatchEvent(new CustomEvent('nasctl:unauthorized'));
      throw new APIError(body?.message || `请求失败（${response.status}）`, response.status, body?.error, [body?.detail, body?.error && `错误代码：${body.error} · HTTP ${response.status}`].filter(Boolean).join('\n'));
    }
    return await read(response);
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
