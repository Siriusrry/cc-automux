/* Auto Mode: classifier routing configuration. */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  const { h, icon, replace, pill, tag, field, input, secretInput, seg, select, kv, skeleton, errorCard, savebar, toast, confirm, healthDot, priorityTip } = CCAM.ui;
  const fmt = CCAM.fmt;
  const store = CCAM.store;
  const api = CCAM.api;
  CCAM.pages = CCAM.pages || {};

  const PROTOCOLS = [
    { value: 'anthropic_messages', label: 'Anthropic Messages', path: '/v1/messages', implemented: true },
    { value: 'openai_responses', label: 'OpenAI Responses', path: '/v1/responses', implemented: false },
    { value: 'openai_compatible', label: 'OpenAI-compatible Chat Completions', path: '/v1/chat/completions', implemented: false }
  ];
  function blankFixed() { return { base_url: '', api_key: '', use_x_api_key: false, protocol: 'anthropic_messages', tls: { ca_file: '', insecure_skip_verify: false }, patches: [] }; }

  CCAM.pages['auto-mode'] = {
    render(ctx) {
      ctx.title('Auto Mode');
      ctx.subtitle(['Where Claude Code’s ', h('b', null, 'security-monitor classifier'), ' requests go']);
      const host = h('div', null, skeleton(5, 48));
      ctx.root.appendChild(host);
      let bar = null, disposed = false;

      async function load() {
        try {
          const [config, providers, patches] = await Promise.all([store.config(), store.providers(), store.patches()]);
          if (disposed) return;
          render(config, providers, patches);
        } catch (e) { if (!disposed) replace(host, errorCard(e.detail || e.message, load)); }
      }

      function render(config, providers, patches) {
        disposers.splice(0).forEach(dispose => dispose());
        const draft = JSON.parse(JSON.stringify(config.auto_mode));
        if (!draft.fixed_provider) draft.fixed_provider = null;
        const baseline = JSON.stringify(draft);
        const fields = {};
        let lastFixed = draft.fixed_provider ? JSON.parse(JSON.stringify(draft.fixed_provider)) : blankFixed();
        if (bar) bar.destroy();
        bar = savebar({ onSave: save, onRevert: load });
        const dirty = () => JSON.stringify(draft) !== baseline;
        const check = () => { bar.show(dirty()); Object.values(fields).forEach(f => f.setError && f.setError('')); bar.setError(''); };
        CCAM.router.setGuard(async () => dirty() ? confirm({ title: 'Discard changes?', text: 'Auto mode has unsaved changes.', confirmLabel: 'Discard', danger: true }) : true);

        // Mode
        const modeDesc = h('p', { class: 'am-desc' });
        const modeSeg = seg({ ariaLabel: 'Auto mode', value: draft.mode, options: [{ value: 'disabled', label: 'Off' }, { value: 'provider_pool', label: 'Provider pool' }, { value: 'fixed_provider', label: 'Fixed provider' }], onchange: (v) => {
          if (draft.mode === 'fixed_provider' && draft.fixed_provider) lastFixed = JSON.parse(JSON.stringify(draft.fixed_provider));
          draft.mode = v;
          if (v === 'disabled') { draft.model = ''; draft.fixed_provider = null; }
          if (v === 'provider_pool') { draft.fixed_provider = null; }
          if (v === 'fixed_provider') { draft.fixed_provider = JSON.parse(JSON.stringify(lastFixed)); }
          modelInput.value = draft.model;
          sync(); check();
        } });

        // Shared classifier model
        const modelInput = input({ id: 'am-model', value: draft.model, placeholder: 'claude-haiku-4-5', list: 'am-models', oninput: (e) => { draft.model = e.target.value; renderCands(); check(); } });
        const datalist = h('datalist', { id: 'am-models' }, Array.from(new Set(providers.flatMap(p => p.models))).sort().map(m => h('option', { value: m })));
        fields.model = field({ label: 'Classifier model', for: 'am-model', control: [modelInput, datalist], help: 'Written over the model in every classifier request. Shared by both modes.' });

        const candsEl = h('div', { class: 'am-cands' });
        const candsHelp = h('p', { class: 'help' });
        function renderCands() {
          const cands = providers.filter(p => p.enabled && p.models.includes(draft.model)).sort((a, b) => b.priority - a.priority);
          replace(candsEl, cands.map(p => h('div', { class: 'am-cand' }, healthDot('unknown'), h('span', { class: 'n' }, p.name), h('span', { class: 'prio', 'data-tip': priorityTip(p.priority) }, 'P ' + fmt.priorityLabel(p.priority)), tag(fmt.host(p.base_url)))));
          if (!draft.model) { candsHelp.textContent = 'Enter a model to see which providers can serve the classifier.'; candsHelp.className = 'help'; }
          else if (!cands.length) { candsHelp.textContent = 'No enabled provider declares this model — classifier requests would get 404 model_not_configured.'; candsHelp.className = 'help warn-text'; }
          else { candsHelp.textContent = fmt.plural(cands.length, 'provider') + ' can serve the classifier. The highest healthy tier is used; sessions stick to one provider and use the classifier health channel. Each request calls at most one provider.'; candsHelp.className = 'help'; }
        }
        const poolBlock = h('div', { class: 'field' }, h('span', { class: 'lab' }, 'Candidates'), candsEl, candsHelp);

        // Fixed provider
        const fx = () => draft.fixed_provider || (draft.fixed_provider = blankFixed());
        const urlIn = input({ id: 'fx-url', value: lastFixed.base_url, placeholder: 'https://classifier.example', oninput: (e) => { fx().base_url = e.target.value; check(); } });
        fields['fixed.base_url'] = field({ label: 'Base URL', for: 'fx-url', control: urlIn, help: h('span', { class: 'fx-path' }) });
        const keyWrap = secretInput({ id: 'fx-key', value: lastFixed.api_key, placeholder: 'target API key', copy: true, oninput: (e) => { fx().api_key = e.target.value; check(); } });
        fields['fixed.api_key'] = field({ label: 'API key', for: 'fx-key', control: keyWrap });
        const authSeg = CCAM.ui.authHeaderSeg({ useXApiKey: lastFixed.use_x_api_key, onchange: (v) => { fx().use_x_api_key = v; check(); } });
        fields['fixed.use_x_api_key'] = field({ label: 'Authentication header', control: authSeg });
        const protoSel = select({ id: 'fx-proto', value: lastFixed.protocol, options: PROTOCOLS.map(p => ({ value: p.value, label: p.label, hint: p.implemented ? '' : 'not implemented' })), onchange: (e) => { fx().protocol = e.target.value; syncProto(); check(); } });
        const protoNote = h('div', { class: 'note warn', hidden: true });
        fields['fixed.protocol'] = field({ label: 'Protocol', for: 'fx-proto', control: protoSel, help: h('span', { class: 'fx-proto-help' }) });
        const tls = CCAM.ui.tlsControl({ idPrefix: 'fx', value: lastFixed.tls, onchange: (v) => { fx().tls = v; check(); } });
        fields['fixed.tls'] = field({ label: 'TLS', control: tls });
        const patchSel = CCAM.ui.patchSelector({ patches, selected: lastFixed.patches, applicableTypes: ['classifier'], onchange: (ids) => { fx().patches = ids; check(); } });
        fields['fixed.patches'] = field({ label: 'Patches', control: patchSel, help: 'Only patches that apply to classifier requests are offered here.' });
        function syncProto() {
          const p = PROTOCOLS.find(x => x.value === (draft.fixed_provider ? draft.fixed_provider.protocol : lastFixed.protocol)) || PROTOCOLS[0];
          fields['fixed.base_url'].querySelector('.fx-path').textContent = 'CC AutoMux appends ' + p.path + ' for this protocol and keeps the client query string.';
          fields['fixed.protocol'].querySelector('.fx-proto-help').textContent = p.implemented ? 'The canonical Anthropic request is sent as-is and the response is used directly.' : 'Requests would be converted to ' + p.label + ' and the response converted back.';
          protoNote.hidden = p.implemented;
          replace(protoNote, h('b', null, 'Not implemented in this version. '), 'Saving is allowed, but every classifier request will fail closed with ', h('code', null, '501 protocol_not_implemented'), ' until a protocol adapter ships. Nothing is passed through unconverted and nothing falls back to the provider pool.');
        }
        const fixedBlock = h('div', null, fields['fixed.base_url'], h('div', { class: 'row-2' }, fields['fixed.api_key'], fields['fixed.use_x_api_key']), fields['fixed.protocol'], protoNote, h('div', { style: { height: '16px' } }), fields['fixed.tls'], h('hr', { class: 'hr' }), fields['fixed.patches']);

        const modeCard = h('div', { class: 'card' },
          h('p', { class: 'eyebrow' }, 'Mode'),
          h('div', { class: 'am-mode' }, modeSeg, modeDesc),
          h('hr', { class: 'hr' }),
          fields.model, poolBlock, fixedBlock);

        function sync() {
          const m = draft.mode;
          fields.model.hidden = m === 'disabled';
          poolBlock.hidden = m !== 'provider_pool';
          fixedBlock.hidden = m !== 'fixed_provider';
          modeCard.querySelector('.hr').hidden = m === 'disabled';
          replace(modeDesc, m === 'disabled' ? ['Classifier requests are answered with ', h('code', null, '503 auto_mode_not_configured'), '. Claude Code auto mode will not work through the gateway; everything else routes normally.']
            : m === 'provider_pool' ? ['Classifier requests use the ', h('b', null, 'same provider pool'), ' as normal traffic: every enabled provider declaring the classifier model, by priority, with per-session stickiness and a dedicated classifier health channel.']
            : ['Classifier requests go to ', h('b', null, 'one upstream outside the pool'), '. It never serves normal traffic, has no health state and no session stickiness, and is called exactly once per request.']);
          renderCands(); syncProto();
        }
        sync();

        // Right column
        const detectCard = h('div', { class: 'card' }, h('p', { class: 'eyebrow' }, 'Auto mode compatibility'),
          h('p', { class: 'lede' }, 'Claude Code’s auto mode can run into compatibility issues with third-party APIs. CC AutoMux helps restore it while letting you choose the model and provider used for auto mode.'),
          h('div', { class: 'am-detect' }, h('div', null, h('span', { class: 'k' }, 'Model choice'), ' — use a dedicated classifier model without changing your main model.'), h('div', null, h('span', { class: 'k' }, 'Provider choice'), ' — use your provider pool or a fixed provider for auto mode.')),
          h('p', { class: 'lede', style: { marginTop: '12px' } }, 'Apply provider compatibility patches as needed to help auto mode work with your chosen upstream.'),
          h('div', { class: 'am-flow' }, h('span', null, 'detect'), h('i', null, '→'), h('span', { class: 'iris' }, 'override model'), h('i', null, '→'), h('span', null, 'request patches'), h('i', null, '→'), h('span', { class: 'iris' }, 'pool or fixed target'), h('i', null, '→'), h('span', null, 'response patches')));
        const lastCallCard = h('div', { class: 'card am-lastcall' }, h('p', { class: 'eyebrow' }, 'Last fixed-target call'), h('p', { class: 'lede' }, 'The fixed target has no pool health; its most recent call is kept in memory for diagnosis.'), h('div', { class: 'lc-body' }));
        function renderLastCall() {
          const st = store.state.status; const body = lastCallCard.querySelector('.lc-body');
          const lc = st && st.auto_mode && st.auto_mode.fixed_target_last_call;
          if (!st || st.auto_mode.mode !== 'fixed_provider') { replace(body, h('p', { class: 'faint', style: { fontSize: '13px', margin: 0 } }, 'Only recorded while fixed-provider mode is active.')); return; }
          if (!lc) { replace(body, h('p', { class: 'faint', style: { fontSize: '13px', margin: 0 } }, 'No classifier request has reached the fixed target yet.')); return; }
          const ok = lc.gateway_status >= 200 && lc.gateway_status < 300;
          replace(body, h('div', { class: 'hl-diag' }, pill(ok ? 'Succeeded' : 'Failed · ' + lc.gateway_status + (lc.gateway_error ? ' ' + lc.gateway_error : ''), ok ? 'ok' : 'bad'), h('span', { class: 'hl-sub' }, fmt.dateTime(lc.observed_at) + ' · ' + fmt.relative(lc.observed_at))),
            kv([['Upstream URL', h('span', { class: 'mono' }, lc.upstream_url)], ['Upstream status', lc.upstream_status ? String(lc.upstream_status) : null], ['Session', lc.session_id ? h('span', { class: 'sid' }, lc.session_id) : null]], { compact: true }),
            lc.error ? [h('p', { class: 'lab', style: { margin: '12px 0 6px', fontSize: '12.5px', fontWeight: '600' } }, 'Error'), h('pre', { class: 'code wrap clamp', onclick: (e) => e.currentTarget.classList.toggle('clamp') }, lc.error)] : null,
            lc.upstream_body ? h('details', { class: 'disc', style: { marginTop: '10px' } }, h('summary', null, icon('chevron-right'), 'Upstream response headers and body'), h('div', { class: 'disc-body' }, h('pre', { class: 'code wrap' }, Object.keys(lc.upstream_headers || {}).map(k => k + ': ' + lc.upstream_headers[k].join(', ')).join('\n') + '\n\n' + lc.upstream_body))) : null);
        }
        renderLastCall();
        const healthNote = h('div', { class: 'note' }, h('b', null, 'Errors are never softened.'), ' A failed classifier call is returned as-is or as a gateway 502; CC AutoMux never fabricates an allow or block decision. Each classifier request calls exactly one upstream — there is no same-request failover.');

        replace(host, h('div', { class: 'grid-2' }, modeCard, h('div', { class: 'stack' }, detectCard, lastCallCard, healthNote)));
        const offStatus = store.on('status', renderLastCall);
        check();
        disposers.push(offStatus);

        async function save() {
          const body = store.clientUpdate(config);
          body.auto_mode = JSON.parse(JSON.stringify(draft));
          if (!body.auto_mode.fixed_provider) delete body.auto_mode.fixed_provider;
          bar.busy(true);
          try {
            await store.saveConfig(config, body);
            bar.busy(false); toast('Auto mode saved'); store.invalidate(); store.refreshStatus(); CCAM.router.clearGuard(); load();
          } catch (e) {
            bar.busy(false);
            if (e.field) { const f = fields[e.field.replace(/\[\d+\]$/, '')] || fields[e.field.replace(/^fixed\.tls\..*/, 'fixed.tls')]; if (f && f.setError) { f.setError(e.detail); const inp = f.querySelector('input, select'); if (inp) inp.focus(); return; } }
            bar.setError(e.code === 'restart_in_progress' ? 'CC AutoMux is restarting — try again in a moment.' : (e.detail || e.message));
          }
        }
      }
      const disposers = [];
      load();
      return () => { disposed = true; disposers.forEach(d => d()); if (bar) bar.destroy(); };
    }
  };
})();
