/* CC AutoMux Web UI — application shell: theme, sidebar, page head, routing. */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  const { h, icon, mascot, replace, clear } = CCAM.ui;
  const fmt = CCAM.fmt;

  const VERSION = (document.querySelector('meta[name="cc-automux-version"]') || {}).content || '';
  const NAV = [
    { name: 'overview', path: '/', label: 'Overview', icon: 'overview' },
    { name: 'providers', path: '/providers', label: 'Providers', icon: 'providers', match: ['providers', 'provider', 'provider-new'] },
    { name: 'auto-mode', path: '/auto-mode', label: 'Auto Mode', icon: 'auto' },
    { name: 'claude-code', path: '/claude-code', label: 'Claude Code', icon: 'claude' },
    { name: 'logs', path: '/logs', label: 'Logs', icon: 'logs' },
    { name: 'service', path: '/service', label: 'Service', icon: 'service' }
  ];

  // ---- theme ----
  const THEME_KEY = 'cc-automux.theme';
  const media = window.matchMedia('(prefers-color-scheme: dark)');
  function themePref() { try { return localStorage.getItem(THEME_KEY) || 'auto'; } catch (e) { return 'auto'; } }
  function applyTheme() {
    const pref = themePref();
    const effective = pref === 'auto' ? (media.matches ? 'dark' : 'light') : pref;
    document.documentElement.setAttribute('data-theme', effective);
  }
  function setThemePref(v) { try { localStorage.setItem(THEME_KEY, v); } catch (e) { /* ignore */ } applyTheme(); }
  media.addEventListener ? media.addEventListener('change', applyTheme) : media.addListener(applyTheme);
  applyTheme();

  const root = document.getElementById('app');
  let shell = null;      // { navLinks, pageHost, titleEl, subEl, actionsEl, chips }
  let currentCleanup = null;
  let uptimeTimer = null;
  let lastStatusAt = 0;

  function renderLogin(reason) {
    teardownPage();
    shell = null;
    clear(root);
    document.body.classList.add('login-mode');
    CCAM.pages.login.render({ root, reason, version: VERSION, onSuccess: () => { document.body.classList.remove('login-mode'); boot(); } });
  }

  function buildShell() {
    clear(root);
    const navLinks = NAV.map(n => h('a', { href: '#' + n.path, dataset: { name: n.name } }, icon(n.icon), h('span', null, n.label)));
    const themeSegs = [];
    const makeThemeSeg = () => {
      const s = CCAM.ui.seg({ class: 'sm icons quiet', ariaLabel: 'Theme', value: themePref(), options: [
        { value: 'auto', icon: 'monitor', title: 'Follow system' }, { value: 'light', icon: 'sun', title: 'Light' }, { value: 'dark', icon: 'moon', title: 'Dark' }
      ], onchange: (v) => { setThemePref(v); themeSegs.forEach(o => o.setValue(v)); } });
      themeSegs.push(s);
      return s;
    };
    const signOutHandler = () => { CCAM.store.reset(); CCAM.auth.logout(); };
    const signOut = h('button', { class: 'btn quiet sm', type: 'button', onclick: signOutHandler }, icon('logout'), 'Sign out');
    const side = h('aside', { class: 'side' },
      h('div', { class: 'brand' }, mascot(), h('div', null, h('div', { class: 'name' }, 'CC AutoMux'), h('div', { class: 'ver' }, VERSION)),
        h('div', { class: 'brand-tools' }, makeThemeSeg(), h('button', { class: 'ib bordered', type: 'button', 'aria-label': 'Sign out', 'data-tip': 'Sign out', onclick: signOutHandler }, icon('logout')))),
      h('nav', { class: 'nav', 'aria-label': 'Sections' }, navLinks),
      h('div', { class: 'side-foot' }, h('div', { class: 'foot-row' }, makeThemeSeg(), signOut), h('div', { class: 'side-note' }, 'Loopback only · 127.0.0.1'))
    );
    const titleEl = h('h1', null, '');
    const subEl = h('div', { class: 'sub' }, '');
    const chips = h('div', { class: 'status-chips' });
    const actionsEl = h('div', { class: 'page-actions' });
    const pageHost = h('div', { id: 'page' });
    const main = h('main', { class: 'main' }, h('div', { class: 'main-in' },
      h('div', { class: 'page-head' }, h('div', null, titleEl, subEl), h('div', { class: 'actions' }, chips, actionsEl)),
      pageHost));
    root.appendChild(h('div', { class: 'app' }, side, main));
    shell = { navLinks, pageHost, titleEl, subEl, actionsEl, chips };
    renderChips();
  }

  function renderChips() {
    if (!shell) return;
    const st = CCAM.store.state;
    const status = st.status;
    const live = st.connection === 'live';
    const liveChip = h('span', { class: 'chip ' + (live ? 'live' : 'off') }, h('span', { class: 'dot' }),
      live ? [h('span', null, 'Live'), status ? h('span', { class: 'mono muted' }, '· ' + status.listen_addr) : null] : 'Reconnecting…');
    CCAM.ui.tip(liveChip, () => live
      ? CCAM.ui.tipBlock({ title: 'Connected', rows: [['Management API', status ? status.listen_addr : '—', 'mono'], ['Status poll', 'every 5 s while this tab is visible'], ['Last update', lastStatusAt ? fmt.clock(new Date(lastStatusAt).toISOString()) : '—']] })
      : CCAM.ui.tipBlock({ title: 'Reconnecting', note: 'The last status request failed. Retrying every 5 s; values on screen may be stale.' }), { live: true });
    const up = status ? h('span', { class: 'chip', 'data-tip': 'Started ' + fmt.dateTime(status.start_time) + '\nConfiguration revision ' + status.revision }, icon('clock'), 'uptime ', h('b', { class: 'num', dataset: { uptime: '1' } }, fmt.duration(status.uptime_seconds))) : null;
    replace(shell.chips, liveChip, up);
  }
  function tickUptime() {
    if (!shell) return;
    const status = CCAM.store.state.status;
    const el = shell.chips.querySelector('[data-uptime]');
    if (!el || !status || !status.start_time) return;
    el.textContent = fmt.duration((Date.now() - new Date(status.start_time).getTime()) / 1000);
  }

  function teardownPage() {
    CCAM.ui.closeOverlays();
    if (typeof currentCleanup === 'function') { try { currentCleanup(); } catch (e) { console.error(e); } }
    currentCleanup = null;
    CCAM.router.clearGuard();
    document.querySelectorAll('.savebar').forEach(el => el.remove());
  }

  function route(r) {
    if (!shell) return;
    if (r.name === 'login') { CCAM.router.replace('/'); return; }
    teardownPage();
    const navName = NAV.find(n => n.name === r.name || (n.match || []).includes(r.name));
    shell.navLinks.forEach(a => { if (navName && a.dataset.name === navName.name) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current'); });
    clear(shell.pageHost); clear(shell.actionsEl);
    shell.titleEl.textContent = ''; shell.subEl.textContent = '';
    window.scrollTo({ top: 0 });
    let page = CCAM.pages[r.name];
    if (r.name === 'provider' || r.name === 'provider-new') page = CCAM.pages.provider;
    if (!page) { shell.titleEl.textContent = 'Not found'; shell.pageHost.appendChild(CCAM.ui.empty({ title: 'Nothing here', text: 'That address does not match a page.', action: h('a', { class: 'btn', href: '#/' }, 'Back to overview') })); return; }
    const ctx = {
      root: shell.pageHost, params: r.params, query: r.query, route: r,
      title(t) { shell.titleEl.textContent = t; document.title = t + ' · CC AutoMux'; },
      subtitle(nodes) { replace(shell.subEl, nodes); },
      actions(nodes) { replace(shell.actionsEl, nodes); }
    };
    const result = page.render(ctx);
    currentCleanup = typeof result === 'function' ? result : (result && result.destroy) || null;
  }

  function boot() {
    const bootstrap = CCAM.host.bootstrapKey();
    if (bootstrap && !CCAM.auth.key()) CCAM.auth.adopt(bootstrap);
    if (!CCAM.auth.key()) { renderLogin(); return; }
    document.body.classList.remove('login-mode');
    buildShell();
    CCAM.store.startPolling();
    clearInterval(uptimeTimer); uptimeTimer = setInterval(tickUptime, 1000);
    CCAM.router.start(route);
  }

  CCAM.store.on('status', () => { lastStatusAt = Date.now(); renderChips(); });
  CCAM.store.on('connection', renderChips);
  CCAM.api.onUnauthorized(() => { CCAM.store.reset(); CCAM.auth.logout(); });
  CCAM.auth.onChange((signedIn) => { if (!signedIn) { clearInterval(uptimeTimer); renderLogin('Signed out. Your management key may have been rotated.'); } });

  CCAM.app = { boot, NAV, version: VERSION, setThemePref, themePref };
  boot();
})();
