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

  function tiersOf(data) {
    const byID = new Map(data.providers.map(p => [p.id, p]));
    return data.tiers.map(tier => ({ priority: tier.priority, items: tier.provider_ids.map(id => byID.get(id)).filter(Boolean) }));
  }

  // ---------- list ----------
  CCAM.pages.providers = {
    render(ctx) {
      ctx.title('Providers');
      ctx.actions([h('a', { class: 'btn primary', href: '#/providers/new' }, icon('plus'), 'Add provider')]);
      const host = h('div', null, skeleton(4, 68));
      ctx.root.appendChild(host);
      let disposed = false, loadEpoch = 0, phase = 'idle', draggingID = null, snapshot = null, deferredData = null, sortingPriority = null;
      let patchIndex = {};
      const views = new Map(), groups = new Map(), busy = new Set();
      const listHost = h('div');
      const live = h('div', { class: 'sr-only', role: 'status', 'aria-live': 'polite' });
      const sortRetry = h('button', { class: 'btn', type: 'button', hidden: true }, 'Check provider order');
      replace(host, sortRetry, listHost, live);
      const order = CCAM.providerOrder({
        live, canStart: () => phase === 'idle' && busy.size === 0 && snapshot && !!snapshot.configETag,
        tier: id => { const tier = snapshot.tiers.find(t => t.provider_ids.includes(id)); return tier && { ids: tier.provider_ids.slice(), configETag: snapshot.configETag, rows: groups.get(tier.priority).rows }; },
        row: id => views.get(id).el, name: id => snapshot.providers.find(p => p.id === id).name,
        onState(id) {
          draggingID = id; phase = id === null ? 'idle' : 'dragging'; syncActions();
          if (id === null && deferredData && !disposed) { const data = deferredData; deferredData = null; render(data); }
        },
        onCommit: saveOrder
      });
      function syncActions() {
        for (const [id, view] of views) { view.sw.setBusy(phase !== 'idle' || busy.has(id)); view.handle.setAttribute('aria-disabled', String((phase !== 'idle' && !(phase === 'dragging' && id === draggingID)) || busy.size > 0)); }
        for (const [priority, group] of groups) group.hint.textContent = priority === sortingPriority && phase === 'saving' ? 'Saving provider order…' : priority === sortingPriority && phase === 'unconfirmed' ? 'Order not confirmed — check saved state' : group.caption;
      }
      async function saveOrder(ids, etag, focused) {
        sortingPriority = snapshot.tiers.find(t => t.provider_ids.includes(focused)).priority;
        phase = 'saving'; draggingID = null; loadEpoch++; deferredData = null; syncActions();
        const read = () => store.providerHealth();
        const matches = data => data.tiers.some(t => store.same(t.provider_ids, ids));
        const write = async () => {
          await api.write('PUT', '/api/v1/providers/order', { provider_ids: ids }, { headers: { 'If-Match': etag } });
          return read();
        };
        await completeOrder(() => store.confirmedWrite(write, read, matches), focused);
      }
      async function completeOrder(run, focused, confirmed = true) {
        sortRetry.disabled = true;
        try {
          const data = await run();
          if (disposed) return;
          phase = 'idle'; deferredData = null; sortRetry.hidden = true; loadEpoch++;
          render(data); live.textContent = confirmed ? 'Provider order saved.' : 'Current provider order loaded.'; toast(live.textContent);
          views.get(focused)?.handle.focus({ preventScroll: true }); store.invalidate();
        } catch (e) {
          if (disposed) return;
          if (e.verify) {
            phase = 'unconfirmed'; sortRetry.hidden = false; sortRetry.disabled = false;
            sortRetry.onclick = () => completeOrder(e.verify, focused); toast(e.detail, 'bad'); return;
          }
          try {
            const data = await store.providerHealth();
            if (disposed) return;
            phase = 'idle'; deferredData = null; sortRetry.hidden = true; loadEpoch++; render(data);
            views.get(focused)?.handle.focus({ preventScroll: true });
            toast(e.code === 'configuration_changed' ? 'Configuration changed. The latest order is shown; try dragging again.' : 'Could not save order: ' + (e.detail || e.message), 'bad');
          } catch (_) {
            phase = 'unconfirmed'; sortRetry.hidden = false; sortRetry.disabled = false;
            sortRetry.onclick = () => completeOrder(() => store.providerHealth(), focused, false);
            toast('Order is not confirmed. Check provider order before continuing.', 'bad');
          }
        } finally { if (!disposed) syncActions(); }
      }
      function place(parent, nodes) {
        nodes.forEach((node, i) => { if (parent.children[i] !== node) parent.insertBefore(node, parent.children[i] || null); });
        while (parent.children.length > nodes.length) parent.lastElementChild.remove();
      }
      async function load() {
        const epoch = ++loadEpoch;
        try {
          const [data, patchList] = await Promise.all([store.providerHealth(), store.patches().catch(() => [])]);
          if (disposed || epoch !== loadEpoch) return;
          patchIndex = {};
          (Array.isArray(patchList) ? patchList : (patchList && patchList.patches) || []).forEach(x => { patchIndex[x.id] = x; });
          if (phase === 'idle') render(data); else deferredData = data;
        } catch (e) { if (!disposed && epoch === loadEpoch) { if (!views.size) replace(listHost, errorCard(e.detail || e.message, load)); else toast('Could not refresh providers: ' + (e.detail || e.message), 'bad'); } }
      }
      function render(data) {
        snapshot = data; const list = data.providers;
        ctx.subtitle([h('b', null, fmt.plural(list.length, 'provider')), ' · upstreams that speak the Anthropic Messages API, scheduled by priority']);
        if (!list.length) {
          views.clear(); groups.clear();
          replace(listHost, h('div', { class: 'card' }, empty({ title: 'No providers yet', text: 'Add an upstream that accepts the Anthropic Messages API directly. CC AutoMux appends /v1/messages to its base URL and does no protocol conversion for normal traffic.', action: h('a', { class: 'btn primary', href: '#/providers/new' }, icon('plus'), 'Add provider') })));
          return;
        }
        const ids = new Set(list.map(p => p.id));
        for (const [id, view] of views) if (!ids.has(id)) { view.el.remove(); views.delete(id); }
        const tiers = tiersOf(data);
        const sections = tiers.map((t, i) => {
          let group = groups.get(t.priority);
          if (!group) {
            const rows = h('div', { class: 'rows' }), hint = h('span', { class: 'hint' });
            group = { rows, hint, el: h('section', { class: 'pr-tier' }, h('div', { class: 'pr-tier-head' }, h('p', { class: 'eyebrow' }, 'Priority ' + fmt.priorityLabel(t.priority)), hint), rows) };
            groups.set(t.priority, group);
          }
          group.caption = tiers.length === 1 ? 'new sessions round-robin across this tier' : i === 0 ? 'tried first · round-robin within the tier' : 'used only while every higher tier is unavailable';
          const nodes = t.items.map(p => {
            let view = views.get(p.id);
            if (!view) { view = row(p); views.set(p.id, view); }
            view.update(p, t.priority); view.handle.disabled = t.items.length < 2; return view.el;
          });
          order.reconcile(nodes, () => place(group.rows, nodes));
          return group.el;
        });
        for (const [priority] of groups) if (!tiers.some(t => t.priority === priority)) groups.delete(priority);
        place(listHost, sections); syncActions();
      }
      function row(initial) {
        let p = initial, verify = null;
        const dot = healthDot('unknown');
        const link = h('a', { class: 'rc-link', href: '#/providers/' + p.id });
        const sub = h('div', { class: 'rc-sub' });
        const tags = h('div', { class: 'rc-tags' }), meta = h('div', { class: 'rc-meta' });
        const retry = h('button', { class: 'btn sm', type: 'button', hidden: true, onclick: () => complete(verify) }, 'Check saved state');
        const sw = switchCtl({ checked: p.enabled, onchange: async on => {
          if (busy.has(p.id) || phase !== 'idle') return;
          busy.add(p.id); loadEpoch++; sw.setBusy(true); syncActions();
          const id = p.id;
          await complete(() => store.confirmedWrite(() => api.put('/api/v1/providers/' + id, Object.assign(stripHealth(p), { enabled: on })), async () => (await api.get('/api/v1/providers')).find(value => value.id === id), value => value && value.enabled === on));
        } });
        sw.input.setAttribute('aria-label', 'Enabled');
        const handle = h('button', { class: 'ib pr-drag', type: 'button', 'aria-pressed': 'false', 'aria-label': 'Reorder ' + p.name }, icon('grip'));
        order.bind(p.id, handle);
        const el = h('div', { class: 'row-card pr-row', dataset: { providerId: p.id }, onclick: e => { if (e.target.closest('a, button, label, input')) return; location.hash = '#/providers/' + p.id; } },
          handle, dot, h('div', { class: 'rc-main' }, h('div', { class: 'rc-title' }, link), sub), tags, meta, retry, sw, icon('chevron-right', 'chev'));
        async function complete(run) {
          if (disposed || !run) return;
          retry.disabled = true;
          try {
            const saved = await run();
            if (disposed) return;
            p = Object.assign({}, p, saved); sw.setChecked(p.enabled);
            toast(p.name + (p.enabled ? ' enabled' : ' disabled'));
          } catch (e) {
            if (disposed) return;
            if (e.verify) { verify = e.verify; retry.hidden = false; retry.disabled = false; toast(e.detail, 'bad'); return; }
            toast('Could not update ' + p.name + ': ' + (e.detail || e.message), 'bad');
            sw.setChecked(p.enabled);
          }
          busy.delete(p.id); verify = null; retry.hidden = true; sw.setBusy(false); store.invalidate();
        }
        function update(value, priority) {
          p = value; handle.setAttribute('aria-label', 'Reorder ' + p.name);
          const stat = p.static_availability, state = p.global_health ? p.global_health.state : 'unknown', n = p.models.length;
          el.classList.toggle('dim', stat !== 'active');
          const nextDot = healthDot(stat === 'active' ? state : 'unknown'); dot.className = nextDot.className;
          dot.setAttribute('data-tip', nextDot.getAttribute('data-tip'));
          link.textContent = p.name; sub.textContent = fmt.host(p.base_url);
          replace(tags, n ? tip(tag(fmt.plural(n, 'model')), () => tipBlock({ title: 'Declared models', list: p.models.slice(), note: 'Requests naming one of these models can be routed here.' }))
            : tip(h('span', { class: 'faint', style: { fontSize: '12px' } }, 'no models declared'), 'Declare at least one model — a provider without models is never routed to.'));
          const healthCell = stat === 'active' ? healthPill(state, state === 'cooldown' && p.global_health.cooldown_until ? 'retry ' + fmt.untilShort(p.global_health.cooldown_until) : null) : pill(STATIC_TEXT[stat], stat === 'no_models' ? 'warn' : 'mist', 'plain');
          tip(healthCell, healthSummary(p), { live: state === 'cooldown' });
          const patchNames = p.patches.map(id => patchIndex[id] ? patchIndex[id].name : id);
          replace(meta,
            tip(h('span', { class: 'prio' }, 'P ' + priority), priorityTip(priority)),
            tip(h('span', { class: 'm' }, icon('patch'), String(p.patches.length)), () => tipBlock({ title: 'Provider patches · ' + p.patches.length, list: patchNames, note: patchNames.length ? 'Request rewrites applied in this order.' : 'No patches — requests are forwarded exactly as received.' })),
            tip(h('span', { class: 'm' }, icon('sessions'), String(p.active_session_count)), () => tipBlock({ title: 'Sticky bindings · ' + p.active_session_count, note: 'A session keeps its provider for one hour after its last request.' })), h('span', { class: 'hs' }, healthCell));
          sw.setAttribute('data-tip', p.enabled ? 'Enabled — in rotation. Turn off to stop routing here without deleting the provider.' : 'Disabled — receives no traffic. Turn on to put it back in rotation.');
          if (!busy.has(p.id)) sw.setChecked(p.enabled);
          sw.setBusy(busy.has(p.id));
        }
        return { el, sw, handle, update };
      }
      load();
      const off = store.on('invalidate', load);
      const tick = setInterval(() => { if (!disposed) load(); }, 15000);
      return () => { disposed = true; order.destroy(); off(); clearInterval(tick); };
    }
  };

  // ---------- detail / new ----------
  function blankProvider() {
    return { id: '', name: '', base_url: '', api_key: '', models: [], priority: 0, enabled: true, use_x_api_key: false, tls: { mode: 'system', ca_file: '' }, patches: [], disable_health: false };
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
      const duplicateHelp = isNew ? null : h('span', { class: 'help', id: 'p-duplicate-help', role: 'status', hidden: true }, 'Save or revert changes before duplicating.');
      const duplicateButton = isNew ? null : h('button', { class: 'btn', type: 'button', 'aria-describedby': 'p-duplicate-help', onclick: duplicate }, icon('copy'), 'Duplicate');
      ctx.actions([h('a', { class: 'btn quiet', href: '#/providers' }, icon('arrow-left'), 'All providers')]);
      const host = h('div', null, skeleton(6, 44));
      ctx.root.appendChild(host);
      let bar = null, disposed = false, duplicating = false, draftDirty = false, saving = false;

      function syncDuplicate() {
        if (isNew || disposed) return;
        duplicateButton.disabled = saving || duplicating || draftDirty;
        duplicateHelp.hidden = !draftDirty;
      }

      async function duplicate() {
        if (disposed || isNew || saving || duplicating || draftDirty) return;
        duplicating = true;
        syncDuplicate();
        let posting = false;
        try {
          for (let attempt = 0; attempt < 2; attempt++) {
            const list = await api.get('/api/v1/providers');
            if (disposed || draftDirty) return;
            const source = list.find(p => p.id === id);
            if (!source) {
              toast('Could not duplicate provider: it no longer exists.', 'bad');
              store.invalidate();
              load();
              return;
            }
            const names = new Set(list.map(p => p.name));
            const base = source.name + ' copy';
            let name = base;
            for (let suffix = 2; names.has(name); suffix++) name = base + ' ' + suffix;
            const copy = JSON.parse(JSON.stringify(source));
            delete copy.id;
            copy.name = name;
            try {
              posting = true;
              const created = await api.post('/api/v1/providers', copy);
              posting = false;
              store.invalidate();
              if (!disposed) {
                toast('Created ' + created.name);
                CCAM.router.go('/providers/' + created.id);
              }
              return;
            } catch (e) {
              if (attempt === 0 && e.status === 409 && e.code === 'conflict' && e.field === 'name') {
                posting = false;
                continue;
              }
              throw e;
            }
          }
        } catch (e) {
          if (!disposed) {
            const uncertain = posting && (e.isNetwork || !e.status || e.code === 'invalid_response');
            toast(uncertain ? 'The copy result is uncertain. Refresh the provider list to check before trying again.'
              : 'Could not duplicate provider: ' + (e.detail || e.message), 'bad');
          }
        } finally {
          duplicating = false;
          syncDuplicate();
        }
      }

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
        const check = () => { draftDirty = dirty(); syncDuplicate(); bar.show(isNew ? true : draftDirty); Object.values(fields).forEach(f => f.setError && f.setError('')); bar.setError(''); };
        CCAM.router.setGuard(async () => { if (saving) return false; if (!dirty() && !isNew) return true; if (isNew && JSON.stringify(draft) === JSON.stringify(blankProvider())) return true; return confirm({ title: 'Discard changes?', text: 'This provider has unsaved changes.', confirmLabel: 'Discard', danger: true }); });

        // --- form ---
        fields.name = field({ label: 'Name', for: 'p-name', control: input({ id: 'p-name', sans: true, value: draft.name, oninput: (e) => { draft.name = e.target.value; check(); } }), help: 'Shown in logs and health diagnostics. Must be unique.' });
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
            g.last_error ? [h('p', { class: 'lab', style: { margin: '14px 0 6px', fontSize: '12.5px', fontWeight: '600' } }, 'Last error', ['pending', 'incomplete', 'truncated'].filter(flag => g['last_error_' + flag]).map(flag => tag(flag)), g.last_error_truncated ? h('span', { class: 'faint' }, ' · complete text is in the log') : null), h('pre', { class: 'code wrap clamp', 'data-tip': 'Click to expand', onclick: (e) => e.currentTarget.classList.toggle('clamp') }, g.last_error)] : null);

          const channels = diag.channels.length ? h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl compact' }, h('thead', null, h('tr', null, h('th', null, 'Model'), h('th', null, 'Type'), h('th', null, 'State'), h('th', null, 'Fails'), h('th', null, 'Retry'))),
            h('tbody', null, diag.channels.map(c => h('tr', null, h('td', { class: 'mono wrap-any' }, c.model), h('td', null, tag(c.request_type, c.request_type === 'classifier' ? 'iris' : '')), h('td', null, tip(healthPill(c.state), channelSummary(c), { live: c.state === 'cooldown' })), h('td', { class: 'num', 'data-tip': 'consecutive / observed failures' }, c.consecutive_failures + ' / ' + c.observed_failures), h('td', { class: 'nowrap' }, c.cooldown_until ? fmt.untilShort(c.cooldown_until) : h('span', { class: 'faint' }, '—'))))))) : h('p', { class: 'lede' }, 'No model channel has been used yet.');
          const channelCard = h('div', { class: 'card' }, h('p', { class: 'eyebrow' }, 'Model channels'), h('p', { class: 'lede' }, 'Health is tracked per model and request type in addition to the provider-wide state above.'), channels);

          const sessions = diag.sessions.length ? h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl compact' }, h('thead', null, h('tr', null, h('th', null, 'Session'), h('th', null, 'Model'), h('th', null, 'Type'), h('th', null, 'Last used'))),
            h('tbody', null, diag.sessions.map(s => h('tr', null, h('td', { class: 'sid nowrap', 'data-tip': s.session_id }, fmt.middle(s.session_id, 13)), h('td', { class: 'mono wrap-any' }, s.model), h('td', null, tag(s.request_type, s.request_type === 'classifier' ? 'iris' : '')), h('td', { class: 'nowrap' }, fmt.relative(s.last_used_at))))))) : h('p', { class: 'lede', style: { margin: 0 } }, 'No session is currently pinned to this provider.');
          const sessionCard = h('div', { class: 'card' }, h('div', { class: 'card-head' }, h('p', { class: 'eyebrow' }, 'Sticky bindings'), pill(String(diag.active_session_count), 'iris', 'plain')), sessions);

          const deleteButton = h('button', { class: 'btn danger', type: 'button', onclick: async () => {
              const ok = await confirm({ title: 'Delete ' + provider.name + '?', text: 'The provider and its patches are removed from the configuration. Health history is discarded.', confirmLabel: 'Delete provider', danger: true });
              if (!ok) return;
              try { await api.del('/api/v1/providers/' + id); toast(provider.name + ' deleted'); store.invalidate(); CCAM.router.clearGuard(); CCAM.router.go('/providers'); }
              catch (e) { toast('Delete failed: ' + (e.detail || e.message), 'bad'); }
            } }, icon('trash'), 'Delete provider');
          const actionsCard = h('div', { class: 'card' },
            h('p', { class: 'eyebrow' }, 'Provider actions'),
            h('div', { class: 'pr-action-row' }, h('div', null, h('p', { class: 'lede' }, 'Create a copy of this provider.'), duplicateHelp), duplicateButton),
            h('div', { class: 'pr-action-row' }, h('p', { class: 'lede' }, 'Remove this provider and release its session bindings.'), deleteButton));
          right = h('div', { class: 'stack' }, healthCard, channelCard, sessionCard, actionsCard);
        }
        replace(host, isNew ? h('div', { class: 'grid-2' }, formCard, h('div', { class: 'stack' }, h('div', { class: 'note' }, h('b', null, 'What a provider is.'), ' One base URL, one key, the models it serves, a priority tier and an on/off switch. Special upstream quirks are handled by opt-in patches, not by provider types.'),
          h('div', { class: 'note' }, h('b', null, 'Scheduling.'), ' Requests for a model go to the highest-priority tier that has a healthy provider declaring that model. Equal priorities round-robin; a Claude Code session sticks to one provider until it cools down.'))) : h('div', { class: 'grid-2' }, formCard, right));
        check();

        async function save() {
          if (saving || disposed) return;
          const body = JSON.parse(JSON.stringify(draft));
          body.tls = tls.getSubmission();
          if (!isNew) body.id = id; else delete body.id;
          saving = true; formCard.inert = true; bar.busy(true); syncDuplicate();
          const read = async () => {
            const list = await api.get('/api/v1/providers');
            return list.find(p => isNew ? p.name === body.name : p.id === id);
          };
          const matches = p => p && store.same(Object.assign({}, p, { id: undefined }), Object.assign({}, body, { id: undefined }));
          await complete(() => store.confirmedWrite(() => isNew ? api.post('/api/v1/providers', body) : api.put('/api/v1/providers/' + id, body), read, matches));
        }
        async function complete(run) {
          try {
            const saved = await run();
            store.invalidate();
            if (disposed) return;
            saving = false;
            toast(isNew ? saved.name + ' added' : 'Saved ' + saved.name);
            CCAM.router.clearGuard();
            if (isNew) { CCAM.router.go('/providers/' + saved.id); return; }
            const fresh = await store.providerHealth().catch(() => null);
            if (disposed) return;
            render(saved, patches, (fresh && fresh.providers.find(p => p.id === id)) || Object.assign({}, diag, saved));
          } catch (e) {
            if (disposed) return;
            if (e.verify) { bar.setError(e.detail); bar.pending(() => complete(e.verify)); return; }
            saving = false; formCard.inert = false; bar.busy(false); syncDuplicate();
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
