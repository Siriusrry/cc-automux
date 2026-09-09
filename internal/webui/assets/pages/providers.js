/* Providers: tiered list, detail editor, health diagnostics. */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  const { h, icon, replace, clear, pill, healthPill, healthDot, tag, field, input, secretInput, seg, switchCtl, chipEditor, kv, empty, skeleton, errorCard, savebar, toast, confirm, tip, tipBlock, healthSummary, channelSummary, priorityTip } = CCAM.ui;
  const fmt = CCAM.fmt;
  const store = CCAM.store;
  const api = CCAM.api;
  CCAM.pages = CCAM.pages || {};

  const STATIC_TEXT = { disabled_provider: 'Disabled', no_models: 'No models' };

  function tiersOf(list) {
    const sorted = list.slice().sort((a, b) => b.priority - a.priority);
    const tiers = [];
    sorted.forEach(p => { let t = tiers.find(x => x.priority === p.priority); if (!t) { t = { priority: p.priority, items: [] }; tiers.push(t); } t.items.push(p); });
    return tiers;
  }

  // ---------- list ----------
  CCAM.pages.providers = {
    render(ctx) {
      ctx.title('Providers');
      ctx.actions([h('a', { class: 'btn primary', href: '#/providers/new' }, icon('plus'), 'Add provider')]);
      const host = h('div', null, skeleton(4, 68));
      ctx.root.appendChild(host);
      let disposed = false;
      let patchIndex = {};

      async function load() {
        try {
          const [data, patchList] = await Promise.all([store.providerHealth(), store.patches().catch(() => [])]);
          if (disposed) return;
          patchIndex = {};
          (Array.isArray(patchList) ? patchList : (patchList && patchList.patches) || []).forEach(x => { patchIndex[x.id] = x; });
          render(data.providers);
        } catch (e) { if (!disposed) replace(host, errorCard(e.detail || e.message, load)); }
      }
      function render(list) {
        ctx.subtitle([h('b', null, fmt.plural(list.length, 'provider')), ' · upstreams that speak the Anthropic Messages API, scheduled by priority']);
        if (!list.length) {
          replace(host, h('div', { class: 'card' }, empty({ title: 'No providers yet', text: 'Add an upstream that accepts the Anthropic Messages API directly. CC AutoMux appends /v1/messages to its base URL and does no protocol conversion for normal traffic.', action: h('a', { class: 'btn primary', href: '#/providers/new' }, icon('plus'), 'Add provider') })));
          return;
        }
        const tiers = tiersOf(list);
        const out = tiers.map((t, i) => {
          const hint = tiers.length === 1 ? 'new sessions round-robin across this tier' : i === 0 ? 'tried first · round-robin within the tier' : 'used only while every higher tier is unavailable';
          return h('section', { class: 'pr-tier' },
            h('div', { class: 'pr-tier-head' }, h('p', { class: 'eyebrow' }, 'Priority ' + fmt.priorityLabel(t.priority)), h('span', { class: 'hint' }, hint)),
            h('div', { class: 'rows' }, t.items.map(row)));
        });
        replace(host, out);
      }
      function row(p) {
        const stat = p.static_availability;
        const state = p.global_health ? p.global_health.state : 'unknown';
        const n = p.models.length;
        const modelsCell = n
          ? tip(tag(fmt.plural(n, 'model')), () => tipBlock({ title: 'Declared models', list: p.models.slice(), note: 'Requests naming one of these models can be routed here.' }))
          : tip(h('span', { class: 'faint', style: { fontSize: '12px' } }, 'no models declared'), 'Declare at least one model — a provider without models is never routed to.');
        const sw = switchCtl({ checked: p.enabled, tip: p.enabled ? 'Enabled — in rotation. Turn off to stop routing here without deleting the provider.' : 'Disabled — receives no traffic. Turn on to put it back in rotation.', onchange: async (on, inp) => {
          inp.disabled = true;
          try {
            const next = Object.assign({}, p, { enabled: on });
            delete next.static_availability; delete next.global_health; delete next.channels; delete next.sessions; delete next.active_session_count;
            await api.put('/api/v1/providers/' + p.id, next);
            toast(on ? p.name + ' enabled' : p.name + ' disabled');
            store.invalidate(); load();
          } catch (e) { inp.checked = !on; inp.disabled = false; toast('Could not update ' + p.name + ': ' + (e.detail || e.message), 'bad'); }
        } });
        sw.setAttribute('aria-label', 'Enabled');
        let healthCell;
        if (stat === 'active') {
          const suffix = state === 'cooldown' && p.global_health.cooldown_until ? 'retry ' + fmt.untilShort(p.global_health.cooldown_until) : null;
          healthCell = healthPill(state, suffix);
        } else healthCell = pill(STATIC_TEXT[stat], stat === 'no_models' ? 'warn' : 'mist', 'plain');
        tip(healthCell, healthSummary(p), { live: state === 'cooldown' });
        const patchNames = p.patches.map(id => patchIndex[id] ? patchIndex[id].name : id);
        const patchesCell = tip(h('span', { class: 'm' }, icon('patch'), String(p.patches.length)), () => tipBlock({ title: 'Provider patches · ' + p.patches.length, list: patchNames,
          note: patchNames.length ? 'Request rewrites applied to every request sent here, in this order.' : 'No patches — requests are forwarded exactly as received.' }));
        const sessionsCell = tip(h('span', { class: 'm' }, icon('sessions'), String(p.active_session_count)), () => tipBlock({ title: 'Sticky sessions · ' + p.active_session_count,
          note: (p.active_session_count ? 'Sessions currently pinned to this provider. ' : 'No session is pinned to this provider right now. ') + 'A session keeps its provider for one hour after its last request, so a conversation stays on one upstream.' }));
        const prioCell = tip(h('span', { class: 'prio' }, 'P ' + fmt.priorityLabel(p.priority)), priorityTip(p.priority));
        // Hover targets sit above the row-wide link, so a click on them navigates explicitly.
        return h('div', { class: 'row-card pr-row' + (stat !== 'active' ? ' dim' : ''), onclick: (e) => { if (e.target.closest('a, button, label, input')) return; location.hash = '#/providers/' + p.id; } },
          healthDot(stat === 'active' ? state : 'unknown'),
          h('div', { class: 'rc-main' },
            h('div', { class: 'rc-title' }, h('a', { class: 'rc-link', href: '#/providers/' + p.id }, p.name),
              p.disable_health ? tip(pill('health off', 'ghost', 'plain'), 'Health tracking is disabled for this provider: it is always considered available and never enters cooldown.') : null),
            h('div', { class: 'rc-sub' }, fmt.host(p.base_url))),
          h('div', { class: 'rc-tags' }, modelsCell),
          h('div', { class: 'rc-meta' }, prioCell, patchesCell, sessionsCell, h('span', { class: 'hs' }, healthCell)),
          sw,
          icon('chevron-right', 'chev'));
      }
      load();
      const off = store.on('invalidate', load);
      const tick = setInterval(() => { if (!disposed) load(); }, 15000);
      return () => { disposed = true; off(); clearInterval(tick); };
    }
  };

  // ---------- detail / new ----------
  function blankProvider() {
    return { id: '', name: '', base_url: '', api_key: '', models: [], priority: 0, enabled: true, use_x_api_key: false, tls: { ca_file: '', insecure_skip_verify: false }, patches: [], disable_health: false };
  }
  function stripHealth(p) {
    const out = Object.assign({}, p);
    ['static_availability', 'global_health', 'channels', 'sessions', 'active_session_count'].forEach(k => delete out[k]);
    return out;
  }

  CCAM.pages.provider = {
    render(ctx) {
      const isNew = ctx.route.name === 'provider-new';
      const id = ctx.params.id;
      ctx.title(isNew ? 'New provider' : 'Provider');
      ctx.actions([h('a', { class: 'btn quiet', href: '#/providers' }, icon('arrow-left'), 'All providers')]);
      const host = h('div', null, skeleton(6, 44));
      ctx.root.appendChild(host);
      let bar = null, disposed = false;

      async function load() {
        try {
          const [patches, health] = await Promise.all([store.patches(), isNew ? Promise.resolve(null) : store.providerHealth()]);
          if (disposed) return;
          let provider = null, diag = null;
          if (!isNew) {
            diag = health.providers.find(p => p.id === id);
            if (!diag) { replace(host, h('div', { class: 'card' }, empty({ title: 'Provider not found', text: 'It may have been deleted in another window.', action: h('a', { class: 'btn', href: '#/providers' }, 'Back to providers') }))); return; }
            provider = stripHealth(diag);
          }
          render(provider || blankProvider(), patches, diag);
        } catch (e) { if (!disposed) replace(host, errorCard(e.detail || e.message, load)); }
      }

      function render(provider, patches, diag) {
        const draft = JSON.parse(JSON.stringify(provider));
        const baseline = JSON.stringify(draft);
        const fields = {};
        if (!isNew) { ctx.title(provider.name); ctx.subtitle([h('span', { class: 'mono' }, fmt.host(provider.base_url)), ' · priority ', h('b', null, fmt.priorityLabel(provider.priority))]); }
        else ctx.subtitle('An upstream that accepts the Anthropic Messages API directly.');

        if (bar) bar.destroy();
        bar = savebar({ onSave: save, onRevert: () => { if (isNew) CCAM.router.go('/providers'); else load(); } });
        const dirty = () => JSON.stringify(draft) !== baseline;
        const check = () => { bar.show(isNew ? true : dirty()); Object.values(fields).forEach(f => f.setError && f.setError('')); bar.setError(''); };
        CCAM.router.setGuard(async () => { if (!dirty() && !isNew) return true; if (isNew && JSON.stringify(draft) === JSON.stringify(blankProvider())) return true; return confirm({ title: 'Discard changes?', text: 'This provider has unsaved changes.', confirmLabel: 'Discard', danger: true }); });

        // --- form ---
        fields.name = field({ label: 'Name', for: 'p-name', control: input({ id: 'p-name', sans: true, value: draft.name, placeholder: 'e.g. AnyRouter', oninput: (e) => { draft.name = e.target.value; check(); } }), help: 'Shown in logs and health diagnostics. Must be unique.' });
        fields.base_url = field({ label: 'Base URL', for: 'p-url', control: input({ id: 'p-url', value: draft.base_url, placeholder: 'https://provider.example', oninput: (e) => { draft.base_url = e.target.value; check(); } }),
          help: ['CC AutoMux appends ', h('code', null, '/v1/messages'), ' and keeps the client query string. The provider must accept the ', h('b', null, 'Anthropic Messages API'), ' directly — normal requests are never converted.'] });
        const keyWrap = secretInput({ id: 'p-key', value: draft.api_key, placeholder: 'provider API key', copy: true, oninput: (e) => { draft.api_key = e.target.value; check(); } });
        fields.api_key = field({ label: 'API key', for: 'p-key', control: keyWrap, help: 'Sent to the provider on every request. Required when the provider is enabled and declares models.' });
        const authSeg = CCAM.ui.authHeaderSeg({ useXApiKey: draft.use_x_api_key, onchange: (v) => { draft.use_x_api_key = v; check(); } });
        fields.use_x_api_key = field({ label: 'Authentication header', control: authSeg, help: 'Most Anthropic-compatible gateways accept a Bearer token; the official API style uses x-api-key.' });
        const models = chipEditor({ values: draft.models, placeholder: draft.models.length ? 'add model…' : 'claude-sonnet-4-5, claude-haiku-4-5, …', onchange: (v) => { draft.models = v; check(); } });
        fields.models = field({ label: 'Models', for: 'p-models', control: models, help: 'Exact, case-sensitive model names as Claude Code sends them. A request is routed only to providers declaring its model.' });
        fields.priority = field({ label: 'Priority', for: 'p-prio', control: input({ id: 'p-prio', type: 'number', value: String(draft.priority), attrs: { step: '1' }, oninput: (e) => { draft.priority = Number(e.target.value) || 0; check(); } }), help: 'Higher tiers are tried first. Providers with the same priority share new sessions round-robin.' });
        const enabledSw = switchCtl({ checked: draft.enabled, label: 'Enabled', onchange: (v) => { draft.enabled = v; check(); } });
        const healthSw = switchCtl({ checked: !draft.disable_health, label: 'Health cooldown', sub: 'failures cool the provider down and back off', onchange: (v) => { draft.disable_health = !v; check(); } });
        const tls = CCAM.ui.tlsControl({ idPrefix: 'p', value: draft.tls, onchange: (v) => { draft.tls = v; check(); } });
        fields.tls = field({ label: 'TLS', control: tls });
        const patchSel = CCAM.ui.patchSelector({ patches, selected: draft.patches, onchange: (ids) => { draft.patches = ids; check(); } });
        fields.patches = field({ label: 'Preset patches', control: patchSel, help: 'Project-maintained compatibility fixes, applied only to this provider and only for the request types they declare. Nothing is inferred from the URL or model name.' });

        const formCard = h('div', { class: 'card pr-form' },
          h('p', { class: 'eyebrow' }, 'Upstream'),
          fields.name, fields.base_url, h('div', { class: 'row-2' }, fields.api_key, fields.use_x_api_key),
          h('hr', { class: 'hr' }),
          h('p', { class: 'eyebrow' }, 'Scheduling'),
          fields.models, h('div', { class: 'row-2' }, fields.priority, h('div', { class: 'field' }, h('span', { class: 'lab' }, 'State'), h('div', { class: 'pr-inline' }, enabledSw, healthSw))),
          h('hr', { class: 'hr' }),
          h('p', { class: 'eyebrow' }, 'Transport'),
          fields.tls,
          h('hr', { class: 'hr' }),
          h('p', { class: 'eyebrow' }, 'Patches'),
          fields.patches);

        // --- right column ---
        let right = null;
        if (!isNew) {
          const g = diag.global_health;
          const stat = diag.static_availability;
          const healthCard = h('div', { class: 'card' },
            h('div', { class: 'card-head' }, h('p', { class: 'eyebrow' }, 'Health'), stat !== 'active' ? pill(STATIC_TEXT[stat], stat === 'no_models' ? 'warn' : 'mist', 'plain') : null),
            h('div', { class: 'hl-diag' }, tip(healthPill(g.state, g.state === 'cooldown' && g.cooldown_until ? 'retry ' + fmt.untilShort(g.cooldown_until) : null), healthSummary(diag), { live: g.state === 'cooldown' }), h('span', { class: 'hl-sub' }, g.state === 'disabled' ? 'Health cooldown is off: results are recorded but never suppress scheduling.' : g.state === 'cooldown' ? 'Not scheduled until the cooldown ends; the first request afterwards probes it.' : g.state === 'half_open' ? 'One live request is probing this provider.' : g.state === 'degraded' ? 'Recent failures below the cooldown threshold.' : g.state === 'unknown' ? 'No request has reached this provider yet.' : 'Serving normally.')),
            kv([
              ['Backoff level', String(g.backoff_level)],
              ['Consecutive failures', String(g.consecutive_failures)],
              ['Observed failures', String(g.observed_failures)],
              ['Cooldown until', g.cooldown_until ? fmt.dateTime(g.cooldown_until) : null],
              ['Last success', g.last_success_at ? fmt.dateTime(g.last_success_at) + ' · ' + fmt.relative(g.last_success_at) : null],
              ['Last failure', g.last_failure_at ? fmt.dateTime(g.last_failure_at) + ' · ' + fmt.relative(g.last_failure_at) : null],
              ['Last upstream URL', g.last_upstream_url ? h('span', { class: 'mono' }, g.last_upstream_url) : null],
              ['Last session', g.last_session_id ? h('span', { class: 'sid' }, g.last_session_id) : null]
            ], { compact: true }),
            g.last_error ? [h('p', { class: 'lab', style: { margin: '14px 0 6px', fontSize: '12.5px', fontWeight: '600' } }, 'Last error'), h('pre', { class: 'code wrap clamp', 'data-tip': 'Click to expand', onclick: (e) => e.currentTarget.classList.toggle('clamp') }, g.last_error)] : null);

          const channels = diag.channels.length ? h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl compact' }, h('thead', null, h('tr', null, h('th', null, 'Model'), h('th', null, 'Type'), h('th', null, 'State'), h('th', null, 'Fails'), h('th', null, 'Retry'))),
            h('tbody', null, diag.channels.map(c => h('tr', null, h('td', { class: 'mono wrap-any' }, c.model), h('td', null, tag(c.request_type, c.request_type === 'classifier' ? 'iris' : '')), h('td', null, tip(healthPill(c.state), channelSummary(c), { live: c.state === 'cooldown' })), h('td', { class: 'num', 'data-tip': 'consecutive / observed failures' }, c.consecutive_failures + ' / ' + c.observed_failures), h('td', { class: 'nowrap' }, c.cooldown_until ? fmt.untilShort(c.cooldown_until) : h('span', { class: 'faint' }, '—'))))))) : h('p', { class: 'lede' }, 'No model channel has been used yet.');
          const channelCard = h('div', { class: 'card' }, h('p', { class: 'eyebrow' }, 'Model channels'), h('p', { class: 'lede' }, 'Health is tracked per model and request type in addition to the provider-wide state above.'), channels);

          const sessions = diag.sessions.length ? h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl compact' }, h('thead', null, h('tr', null, h('th', null, 'Session'), h('th', null, 'Model'), h('th', null, 'Type'), h('th', null, 'Last used'))),
            h('tbody', null, diag.sessions.map(s => h('tr', null, h('td', { class: 'sid nowrap', 'data-tip': s.session_id }, fmt.middle(s.session_id, 13)), h('td', { class: 'mono wrap-any' }, s.model), h('td', null, tag(s.request_type, s.request_type === 'classifier' ? 'iris' : '')), h('td', { class: 'nowrap' }, fmt.relative(s.last_used_at))))))) : h('p', { class: 'lede', style: { margin: 0 } }, 'No session is currently pinned to this provider.');
          const sessionCard = h('div', { class: 'card' }, h('div', { class: 'card-head' }, h('p', { class: 'eyebrow' }, 'Sticky sessions'), pill(String(diag.active_session_count), 'iris', 'plain')), sessions);

          const dangerCard = h('div', { class: 'card danger-card' }, h('p', { class: 'eyebrow mist' }, 'Remove'), h('p', { class: 'lede' }, 'Deleting stops routing to this provider immediately. Sessions pinned to it are released and re-scheduled.'),
            h('button', { class: 'btn danger', type: 'button', onclick: async () => {
              const ok = await confirm({ title: 'Delete ' + provider.name + '?', text: 'The provider and its patches are removed from the configuration. Health history is discarded.', confirmLabel: 'Delete provider', danger: true });
              if (!ok) return;
              try { await api.del('/api/v1/providers/' + id); toast(provider.name + ' deleted'); store.invalidate(); CCAM.router.clearGuard(); CCAM.router.go('/providers'); }
              catch (e) { toast('Delete failed: ' + (e.detail || e.message), 'bad'); }
            } }, icon('trash'), 'Delete provider'));
          right = h('div', { class: 'stack' }, healthCard, channelCard, sessionCard, dangerCard);
        }
        replace(host, isNew ? h('div', { class: 'grid-2' }, formCard, h('div', { class: 'stack' }, h('div', { class: 'note' }, h('b', null, 'What a provider is.'), ' One base URL, one key, the models it serves, a priority tier and an on/off switch. Special upstream quirks are handled by opt-in patches, not by provider types.'),
          h('div', { class: 'note' }, h('b', null, 'Scheduling.'), ' Requests for a model go to the highest-priority tier that has a healthy provider declaring that model. Equal priorities round-robin; a Claude Code session sticks to one provider until it cools down.'))) : h('div', { class: 'grid-2' }, formCard, right));
        check();

        async function save() {
          const body = JSON.parse(JSON.stringify(draft));
          if (!isNew) body.id = id; else delete body.id;
          bar.busy(true);
          try {
            const saved = isNew ? await api.post('/api/v1/providers', body) : await api.put('/api/v1/providers/' + id, body);
            bar.busy(false);
            toast(isNew ? saved.name + ' added' : 'Saved ' + saved.name);
            store.invalidate();
            CCAM.router.clearGuard();
            if (isNew) CCAM.router.go('/providers/' + saved.id); else load();
          } catch (e) {
            bar.busy(false);
            if (e.status === 409 && /name/.test(e.message)) { fields.name.setError('A provider with this name already exists.'); fields.name.querySelector('input').focus(); return; }
            if (e.field) {
              const key = e.field.replace(/\[\d+\]$/, '').replace(/^tls\..*/, 'tls');
              const f = fields[key];
              if (f && f.setError) { f.setError(e.detail); const inp = f.querySelector('input, select'); if (inp) inp.focus(); return; }
            }
            bar.setError(e.status === 409 && e.code === 'restart_in_progress' ? 'CC AutoMux is restarting — try again in a moment.' : (e.detail || e.message));
          }
        }
      }
      load();
      return () => { disposed = true; if (bar) bar.destroy(); };
    }
  };
})();
