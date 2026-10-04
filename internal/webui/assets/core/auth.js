/* CC AutoMux Web UI — management key storage and sign-in. */
(function () {
  'use strict';
  const CCAM = (window.CCAM = window.CCAM || {});
  const KEY = 'cc-automux.management-key';
  const PENDING = KEY + '.pending';
  const listeners = [];
  let memoryKey = null;
  let epoch = 0, recovering = null;
  let candidates = [];
  try {
    const saved = JSON.parse(sessionStorage.getItem(PENDING) || '[]');
    if (Array.isArray(saved)) candidates = saved.filter(value => typeof value === 'string' && value);
  } catch (e) { /* storage unavailable */ }

  function stored() {
    try { return sessionStorage.getItem(KEY) || localStorage.getItem(KEY) || null; } catch (e) { return null; }
  }
  function key() { return memoryKey || stored(); }
  function isRemembered() { try { return !!localStorage.getItem(KEY); } catch (e) { return false; } }

  function persist(value, remember) {
    memoryKey = value;
    try {
      sessionStorage.setItem(KEY, value);
      if (remember) localStorage.setItem(KEY, value); else localStorage.removeItem(KEY);
    } catch (e) { /* storage unavailable: memory only */ }
  }
  function forget() {
    epoch++; memoryKey = null; candidates = []; recovering = null; saveCandidates();
    try { sessionStorage.removeItem(KEY); localStorage.removeItem(KEY); } catch (e) { /* ignore */ }
  }
  function emit() { listeners.forEach(fn => fn(!!key())); }

  async function login(candidate, remember) {
    const previous = memoryKey;
    memoryKey = candidate;
    try {
      const status = await CCAM.api.get('/api/v1/status', { silentUnauthorized: true });
      persist(candidate, remember);
      epoch++; candidates = []; saveCandidates();
      emit();
      return status;
    } catch (e) {
      memoryKey = previous;
      throw e;
    }
  }
  function logout() { forget(); emit(); }
  function sessionGuard() { const current = epoch; return () => epoch === current; }

  function saveCandidates() {
    try {
      if (candidates.length) sessionStorage.setItem(PENDING, JSON.stringify(candidates));
      else sessionStorage.removeItem(PENDING);
    } catch (e) { /* storage unavailable: memory only */ }
  }
  // Retain only submitted credentials, separately from editable page drafts.
  // An unanswered write may finish after its caller's deadline or navigation.
  function retain(newKey) {
    if (newKey === key() || candidates.includes(newKey)) return () => {};
    candidates.push(newKey); saveCandidates();
    const submittedEpoch = epoch;
    return () => {
      if (epoch !== submittedEpoch) return;
      candidates = candidates.filter(value => value !== newKey); saveCandidates();
    };
  }
  function adopt(newKey) {
    persist(newKey, isRemembered());
    const index = candidates.indexOf(newKey);
    if (index >= 0) candidates.splice(0, index + 1);
    saveCandidates();
  }
  async function recover(rejectedKey, options) {
    if (recovering) return recovering;
    const startedEpoch = epoch;
    const pending = (async () => {
      let unavailable;
      for (const candidate of candidates.slice().reverse()) {
        if (candidate === rejectedKey) continue;
        try {
          await CCAM.api.get('/api/v1/status', { ...options, timeoutMs: options?.timeoutMs ?? 500, silentUnauthorized: true, headers: { Authorization: 'Bearer ' + candidate } });
          if (startedEpoch !== epoch || options?.signal?.aborted) return false;
          adopt(candidate); return true;
        } catch (error) {
          if (error.status !== 401) unavailable = error;
        }
      }
      // Rejection of the old key alone is not grounds to destroy a session.
      if (unavailable) throw unavailable;
      return false;
    })();
    recovering = pending;
    try { return await pending; } finally { if (recovering === pending) recovering = null; }
  }

  CCAM.auth = { key, isRemembered, login, logout, adopt, retain, sessionGuard, onChange(fn) { listeners.push(fn); } };
  CCAM.api.setKeyProvider(key);
  CCAM.api.setAuthRecovery(recover);
})();
