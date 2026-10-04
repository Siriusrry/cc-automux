/* CC AutoMux Web UI — shared state: status polling, cached resources, foreground refresh. */
(function () {
  'use strict';
  const CCAM = (window.CCAM = window.CCAM || {});
  const api = CCAM.api;

  const listeners = {};
  function on(name, fn) { (listeners[name] = listeners[name] || []).push(fn); return () => off(name, fn); }
  function off(name, fn) { const l = listeners[name]; if (!l) return; const i = l.indexOf(fn); if (i >= 0) l.splice(i, 1); }
  function emit(name, value) { (listeners[name] || []).slice().forEach(fn => { try { fn(value); } catch (e) { console.error(e); } }); }

  const state = { status: null, connection: 'idle', statusError: null, failures: 0 };
  let pollTimer = null;
  let polling = false;
  let pollEpoch = 0;
  const POLL_MS = 5000;

  async function refreshStatus() {
    const epoch = pollEpoch;
    try {
      const status = await api.get('/api/v1/status');
      if (epoch !== pollEpoch) return null;
      const previous = state.status;
      if (previous && (previous.revision !== status.revision || previous.start_time !== status.start_time)) invalidate();
      state.status = status; state.statusError = null; state.failures = 0;
      if (state.connection !== 'live') { state.connection = 'live'; emit('connection', 'live'); }
      emit('status', status);
      return status;
    } catch (e) {
      if (e.status === 401 || epoch !== pollEpoch) return null;
      state.failures++; state.statusError = e;
      if (state.connection !== 'reconnecting') { state.connection = 'reconnecting'; emit('connection', 'reconnecting'); }
      return null;
    }
  }
  function schedule() {
    clearTimeout(pollTimer);
    if (!polling) return;
    if (document.visibilityState !== 'visible') return;
    pollTimer = setTimeout(async () => { await refreshStatus(); schedule(); }, POLL_MS);
  }
  function startPolling() { if (polling) return; polling = true; refreshStatus().then(schedule); }
  function stopPolling() { polling = false; pollEpoch++; clearTimeout(pollTimer); }

  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') { if (polling) refreshStatus().then(schedule); emit('foreground'); }
    else clearTimeout(pollTimer);
  });
  window.addEventListener('focus', () => emit('foreground'));

  // Cached resources. Any successful write invalidates everything derived from configuration.
  const cache = {};
  function cached(name, loader) {
    if (!cache[name]) {
      const pending = loader().catch(e => { if (cache[name] === pending) delete cache[name]; throw e; });
      cache[name] = pending;
    }
    return cache[name];
  }
  function invalidate() { for (const k of Object.keys(cache)) delete cache[k]; emit('invalidate'); }

  const copy = value => value === undefined ? undefined : JSON.parse(JSON.stringify(value));
  function same(a, b) {
    if (a === b) return true;
    if (!a || !b || typeof a !== 'object' || typeof b !== 'object') return false;
    const ka = Object.keys(a), kb = Object.keys(b);
    return ka.length === kb.length && ka.every(k => Object.prototype.hasOwnProperty.call(b, k) && same(a[k], b[k]));
  }
  // Three-way merge only locally changed fields. Lists are atomic; a changed
  // list must never silently replace a concurrent edit of that same list.
  function mergeDraft(baseline, draft, latest, path) {
    if (same(baseline, draft)) return copy(latest);
    if (same(latest, baseline) || same(latest, draft)) return copy(draft);
    if (baseline && draft && latest && [baseline, draft, latest].every(v => typeof v === 'object' && !Array.isArray(v))) {
      const out = copy(latest);
      new Set([...Object.keys(baseline), ...Object.keys(draft)]).forEach(k => {
        const value = mergeDraft(baseline[k], draft[k], latest[k], path ? path + '.' + k : k);
        if (value === undefined) delete out[k]; else out[k] = value;
      });
      return out;
    }
    throw new api.ApiError(412, 'configuration_changed', 'Another window changed ' + (path || 'this configuration') + '. Your draft is kept; use Revert to load the latest values.');
  }
  const uncertain = error => error.isNetwork || error.code === 'invalid_response';
  const SAVE_TIMEOUT_MS = 2000, REQUEST_TIMEOUT_MS = 500, MAX_ATTEMPTS = 3;
  function saveFailure(submitted) {
    return submitted
      ? new api.ApiError(503, 'save_unconfirmed', 'Save not confirmed. The changes may already be applied. Your input is kept; saving again will reconcile the previous submission.')
      : new api.ApiError(503, 'save_unavailable', 'Save failed before submission. Your input is kept; try saving again.');
  }
  // The deadline starts at submission, including time waiting in the queue.
  // Aborted queued work never starts; expired active work releases the queue.
  function saveTask(run) {
    const controller = new AbortController(), deadline = Date.now() + SAVE_TIMEOUT_MS;
    let submitted = false;
    const failure = () => saveFailure(submitted);
    let expire;
    const expired = new Promise((_, reject) => { expire = () => { controller.abort(); reject(failure()); }; });
    const timer = setTimeout(expire, SAVE_TIMEOUT_MS);
    const task = {
      failure,
      submitted() { submitted = true; },
      check() { if (controller.signal.aborted || Date.now() >= deadline) { expire(); throw failure(); } },
      options() { task.check(); return { signal: controller.signal, timeoutMs: Math.min(REQUEST_TIMEOUT_MS, deadline - Date.now()) }; },
      wait(ms) {
        task.check();
        return new Promise((resolve, reject) => {
          const aborted = () => { clearTimeout(waitTimer); reject(failure()); };
          const waitTimer = setTimeout(() => { controller.signal.removeEventListener('abort', aborted); resolve(); }, ms);
          controller.signal.addEventListener('abort', aborted, { once: true });
        });
      }
    };
    const queued = api.enqueue(() => {
      task.check();
      return Promise.race([run(task), expired]);
    });
    return Promise.race([queued, expired]).finally(() => clearTimeout(timer));
  }
  async function retryRead(read, task) {
    for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt++) {
      if (attempt) await task.wait(attempt * 250);
      try { const value = await read(task.options()); task.check(); return value; }
      catch (error) { task.check(); if (!uncertain(error)) throw error; }
    }
    throw task.failure();
  }
  const configResponse = options => api.get('/api/v1/config', { ...options, response: true });
  function configTag(response) {
    const tag = response.headers.get('ETag');
    if (!tag || !/^"[^"\s]+"$/.test(tag)) throw new api.ApiError(502, 'invalid_response', 'The service did not provide a configuration validator.');
    return tag;
  }
  // Each retry carries the original strong precondition and immutable intent.
  // A read may race an in-flight write; the mutation lock still allows only one
  // state change. A different version is never used to replay an old intent.
  async function commit({ method, path, body, tag, matches, recovered, read = configResponse, onDisconnect, onSubmit, onRejected }, task) {
    let recovering = false;
    for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt++) {
      if (attempt) await task.wait(attempt * 100);
      const options = task.options();
      if (!attempt && onSubmit) onSubmit();
      task.submitted();
      try {
        const value = await api.request(method, path, body, { ...options, headers: { 'If-Match': tag } });
        task.check(); return value;
      }
      catch (error) {
        task.check();
        if (!uncertain(error) && !recovering) { if (onRejected) onRejected(); throw error; }
        recovering = true;
      }
      for (let check = 0; check < MAX_ATTEMPTS; check++) {
        if (check) await task.wait(check * 250);
        let response;
        try { response = await read(task.options()); task.check(); }
        catch (error) {
          task.check();
          if (onDisconnect) { const result = await onDisconnect(task.options()); task.check(); if (result) return result; }
          if (uncertain(error)) { if (check + 1 === MAX_ATTEMPTS) throw task.failure(); continue; }
          throw error;
        }
        if (matches(response.json)) { const value = await recovered(response.json, task); task.check(); return value; }
        if (configTag(response) !== tag) throw new api.ApiError(412, 'configuration_changed', 'Another window changed these settings. Your draft is kept; review or revert before saving again.');
        break;
      }
    }
    throw task.failure();
  }
  function saveConfig(baseline, draft, onApplied, recovery) {
    const before = store.clientUpdate(baseline), wanted = copy(draft);
    return saveTask(async task => {
      const response = await retryRead(configResponse, task);
      const tag = configTag(response);
      const body = mergeDraft(before, wanted, store.clientUpdate(response.json), '');
      const result = await commit({ method: 'PUT', path: '/api/v1/config', body, tag,
        matches: value => same(store.clientUpdate(value), body), recovered: () => ({ applied: true }),
        ...(recovery ? recovery(body) : {}) }, task);
      if (onApplied) await onApplied(result, body, task);
      task.check();
      invalidate();
      return result;
    });
  }
  function contains(value, wanted) {
    if (same(value, wanted)) return true;
    return value && wanted && !Array.isArray(wanted) && typeof wanted === 'object' &&
      Object.keys(wanted).every(key => contains(value[key], wanted[key]));
  }
  function write(method, path, input, options) {
    const body = copy(input);
    const provider = path.match(/^\/api\/v1\/providers(?:\/((?!order$)[^/]+))?$/);
    const profile = path.match(/^\/api\/v1\/harnesses\/claude-code\/profiles(?:\/([^/]+))?(\/activate)?$/);
    const harness = path === '/api/v1/harnesses/claude-code';
    const order = path === '/api/v1/providers/order';
    const creating = method === 'POST' && ((provider && !provider[1]) || (profile && !profile[1]));
    if (creating) body.id ||= CCAM.fmt.uuid();
    if (!provider && !profile && !harness && !order) throw new Error('Unsupported configuration resource');
    const id = provider?.[1] || profile?.[1] || body?.id;
    const select = cfg => order ? (cfg.providers || []).filter(p => body.provider_ids.includes(p.id)).map(p => p.id)
      : harness || profile?.[2] ? cfg.harnesses.claude_code
      : (provider ? cfg.providers || [] : cfg.harnesses.claude_code.profiles || []).find(value => value.id === id);
    const matches = cfg => method === 'DELETE' ? !select(cfg) : order ? same(select(cfg), body.provider_ids)
      : profile?.[2] ? select(cfg).active_profile_id === id : contains(select(cfg), body);
    const recovered = async (cfg, task) => {
      if (method === 'DELETE') return null;
      if (harness) return retryRead(options => store.harness(options), task);
      if (profile) {
        const status = await retryRead(options => store.harness(options), task);
        const active = status.state === 'in_sync' && status.active_profile_id === id;
        if (profile[2] && !active) throw new api.ApiError(409, 'activation_changed', 'The settings file no longer matches this profile. Review its current state before activating again.');
        const value = Object.assign({}, cfg.harnesses.claude_code.profiles.find(p => p.id === id), { active });
        return profile[2] ? { active, harness: status, profile: value } : value;
      }
      return order ? { applied: true } : select(cfg);
    };
    return saveTask(async task => {
      const response = await retryRead(configResponse, task);
      const tag = options?.headers?.['If-Match'] || configTag(response);
      // A user may retry a timed-out creation with the same form identity.
      const result = creating && matches(response.json) ? await recovered(response.json, task)
        : await commit({ method, path, body, tag, matches, recovered }, task);
      task.check();
      invalidate();
      return result;
    });
  }

  const store = {
    on, off, emit, state,
    startPolling, stopPolling, refreshStatus,
    invalidate, same, mergeDraft, saveConfig, write,
    config: () => cached('config', () => api.get('/api/v1/config')),
    providers: () => cached('providers', async () => (await api.get('/api/v1/providers')) || []),
    providerHealth: async () => {
      const response = await api.get('/api/v1/provider-health', { response: true });
      const data = response.json;
      data.configETag = response.headers.get('Config-ETag');
      data.providers = (data.providers || []).map(p => Object.assign({}, p, { models: p.models || [], patches: p.patches || [], channels: p.channels || [], sessions: p.sessions || [] }));
      return data;
    },
    patches: () => cached('patches', () => api.get('/api/v1/provider-patches')),
    harness: options => api.get('/api/v1/harnesses/claude-code', options),
    profiles: () => api.get('/api/v1/harnesses/claude-code/profiles'),
    // Build the client update object from a complete config resource.
    clientUpdate(config) {
      const cc = config.harnesses.claude_code;
      return {
        schema_version: config.schema_version,
        service: Object.assign({}, config.service),
        auth: Object.assign({}, config.auth),
        auto_mode: JSON.parse(JSON.stringify(config.auto_mode)),
        harnesses: { claude_code: { path_mode: cc.path_mode, settings_path: cc.settings_path, disable_telemetry: cc.disable_telemetry, profiles: JSON.parse(JSON.stringify(cc.profiles || [])) } },
        providers: JSON.parse(JSON.stringify(config.providers || []))
      };
    },
    reset() { stopPolling(); invalidate(); state.status = null; state.connection = 'idle'; }
  };
  CCAM.store = store;
})();
