/* Service: listener, keys, runtime information, raw configuration. */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  const { h, icon, replace, pill, field, input, secretInput, copyButton, switchCtl, kv, skeleton, errorCard, savebar, toast, confirm } = CCAM.ui;
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

  function probeConsole(origin, options) {
    return new Promise(resolve => {
      const img = new Image();
      let finished = false;
      const aborted = () => finish(false);
      const finish = ok => { if (finished) return; finished = true; clearTimeout(timer); options.signal.removeEventListener('abort', aborted); img.onload = img.onerror = null; img.removeAttribute('src'); resolve(ok); };
      const timer = setTimeout(() => finish(false), options.timeoutMs);
      if (options.signal.aborted) { finish(false); return; }
      options.signal.addEventListener('abort', aborted, { once: true });
      img.onload = () => finish(true); img.onerror = () => finish(false);
      img.src = origin + '/management/assets/favicon.svg?restart=' + Date.now();
    });
  }

  CCAM.pages.service = {
    render(ctx) {
      ctx.title('Service');
      ctx.subtitle(['Process binding, keys and runtime state of ', h('b', null, 'this CC AutoMux instance')]);
      const host = h('div', null, skeleton(5, 56));
      ctx.root.appendChild(host);
      let bar = null, disposed = false, saving = false;
      const disposers = [];

      async function load() {
        try { const config = await api.get('/api/v1/config'); if (disposed) return; render(config); }
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
        CCAM.router.setGuard(async () => saving || dirty() ? confirm({ title: 'Discard changes?', text: saving ? 'Saving will continue after you leave. Other unsaved edits will be discarded.' : 'Service settings have unsaved changes.', confirmLabel: 'Discard', danger: true }) : true);

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
            [h('span', { 'data-tip': "Session bindings currently pinned to a provider, keyed by session, model, and request type. A binding lasts one hour after its last request. Not the number of running Claude Code sessions." }, 'Sticky bindings'), String(st.sticky_assignment_count)],
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
          CCAM.host.capabilities.autostart.get().then(v => { if (!disposed) sw.setChecked(v); }).catch(e => { if (!disposed) toast(e.message, 'bad'); });
          sw.input.addEventListener('change', async () => {
            sw.setBusy(true);
            try { await CCAM.host.capabilities.autostart.set(sw.input.checked); }
            catch (e) { if (!disposed) { toast(e.message, 'bad'); try { sw.setChecked(await CCAM.host.capabilities.autostart.get()); } catch (_) { sw.setDisabled(true); } } }
            finally { if (!disposed) sw.setBusy(false); }
          });
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
          if (saving || disposed) return;
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
            if (!ok || disposed || saving) return;
          }
          saving = true; listenerCard.inert = true; keysCard.inert = true; bar.busy(true);
          const sameSession = CCAM.auth.sessionGuard();
          store.stopPolling();
          const resumeUnauthorized = api.pauseUnauthorized();
          let rejectRotation;
          try {
            let redirected = false;
            await store.saveConfig(config, body, async (result, applied, task) => {
              if (!sameSession()) return;
              if (result.restart_required) {
                // Keep subsequent saves queued until the replacement is ready.
                redirected = await waitForRestart({ config: applied, oldKey: CCAM.auth.key(), sameSession }, task);
              } else {
                CCAM.auth.adopt(applied.auth.management_key); toast('Service settings saved');
              }
            }, applied => ({
              onSubmit: () => {
                if (!sameSession()) throw new api.ApiError(401, 'session_changed', 'The sign-in session changed before submission.');
                rejectRotation = CCAM.auth.retain(applied.auth.management_key);
              },
              onRejected: () => rejectRotation?.(),
              onDisconnect: async options => {
                const target = new URL('http://' + applied.service.listen_addr);
                if (target.origin !== location.origin && await probeConsole(target.origin, options)) return { restart_required: true, restarting: true };
                return null;
              }
            }));
            if (redirected || !sameSession()) return;
            if (!disposed) bar.busy(false);
            store.invalidate(); store.startPolling(); if (!disposed) { CCAM.router.clearGuard(); load(); }
          } catch (e) {
            if (!sameSession()) return;
            store.startPolling();
            if (disposed) { toast(e.detail || e.message, 'bad'); return; }
            bar.busy(false);
            if (e.field) { const f = fields[e.field.replace(/^service\./, '').replace(/^auth\./, '')]; if (f && f.setError) { f.setError(e.detail); return; } if (e.field === 'auth') { fields.gateway_key.setError(e.detail); return; } }
            bar.setError(e.code === 'restart_in_progress' ? 'CC AutoMux is already restarting — try again in a moment.' : (e.detail || e.message));
          } finally { saving = false; resumeUnauthorized(); if (!disposed) { listenerCard.inert = false; keysCard.inert = false; } }
        }
        async function waitForRestart(plan, task) {
          const target = new URL(location.href);
          target.hostname = '127.0.0.1'; target.port = plan.config.service.listen_addr.split(':')[1];
          target.pathname = '/management'; target.search = ''; target.hash = '#/service';
          const changedOrigin = target.origin !== location.origin;
          toast(changedOrigin ? 'Restarting CC AutoMux at ' + target.origin + '. Sign in again at the new address.' : 'Restarting CC AutoMux…', 'warn');
          for (let attempt = 0; attempt < 3; attempt++) {
            await task.wait((attempt + 1) * 100);
            if (!plan.sameSession()) return false;
            if (changedOrigin && await probeConsole(target.origin, task.options())) {
              task.check();
              if (!plan.sameSession()) return false;
              CCAM.auth.logout();
              location.replace(target.href);
              return true;
            }
            // Poll only our current origin. Try both credentials during a
            // pending rotation without signing out on an expected 401.
            for (const key of new Set([plan.config.auth.management_key, plan.oldKey])) {
              let st;
              try {
                st = await api.get('/api/v1/status', { ...task.options(), silentUnauthorized: true, headers: { Authorization: 'Bearer ' + key } });
                task.check();
              } catch (e) { task.check(); continue; }
              if (!plan.sameSession()) return false;
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
          throw task.failure();
        }

      }
      load();
      return () => { disposed = true; disposers.forEach(d => d()); if (bar) bar.destroy(); };
    }
  };
})();
