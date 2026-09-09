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
  function saveConfig(baseline, draft, onApplied) {
    const before = store.clientUpdate(baseline), wanted = copy(draft);
    return api.enqueue(async () => {
      const response = await api.get('/api/v1/config', { response: true });
      const tag = response.headers.get('ETag');
      if (!tag) throw new api.ApiError(502, 'invalid_response', 'The service did not provide a configuration validator.');
      const body = mergeDraft(before, wanted, store.clientUpdate(response.json), '');
      const result = await api.request('PUT', '/api/v1/config', body, { headers: { 'If-Match': tag } });
      if (onApplied) await onApplied(result, body);
      invalidate();
      return result;
    });
  }

  const store = {
    on, off, emit, state,
    startPolling, stopPolling, refreshStatus,
    invalidate, same, mergeDraft, saveConfig,
    config: () => cached('config', () => api.get('/api/v1/config')),
    providers: () => cached('providers', async () => (await api.get('/api/v1/providers')) || []),
    providerHealth: async () => {
      const data = await api.get('/api/v1/provider-health');
      data.providers = (data.providers || []).map(p => Object.assign({}, p, { models: p.models || [], patches: p.patches || [], channels: p.channels || [], sessions: p.sessions || [] }));
      return data;
    },
    patches: () => cached('patches', () => api.get('/api/v1/provider-patches')),
    harness: () => api.get('/api/v1/harnesses/claude-code'),
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
