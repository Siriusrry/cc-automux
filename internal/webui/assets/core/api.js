/* CC AutoMux Web UI — authenticated JSON requests and log streams. */
(function () {
  'use strict';
  const CCAM = (window.CCAM = window.CCAM || {});

  class ApiError extends Error {
    constructor(status, code, message, field) {
      super(message || code || ('HTTP ' + status));
      this.name = 'ApiError';
      this.status = status;
      this.code = code || (status === 0 ? 'network' : 'http_' + status);
      const split = CCAM.ui ? CCAM.ui.splitFieldError(message) : { field: null, message };
      this.field = field || split.field;
      this.detail = split.message;
    }
    get isNetwork() { return this.status === 0; }
  }

  let keyProvider = () => null;
  let sessionGuardProvider = () => () => true;
  let authRecovery = null;
  let quietUnauthorized = 0;
  const unauthorizedListeners = [];
  function authHeaders() {
    const key = keyProvider();
    return key ? { Authorization: 'Bearer ' + key } : {};
  }
  function captureSession() {
    const sameSession = sessionGuardProvider();
    return () => {
      if (!sameSession()) throw new ApiError(401, 'session_changed', 'The sign-in session changed. This operation was stopped.');
    };
  }
  async function recoverUnauthorized(requestKey, opts) {
    if (opts?.silentUnauthorized || opts?.headers?.Authorization) return false;
    if (requestKey !== keyProvider()) return !!keyProvider();
    return authRecovery ? authRecovery(requestKey, opts) : false;
  }
  function notifyUnauthorized(requestKey, opts) {
    if (!quietUnauthorized && !opts?.silentUnauthorized && !opts?.headers?.Authorization && requestKey === keyProvider()) unauthorizedListeners.forEach(fn => fn());
  }

  async function transport(method, path, body, opts) {
    const init = { method, cache: 'no-store', headers: Object.assign({ Accept: 'application/json' }, authHeaders(), opts && opts.headers), signal: opts && opts.signal };
    if (body !== undefined) { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(body); }
    const controller = new AbortController();
    const abort = () => controller.abort();
    if (opts?.signal?.aborted) controller.abort();
    opts?.signal?.addEventListener('abort', abort, { once: true });
    init.signal = controller.signal;
    const timer = setTimeout(abort, opts?.timeoutMs ?? 10000);
    let res, text;
    try {
      if (controller.signal.aborted) throw new Error('Request aborted');
      res = await fetch(path, init); text = await res.text();
      if (controller.signal.aborted) throw new Error('Request aborted');
    }
    catch (e) { throw new ApiError(0, 'network', 'Could not reach CC AutoMux'); }
    finally { clearTimeout(timer); opts?.signal?.removeEventListener('abort', abort); }
    let json = null;
    if (text) { try { json = JSON.parse(text); } catch (e) { throw new ApiError(res.status >= 400 ? res.status : 502, 'invalid_response', 'The service returned an invalid JSON response.'); } }

    return { status: res.status, json, headers: res.headers };
  }

  async function request(method, path, body, opts) {
    const checkSession = captureSession();
    let requestKey = keyProvider();
    let res;
    try { res = await transport(method, path, body, opts); }
    finally { checkSession(); }
    // A 401 rejected the operation before execution. Retry it once only after
    // authentication has resolved a credential transition owned by this tab.
    if (res.status === 401) {
      let recovered;
      try { recovered = await recoverUnauthorized(requestKey, opts); }
      finally { checkSession(); }
      if (recovered) {
        requestKey = keyProvider();
        try { res = await transport(method, path, body, opts); }
        finally { checkSession(); }
      }
    }
    if (res.status === 401) {
      notifyUnauthorized(requestKey, opts);
      throw new ApiError(401, 'unauthorized', 'That key was not accepted');
    }
    if (res.status >= 400) {
      const err = (res.json && res.json.error) || ('http_' + res.status);
      throw new ApiError(res.status, err, (res.json && res.json.message) || err, res.json && res.json.field);
    }
    if (res.status === 204) return null;
    if (res.json === null) throw new ApiError(502, 'invalid_response', 'The service returned an empty JSON response.');
    if (opts && opts.response) return res;
    return res.json;
  }

  // Server-sent events over fetch so the Authorization header can be sent.
  function stream(path, handlers) {
    handlers = handlers || {};
    const checkSession = captureSession();
    let requestKey = keyProvider();
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
        checkSession();
        res = await fetch(path, { headers: Object.assign({ Accept: 'text/event-stream' }, authHeaders()), signal: controller.signal });
        checkSession();
        if (res.status === 401) {
          let recovered;
          try { recovered = await recoverUnauthorized(requestKey, { signal: controller.signal }); }
          finally { checkSession(); }
          if (closed) return;
          if (recovered) {
            requestKey = keyProvider();
            res = await fetch(path, { headers: Object.assign({ Accept: 'text/event-stream' }, authHeaders()), signal: controller.signal });
            checkSession();
          }
        }
      } catch (e) { clearTimeout(openTimer); controller.abort(); fail(e.code === 'session_changed' ? e : new ApiError(0, 'network', 'Stream connection failed')); return; }
      clearTimeout(openTimer);
      if (closed) return;
      if (res.status === 401) { notifyUnauthorized(requestKey); fail(new ApiError(401, 'unauthorized', 'Unauthorized')); return; }
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
        checkSession();
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
          checkSession();
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
      finally { controller.abort(); reader.releaseLock(); }
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
    setSessionGuardProvider(fn) { sessionGuardProvider = fn; },
    setAuthRecovery(fn) { authRecovery = fn; },
    onUnauthorized(fn) { unauthorizedListeners.push(fn); },
    captureSession, request, stream, enqueue,
    get: (path, opts) => request('GET', path, undefined, opts)
  };
})();
