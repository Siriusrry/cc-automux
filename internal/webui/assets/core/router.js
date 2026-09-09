/* CC AutoMux Web UI — hash router with a leave guard for unsaved changes. */
(function () {
  'use strict';
  const CCAM = (window.CCAM = window.CCAM || {});

  const ROUTES = [
    { name: 'overview', pattern: /^\/?$/ },
    { name: 'login', pattern: /^\/login$/ },
    { name: 'providers', pattern: /^\/providers$/ },
    { name: 'provider-new', pattern: /^\/providers\/new$/ },
    { name: 'provider', pattern: /^\/providers\/([^/]+)$/, params: ['id'] },
    { name: 'auto-mode', pattern: /^\/auto-mode$/ },
    { name: 'claude-code', pattern: /^\/claude-code$/ },
    { name: 'logs', pattern: /^\/logs$/ },
    { name: 'service', pattern: /^\/service$/ }
  ];

  function parse(hash) {
    let path = (hash || '#/').replace(/^#/, '');
    if (!path.startsWith('/')) path = '/' + path;
    const q = path.indexOf('?');
    let query = {};
    if (q >= 0) { new URLSearchParams(path.slice(q + 1)).forEach((v, k) => { query[k] = v; }); path = path.slice(0, q); }
    for (const r of ROUTES) {
      const m = r.pattern.exec(path);
      if (m) {
        const params = {};
        (r.params || []).forEach((p, i) => { params[p] = decodeURIComponent(m[i + 1]); });
        return { name: r.name, params, query, path };
      }
    }
    return { name: 'not-found', params: {}, query, path };
  }

  let guard = null;      // () => Promise<boolean> | boolean; true = may leave
  let current = parse(location.hash);
  let handler = null;
  let ignoreNext = false;

  async function onHashChange() {
    if (ignoreNext) { ignoreNext = false; return; }
    const next = parse(location.hash);
    if (guard && next.path !== current.path) {
      const ok = await guard();
      if (!ok) { ignoreNext = true; location.hash = '#' + current.path; return; }
    }
    guard = null;
    current = next;
    if (handler) handler(current);
  }

  const router = {
    start(fn) { handler = fn; window.removeEventListener('hashchange', onHashChange); window.addEventListener('hashchange', onHashChange); current = parse(location.hash); handler(current); },
    go(path) { location.hash = '#' + (path.startsWith('/') ? path : '/' + path); },
    replace(path) { ignoreNext = false; history.replaceState(null, '', '#' + path); current = parse(location.hash); if (handler) handler(current); },
    setGuard(fn) { guard = fn; },
    clearGuard() { guard = null; },
    get current() { return current; },
    parse
  };
  CCAM.router = router;
})();
