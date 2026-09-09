/* Service: listener, keys, runtime information, raw configuration. */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  const { h, icon, replace, pill, field, input, secretInput, copyButton, switchCtl, kv, skeleton, errorCard, savebar, toast, confirm, overlay } = CCAM.ui;
  const fmt = CCAM.fmt;
  const store = CCAM.store;
  const api = CCAM.api;
  CCAM.pages = CCAM.pages || {};

  function maskConfig(cfg) {
    const c = JSON.parse(JSON.stringify(cfg));
    c.auth.gateway_key = fmt.mask(c.auth.gateway_key); c.auth.management_key = fmt.mask(c.auth.management_key);
    (c.providers || []).forEach(p => { p.api_key = fmt.mask(p.api_key); });
    if (c.auto_mode && c.auto_mode.fixed_provider) c.auto_mode.fixed_provider.api_key = fmt.mask(c.auto_mode.fixed_provider.api_key);
    return c;
  }

  function probeConsole(origin) {
    return new Promise(resolve => {
      const img = new Image();
      let finished = false;
      const finish = ok => { if (finished) return; finished = true; clearTimeout(timer); img.onload = img.onerror = null; img.removeAttribute('src'); resolve(ok); };
      const timer = setTimeout(() => finish(false), 1000);
      img.onload = () => finish(true); img.onerror = () => finish(false);
      img.src = origin + '/ui/assets/favicon.svg?restart=' + Date.now();
    });
  }

  CCAM.pages.service = {
    render(ctx) {
      ctx.title('Service');
      ctx.subtitle(['Process binding, keys and runtime state of ', h('b', null, 'this CC AutoMux instance')]);
      const host = h('div', null, skeleton(5, 56));
      ctx.root.appendChild(host);
      let bar = null, disposed = false;
      const disposers = [];

      async function load() {
        try { const config = await store.config(); if (disposed) return; render(config); }
        catch (e) { if (!disposed) replace(host, errorCard(e.detail || e.message, load)); }
      }

      function render(config) {
        disposers.splice(0).forEach(dispose => dispose());
        const port = Number(config.service.listen_addr.split(':')[1]);
        const draft = { port, logMb: config.service.log_max_bytes / 1048576, gateway_key: config.auth.gateway_key, management_key: config.auth.management_key };
        const baseline = JSON.stringify(draft);
        const fields = {};
        if (bar) bar.destroy();
        bar = savebar({ onSave: save, onRevert: load });
        const dirty = () => JSON.stringify(draft) !== baseline;
        const restartNeeded = () => draft.port !== port || Math.round(draft.logMb * 1048576) !== config.service.log_max_bytes;
        const check = () => { bar.show(dirty()); Object.values(fields).forEach(f => f.setError && f.setError('')); bar.setError(''); if (dirty() && restartNeeded()) bar.setError(''); restartNote.hidden = !restartNeeded(); };
        CCAM.router.setGuard(async () => dirty() ? confirm({ title: 'Discard changes?', text: 'Service settings have unsaved changes.', confirmLabel: 'Discard', danger: true }) : true);

        // Listener
        fields.listen_addr = field({ label: 'Port', for: 'sv-port', control: input({ id: 'sv-port', type: 'number', value: String(port), attrs: { min: '1', max: '65535', step: '1' }, oninput: (e) => { draft.port = Number(e.target.value) || 0; check(); } }) });
        fields.log_max_bytes = field({ label: 'Log size limit', for: 'sv-log', control: h('div', { class: 'in-wrap' }, input({ id: 'sv-log', type: 'number', value: String(draft.logMb), attrs: { min: '1', step: '1' }, oninput: (e) => { draft.logMb = Number(e.target.value) || 0; check(); } }), h('span', { class: 'faint', style: { padding: '0 10px', fontSize: '12.5px' } }, 'MB')), help: 'Total of the active file and one archive; the active file rotates at half this size.' });
        const restartNote = h('div', { class: 'note warn', hidden: true }, h('b', null, 'Saving restarts CC AutoMux. '), 'The listener and log limit are bound at startup, so the process re-executes itself after writing the pending configuration. In-flight requests are interrupted.');
        const listenerCard = h('div', { class: 'card' }, h('p', { class: 'eyebrow' }, 'Listener'), h('p', { class: 'lede' }, 'The gateway and this console share one loopback listener. The host is fixed to 127.0.0.1 and cannot be exposed.'),
          h('div', { class: 'sv-host' }, field({ label: 'Host', for: 'svc-host', control: input({ id: 'svc-host', value: '127.0.0.1', disabled: true }) }), fields.listen_addr, fields.log_max_bytes), h('div', { style: { height: '14px' } }), restartNote);

        // Keys
        const gwWrap = secretInput({ id: 'sv-gw', value: draft.gateway_key, placeholder: 'empty → data plane returns 503', copy: true, oninput: (e) => { draft.gateway_key = e.target.value; check(); } });
        const genBtn = h('button', { class: 'btn', type: 'button', onclick: () => { draft.gateway_key = fmt.randomKey(32); gwWrap.input.value = draft.gateway_key; gwWrap.input.type = 'text'; check(); } }, icon('key'), 'Generate');
        fields.gateway_key = field({ label: 'Gateway key', for: 'sv-gw', control: h('div', { class: 'sv-keyrow' }, gwWrap, genBtn), help: ['Claude Code sends this as ', h('code', null, 'ANTHROPIC_AUTH_TOKEN'), '; profile activation writes it into settings.json. Changing it invalidates the active profile until it is activated again.'] });
        const mkNew = secretInput({ id: 'sv-mk', value: '', placeholder: 'new management key', oninput: (e) => { draft.management_key = e.target.value || config.auth.management_key; check(); } });
        const rotate = h('details', { class: 'disc sv-rotate' }, h('summary', null, icon('chevron-right'), 'Rotate the management key'), h('div', { class: 'disc-body' },
          field({ label: 'New management key', for: 'sv-mk', control: h('div', { class: 'sv-keyrow' }, mkNew, h('button', { class: 'btn', type: 'button', onclick: () => { draft.management_key = fmt.randomKey(32); mkNew.input.value = draft.management_key; mkNew.input.type = 'text'; check(); } }, icon('key'), 'Generate')), help: 'This console switches to the new key as soon as it is saved. Other sessions must sign in again. Must differ from the gateway key.' })));
        fields.management_key = rotate;
        const keysCard = h('div', { class: 'card' }, h('p', { class: 'eyebrow' }, 'Keys'), h('p', { class: 'lede' }, 'Two independent credentials: the gateway key protects the Messages data plane, the management key protects this console and the API.'), fields.gateway_key, rotate);

        // Runtime
        const runtimeBody = h('div');
        function renderRuntime() {
          const st = store.state.status; if (!st) { replace(runtimeBody, skeleton(4, 20)); return; }
          const r = st.restart || {};
          replace(runtimeBody, kv([
            ['Product', st.product + ' ' + st.version],
            [h('span', { 'data-tip': 'Increments when configuration changes within this process. A new process starts its own revision counter.' }, 'Configuration revision'), String(st.revision)],
            ['Started', fmt.dateTime(st.start_time) + ' · up ' + fmt.duration(st.uptime_seconds)],
            ['Listening on', h('span', { class: 'mono' }, st.listen_addr)],
            [h('span', { 'data-tip': 'Messages requests being served right now, including retries and failovers still in progress.' }, 'Requests in flight'), String(st.active_data_requests)],
            [h('span', { 'data-tip': "Sessions currently pinned to a provider. A pin lasts one hour after the session's last request." }, 'Sticky sessions'), String(st.sticky_assignment_count)],
            [h('span', { 'data-tip': 'idle — nothing pending\npending — the new configuration is written and waits for the re-exec\nrestarting — the new process is binding\nfailed — the previous configuration and listener were restored' }, 'Restart'), [pill(r.state === 'idle' ? 'idle' : r.state, r.state === 'idle' ? 'mist' : r.state === 'failed' ? 'bad' : 'warn', 'plain'), r.pending ? ' pending configuration written' : '', r.requested_at ? ' · ' + fmt.relative(r.requested_at) : '']],
            r.last_error ? ['Last restart error', h('span', { class: 'mono' }, r.last_error)] : null
          ], { compact: true }),
            h('p', { class: 'eyebrow', style: { marginTop: '18px' } }, 'Logging'),
            st.logging.healthy ? h('div', { class: 'sv-log ok' }, icon('check'), h('div', null, h('div', { class: 't' }, 'Writing normally'), h('div', { class: 'd' }, 'Records are persisted before they are streamed to the log view.')))
              : h('div', { class: 'sv-log bad' }, icon('alert'), h('div', null, h('div', { class: 't' }, 'Log writes are failing · ' + fmt.plural(st.logging.failures, 'failure')), h('div', { class: 'd' }, 'Last failure ' + fmt.dateTime(st.logging.last_failure_at) + ' · ' + (st.logging.last_error || '')), h('div', { class: 'd' }, 'The gateway keeps serving. Until writes recover, the log view shows nothing newer than the last successful write.'))));
        }
        renderRuntime();
        const runtimeCard = h('div', { class: 'card sv-runtime' }, h('p', { class: 'eyebrow' }, 'Runtime'), runtimeBody);
        if (CCAM.host.capabilities.autostart) {
          const sw = switchCtl({ checked: false, label: 'Launch at login', sub: 'registered with the operating system' });
          CCAM.host.capabilities.autostart.get().then(v => { sw.input.checked = v; });
          sw.input.addEventListener('change', () => CCAM.host.capabilities.autostart.set(sw.input.checked));
          runtimeCard.appendChild(h('div', { style: { marginTop: '16px' } }, sw));
        }

        // Configuration JSON
        let masked = true;
        const code = h('pre', { class: 'code' }, fmt.pretty(maskConfig(config)));
        const maskBtn = h('button', { class: 'btn sm', type: 'button', onclick: () => { masked = !masked; code.textContent = fmt.pretty(masked ? maskConfig(config) : config); replace(maskBtn, icon(masked ? 'eye' : 'eye-off'), masked ? 'Reveal keys' : 'Mask keys'); } }, icon('eye'), 'Reveal keys');
        const jsonCard = h('div', { class: 'card sv-json' }, h('div', { class: 'card-head' }, h('div', null, h('p', { class: 'eyebrow' }, 'Configuration'), h('p', { class: 'lede', style: { margin: 0 } }, 'The complete resource as stored on disk, including server-owned state.')), h('div', { class: 'sv-json-acts' }, maskBtn, copyButton(() => fmt.pretty(config), 'Copy configuration'))), code);

        replace(host, h('div', { class: 'grid-2' }, h('div', { class: 'stack' }, listenerCard, keysCard), h('div', { class: 'stack' }, runtimeCard, jsonCard)));
        disposers.push(store.on('status', renderRuntime));
        check();

        async function save() {
          if (!(Number.isInteger(draft.port) && draft.port >= 1 && draft.port <= 65535)) { fields.listen_addr.setError('Port must be between 1 and 65535.'); return; }
          if (!(draft.logMb >= 1)) { fields.log_max_bytes.setError('Use at least 1 MB.'); return; }
          if (draft.gateway_key && draft.gateway_key === draft.management_key) { fields.gateway_key.setError('The gateway key must differ from the management key.'); return; }
          const body = store.clientUpdate(config);
          body.service.listen_addr = '127.0.0.1:' + draft.port;
          body.service.log_max_bytes = Math.round(draft.logMb * 1048576);
          body.auth.gateway_key = draft.gateway_key;
          body.auth.management_key = draft.management_key;
          if (restartNeeded()) {
            const ok = await confirm({ title: 'Restart CC AutoMux?', text: 'The new listener or log limit takes effect only after the process re-executes itself. In-flight Claude Code requests will be interrupted.', confirmLabel: 'Save and restart' });
            if (!ok) return;
          }
          bar.busy(true);
          store.stopPolling();
          const resumeUnauthorized = api.pauseUnauthorized();
          try {
            let restartPlan;
            const result = await store.saveConfig(config, body, (result, applied) => {
              restartPlan = { config: applied, oldKey: CCAM.auth.key() };
              if (!result.restart_required) CCAM.auth.adopt(applied.auth.management_key);
            });
            bar.busy(false);
            if (result && result.restart_required) {
              bar.show(false); CCAM.router.clearGuard();
              if (await waitForRestart(restartPlan)) return;
            } else { toast('Service settings saved'); }
            store.invalidate(); store.startPolling(); CCAM.router.clearGuard(); load();
          } catch (e) {
            bar.busy(false); store.startPolling();
            if (e.field) { const f = fields[e.field.replace(/^service\./, '').replace(/^auth\./, '')]; if (f && f.setError) { f.setError(e.detail); return; } if (e.field === 'auth') { fields.gateway_key.setError(e.detail); return; } }
            bar.setError(e.code === 'restart_in_progress' ? 'CC AutoMux is already restarting — try again in a moment.' : (e.detail || e.message));
          } finally { resumeUnauthorized(); }
        }
        async function waitForRestart(plan) {
          const target = new URL(location.href);
          target.hostname = '127.0.0.1'; target.port = plan.config.service.listen_addr.split(':')[1];
          target.pathname = '/ui/'; target.search = ''; target.hash = '#/service';
          const changedOrigin = target.origin !== location.origin;
          const ov = overlay('Restarting CC AutoMux…', changedOrigin
            ? 'Opening ' + target.origin + '. Sign in again at the new address.'
            : 'Waiting for the service to apply the new configuration…');
          const started = Date.now();
          try {
            while (!disposed && Date.now() - started < 20000) {
              await new Promise(r => setTimeout(r, 500));
              if (changedOrigin && await probeConsole(target.origin)) {
                CCAM.auth.logout();
                location.replace(target.href);
                return true;
              }
              // Poll only our current origin. Try both credentials during a
              // pending rotation without signing out on an expected 401.
              for (const key of new Set([plan.config.auth.management_key, plan.oldKey])) {
                let st;
                try {
                  const controller = new AbortController();
                  const timer = setTimeout(() => controller.abort(), 1500);
                  try { st = await api.get('/api/v1/status', { silentUnauthorized: true, headers: { Authorization: 'Bearer ' + key }, signal: controller.signal }); }
                  finally { clearTimeout(timer); }
                } catch (e) { continue; }
                if (st.restart && st.restart.last_error && !st.restart_in_progress) {
                  CCAM.auth.adopt(key);
                  throw new Error('Restart failed. The previous configuration is still active: ' + st.restart.last_error);
                }
                // For a same-port restart, the changed startup-bound log limit
                // proves promotion even when two starts share one timestamp second.
                if (!changedOrigin && !st.restart_in_progress && !st.pending &&
                    st.listen_addr === plan.config.service.listen_addr && st.log_max_bytes === plan.config.service.log_max_bytes && key === plan.config.auth.management_key) {
                  CCAM.auth.adopt(key); toast('Restarted with the new configuration'); return false;
                }
              }
            }
            throw new Error('The service did not restart within 20 seconds. ' + (changedOrigin ? 'Open ' + target.href + ' to reconnect.' : 'Retry after checking the service.'));
          } finally { ov.close(); }
        }

      }
      load();
      return () => { disposed = true; disposers.forEach(d => d()); if (bar) bar.destroy(); };
    }
  };
})();
