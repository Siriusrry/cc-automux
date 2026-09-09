/* CC AutoMux Web UI — management key storage and sign-in. */
(function () {
  'use strict';
  const CCAM = (window.CCAM = window.CCAM || {});
  const KEY = 'cc-automux.management-key';
  const listeners = [];
  let memoryKey = null;

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
    memoryKey = null;
    try { sessionStorage.removeItem(KEY); localStorage.removeItem(KEY); } catch (e) { /* ignore */ }
  }
  function emit() { listeners.forEach(fn => fn(!!key())); }

  async function login(candidate, remember) {
    const previous = memoryKey;
    memoryKey = candidate;
    try {
      const status = await CCAM.api.get('/api/v1/status', { silentUnauthorized: true });
      persist(candidate, remember);
      emit();
      return status;
    } catch (e) {
      memoryKey = previous;
      throw e;
    }
  }
  function logout() { forget(); emit(); }

  // Hot rotation: adopt a new management key the client itself just saved.
  function adopt(newKey) { persist(newKey, isRemembered()); }

  CCAM.auth = { key, isRemembered, login, logout, adopt, onChange(fn) { listeners.push(fn); } };
  CCAM.api.setKeyProvider(key);
})();
