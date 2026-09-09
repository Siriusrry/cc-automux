/* CC AutoMux Web UI — authenticated JSON requests and log streams. */
(function () {
  'use strict';
  const CCAM = (window.CCAM = window.CCAM || {});

  class ApiError extends Error {
    constructor(status, code, message) {
      super(message || code || ('HTTP ' + status));
      this.name = 'ApiError';
      this.status = status;
      this.code = code || (status === 0 ? 'network' : 'http_' + status);
      const split = CCAM.ui ? CCAM.ui.splitFieldError(message) : { field: null, message };
      this.field = split.field;
      this.detail = split.message;
    }
    get isNetwork() { return this.status === 0; }
  }

  let keyProvider = () => null;
  let quietUnauthorized = 0;
  const unauthorizedListeners = [];
  function authHeaders() {
    const key = keyProvider();
    return key ? { Authorization: 'Bearer ' + key } : {};
  }

  async function transport(method, path, body, opts) {
    const init = { method, cache: 'no-store', headers: Object.assign({ Accept: 'application/json' }, authHeaders(), opts && opts.headers), signal: opts && opts.signal };
    if (body !== undefined) { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(body); }
    let res;
    try { res = await fetch(path, init); } catch (e) { throw new ApiError(0, 'network', 'Could not reach CC AutoMux'); }
    let json = null;
    const text = await res.text();
    if (text) { try { json = JSON.parse(text); } catch (e) { throw new ApiError(res.status >= 400 ? res.status : 502, 'invalid_response', 'The service returned an invalid JSON response.'); } }
    return { status: res.status, json, headers: res.headers };
  }

  async function request(method, path, body, opts) {
    const requestKey = keyProvider();
    const res = await transport(method, path, body, opts);
    if (res.status === 401) {
      if (!quietUnauthorized && !(opts && opts.silentUnauthorized) && requestKey === keyProvider()) unauthorizedListeners.forEach(fn => fn());
      throw new ApiError(401, 'unauthorized', 'That key was not accepted');
    }
    if (res.status >= 400) {
      const err = (res.json && res.json.error) || ('http_' + res.status);
      throw new ApiError(res.status, err, (res.json && res.json.message) || err);
    }
    if (res.status === 204) return null;
    if (opts && opts.response) return res;
    return res.json;
  }

  // Server-sent events over fetch so the Authorization header can be sent.
  function stream(path, handlers) {
    handlers = handlers || {};
    const requestKey = keyProvider();
    let resolveReady, rejectReady;
    const ready = new Promise((resolve, reject) => { resolveReady = resolve; rejectReady = reject; });
    ready.catch(() => {});
    const controller = new AbortController();
    let closed = false;
    const fail = (error) => { rejectReady(error); if (!closed && handlers.onClose) handlers.onClose(error); };
    const openTimer = setTimeout(() => { if (!closed) { fail(new ApiError(0, 'network', 'Stream connection timed out')); closed = true; controller.abort(); } }, 10000);
    (async () => {
      let res;
      try {
        res = await fetch(path, { headers: Object.assign({ Accept: 'text/event-stream' }, authHeaders()), signal: controller.signal });
      } catch (e) { clearTimeout(openTimer); fail(new ApiError(0, 'network', 'Stream connection failed')); return; }
      clearTimeout(openTimer);
      if (closed) return;
      if (res.status === 401) { if (!quietUnauthorized && requestKey === keyProvider()) unauthorizedListeners.forEach(fn => fn()); fail(new ApiError(401, 'unauthorized', 'Unauthorized')); return; }
      if (res.status >= 400) {
        let json = null; try { json = await res.json(); } catch (e) { /* ignore */ }
        fail(new ApiError(res.status, json && json.error, json && json.message));
        return;
      }
      if (!res.body || !(res.headers.get('Content-Type') || '').startsWith('text/event-stream')) { fail(new ApiError(502, 'invalid_response', 'The service did not return a log stream.')); return; }
      resolveReady();
      handlers.onOpen && handlers.onOpen();
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';
      let event = 'message', id = null, data = [];
      const dispatch = () => {
        if (data.length) {
          const payload = data.join('\n');
          if (event === 'record') { try { handlers.onRecord && handlers.onRecord(JSON.parse(payload), id); } catch (e) { /* malformed line: ignore */ } }
          else if (event === 'dropped') { try { handlers.onDropped && handlers.onDropped(JSON.parse(payload).dropped || 0); } catch (e) { /* ignore */ } }
        }
        event = 'message'; id = null; data = [];
      };
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let idx;
          while ((idx = buffer.indexOf('\n')) >= 0) {
            let line = buffer.slice(0, idx); buffer = buffer.slice(idx + 1);
            if (line.endsWith('\r')) line = line.slice(0, -1);
            if (line === '') { dispatch(); continue; }
            if (line.startsWith(':')) continue;
            const colon = line.indexOf(':');
            const name = colon < 0 ? line : line.slice(0, colon);
            let value2 = colon < 0 ? '' : line.slice(colon + 1);
            if (value2.startsWith(' ')) value2 = value2.slice(1);
            if (name === 'event') event = value2; else if (name === 'id') id = value2; else if (name === 'data') data.push(value2);
          }
        }
      } catch (e) { /* aborted or network drop */ }
      if (!closed) handlers.onClose && handlers.onClose(null);
    })();
    return { ready, close() { closed = true; clearTimeout(openTimer); rejectReady(new ApiError(0, 'aborted', 'Stream closed')); controller.abort(); } };
  }

  // Serialize writes so two saves never race each other.
  let writeChain = Promise.resolve();
  function enqueue(run) {
    const p = writeChain.then(run, run);
    writeChain = p.catch(() => {});
    return p;
  }

  CCAM.api = {
    ApiError,
    pauseUnauthorized() { quietUnauthorized++; let resumed = false; return () => { if (!resumed) { resumed = true; quietUnauthorized--; } }; },
    setKeyProvider(fn) { keyProvider = fn; },
    onUnauthorized(fn) { unauthorizedListeners.push(fn); },
    request, stream, enqueue,
    write: (method, path, body, opts) => enqueue(() => request(method, path, body, opts)),
    get: (path, opts) => request('GET', path, undefined, opts),
    post: (path, body) => enqueue(() => request('POST', path, body)),
    put: (path, body) => enqueue(() => request('PUT', path, body)),
    del: (path) => enqueue(() => request('DELETE', path))
  };
})();
