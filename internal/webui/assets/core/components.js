/* CC AutoMux Web UI — composite controls shared by several pages (patch selector, TLS mode, auth header). */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  const { h, icon, replace, clear, tag, seg, input, field } = CCAM.ui;

  const STAGE_LABEL = { request: 'request', response: 'response' };

  // Ordered patch selection. `applicable` filters definitions by request type ("*" always passes).
  function patchSelector(opts) {
    const all = opts.patches || [];
    let selected = (opts.selected || []).slice();
    const applicable = opts.applicableTypes ? all.filter(d => d.request_types.includes('*') || d.request_types.some(t => opts.applicableTypes.includes(t))) : all;
    const wrap = h('div', { class: 'patches' });

    function conflictsWith(def) {
      const others = selected.filter(id => id !== def.id).map(id => all.find(d => d.id === id)).filter(Boolean);
      for (const o of others) {
        const typesMeet = def.request_types.includes('*') || o.request_types.includes('*') || def.request_types.some(t => o.request_types.includes(t));
        const stagesMeet = def.stages.some(s => o.stages.includes(s));
        if (typesMeet && stagesMeet && ((def.conflicts || []).includes(o.id) || (o.conflicts || []).includes(def.id))) return o;
      }
      return null;
    }
    function emit() { opts.onchange && opts.onchange(selected.slice()); }
    function render() {
      clear(wrap);
      if (!applicable.length) { wrap.appendChild(h('p', { class: 'help' }, 'No registered patch applies here.')); return; }
      const ordered = selected.map(id => applicable.find(d => d.id === id)).filter(Boolean);
      const rest = applicable.filter(d => !selected.includes(d.id));
      const list = h('div', { class: 'patch-list' });
      ordered.forEach((def, i) => list.appendChild(row(def, i)));
      rest.forEach(def => list.appendChild(row(def, -1)));
      wrap.appendChild(list);
      if (ordered.length > 1) wrap.appendChild(h('p', { class: 'help' }, 'Request hooks run top to bottom; response hooks run in reverse. Provider authentication is written last.'));
    }
    function row(def, index) {
      const on = index >= 0;
      const conflict = !on ? conflictsWith(def) : null;
      const cb = h('input', { type: 'checkbox', checked: on, disabled: !!conflict, id: 'patch-' + def.id });
      cb.addEventListener('change', () => { if (cb.checked) selected.push(def.id); else selected = selected.filter(x => x !== def.id); render(); emit(); });
      const move = (dir) => { const i = selected.indexOf(def.id); const j = i + dir; if (j < 0 || j >= selected.length) return; selected.splice(i, 1); selected.splice(j, 0, def.id); render(); emit(); };
      return h('div', { class: 'patch' + (on ? ' on' : '') + (conflict ? ' conflict' : '') },
        h('label', { class: 'patch-main', for: 'patch-' + def.id }, cb,
          h('div', { class: 'patch-body' },
            h('div', { class: 'patch-name' }, on ? h('span', { class: 'patch-idx' }, String(index + 1)) : null, def.name),
            h('div', { class: 'patch-desc' }, conflict ? ['Conflicts with ', h('b', null, conflict.name), ' on the same request type and stage.'] : def.description),
            h('div', { class: 'patch-tags' }, def.request_types.map(t => tag(t === '*' ? 'all types' : t, t === 'classifier' || t === '*' ? 'iris' : '')), def.stages.map(s => tag(STAGE_LABEL[s] || s)), def.idempotence === 'per_execution' ? tag('per execution') : null))),
        on ? h('div', { class: 'patch-order' },
          h('button', { class: 'ib', type: 'button', 'aria-label': 'Move up', disabled: index === 0, onclick: () => move(-1) }, icon('up')),
          h('button', { class: 'ib', type: 'button', 'aria-label': 'Move down', disabled: index === selected.length - 1, onclick: () => move(1) }, icon('down'))) : null);
    }
    render();
    wrap.getSelected = () => selected.slice();
    wrap.setSelected = (ids) => { selected = ids.slice(); render(); };
    return wrap;
  }

  // TLS trust selection mapped onto { ca_file, insecure_skip_verify }.
  function tlsControl(opts) {
    let value = { ca_file: (opts.value && opts.value.ca_file) || '', insecure_skip_verify: !!(opts.value && opts.value.insecure_skip_verify) };
    let selectedMode = value.insecure_skip_verify ? 'skip' : value.ca_file ? 'custom' : 'system';
    const mode = () => selectedMode;
    const caField = field({ label: 'CA file', control: input({ id: opts.idPrefix + '-ca', placeholder: '/absolute/path/to/ca.pem', value: value.ca_file, oninput: (e) => { value.ca_file = e.target.value; emit(); } }), help: 'PEM bundle used instead of the system roots. Cannot be combined with skipping verification.' });
    const help = h('p', { class: 'help' });
    const segEl = seg({ ariaLabel: 'TLS trust', value: mode(), options: [{ value: 'system', label: 'System roots' }, { value: 'custom', label: 'Custom CA file' }, { value: 'skip', label: 'Skip verification' }], onchange: (v) => {
      selectedMode = v;
      if (v === 'system') value = { ca_file: '', insecure_skip_verify: false };
      else if (v === 'custom') value = { ca_file: value.ca_file, insecure_skip_verify: false };
      else value = { ca_file: '', insecure_skip_verify: true };
      sync(); emit();
    } });
    function sync() {
      const m = mode();
      caField.hidden = m !== 'custom';
      help.textContent = m === 'system' ? 'Verify the upstream certificate against the operating system trust store.' : m === 'skip' ? 'Accept any certificate. Only for a self-signed local upstream you control.' : '';
      help.hidden = !help.textContent;
      help.classList.toggle('warn-text', m === 'skip');
    }
    sync();
    const wrap = h('div', { class: 'tls' }, segEl, help, caField);
    function emit() { opts.onchange && opts.onchange(Object.assign({}, value)); }
    wrap.getValue = () => Object.assign({}, value);
    wrap.setError = (msg) => caField.setError(msg);
    return wrap;
  }

  function authHeaderSeg(opts) {
    return seg({ ariaLabel: 'Authentication header', value: opts.useXApiKey ? 'x-api-key' : 'bearer', options: [{ value: 'bearer', label: 'Authorization: Bearer' }, { value: 'x-api-key', label: 'x-api-key' }], onchange: (v) => opts.onchange(v === 'x-api-key') });
  }

  // ---- hover summaries ----
  // Shared by the providers list, the routing map and the diagnostics card. Each returns a content
  // function so a live tooltip can re-render its cooldown countdown every second.
  const U = CCAM.ui;
  const STATE_NOTE = {
    healthy: 'Requests are routed here normally.',
    degraded: 'Recent failures, still in rotation. Further failures move it into cooldown.',
    cooldown: 'Skipped by routing until the retry time; one probe request then re-opens it.',
    half_open: 'Cooldown ended — a single probe request is being allowed through.',
    unknown: 'No request has been routed here since the service started.',
    disabled: 'Health tracking is off: always considered available and never enters cooldown.'
  };
  const STATIC_NOTE = {
    disabled_provider: 'Disabled in the configuration — receives no traffic until re-enabled.',
    no_models: 'No models declared — skipped by routing until at least one model is added.'
  };
  function clip(text) { text = String(text || ''); return text.length > 96 ? text.slice(0, 96) + '…' : text; }
  function stateTitle(kind, text) { return [U.h('i', { class: 'dot ' + (kind === 'ghost' ? 'mist' : kind) }), text]; }
  function healthSummary(p) {
    const fmt = CCAM.fmt;
    return () => {
      const g = p.global_health || { state: 'unknown' };
      const stat = p.static_availability || 'active';
      const state = stat === 'active' ? (g.state || 'unknown') : 'unknown';
      const rows = [];
      if (stat === 'active') {
        if (state === 'cooldown' && g.cooldown_until) { rows.push(['Retry in', fmt.untilShort(g.cooldown_until)]); rows.push(['Backoff level', String(g.backoff_level || 0)]); }
        if (g.consecutive_failures) rows.push(['Consecutive failures', String(g.consecutive_failures)]);
        rows.push(['Active sessions', String(p.active_session_count !== undefined ? p.active_session_count : (p.sessions || []).length)]);
        rows.push(['Last success', g.last_success_at ? fmt.relative(g.last_success_at) : 'never']);
        rows.push(['Last failure', g.last_failure_at ? fmt.relative(g.last_failure_at) : 'none']);
        const chans = p.channels || [];
        const degraded = chans.filter(c => c.state === 'degraded').length, cooling = chans.filter(c => c.state === 'cooldown').length;
        if (degraded || cooling) rows.push(['Channels', chans.length + ' · ' + [degraded ? degraded + ' degraded' : '', cooling ? cooling + ' cooling' : ''].filter(Boolean).join(' · ')]);
        if ((state === 'degraded' || state === 'cooldown') && g.last_error) rows.push(['Last error', clip(g.last_error), 'mono']);
      }
      const kind = stat !== 'active' ? (stat === 'no_models' ? 'warn' : 'mist') : (U.HEALTH_KIND[state] || 'mist');
      const title = stat !== 'active' ? (stat === 'no_models' ? 'No models' : 'Disabled') : (U.HEALTH_TEXT[state] || fmt.cap(fmt.words(state)));
      return U.tipBlock({ title: stateTitle(kind, title), rows, note: stat !== 'active' ? STATIC_NOTE[stat] : STATE_NOTE[state] });
    };
  }
  function channelSummary(c) {
    const fmt = CCAM.fmt;
    return () => {
      const rows = [];
      if (c.state === 'cooldown' && c.cooldown_until) { rows.push(['Retry in', fmt.untilShort(c.cooldown_until)]); rows.push(['Backoff level', String(c.backoff_level || 0)]); }
      rows.push(['Consecutive failures', String(c.consecutive_failures || 0)]);
      rows.push(['Observed failures', String(c.observed_failures || 0)]);
      rows.push(['Last success', c.last_success_at ? fmt.relative(c.last_success_at) : 'never']);
      rows.push(['Last failure', c.last_failure_at ? fmt.relative(c.last_failure_at) : 'none']);
      if (c.state !== 'healthy' && c.last_error) rows.push(['Last error', clip(c.last_error), 'mono']);
      const kind = U.HEALTH_KIND[c.state] || 'mist';
      return U.tipBlock({ title: stateTitle(kind, (U.HEALTH_TEXT[c.state] || c.state) + ' · ' + c.model + ' / ' + c.request_type), rows, note: STATE_NOTE[c.state] });
    };
  }
  function priorityTip(priority) {
    return 'Priority ' + CCAM.fmt.priorityLabel(priority) + ' — higher tiers are tried first; providers sharing a priority round-robin, and a session stays on its provider.';
  }

  Object.assign(CCAM.ui, { patchSelector, tlsControl, authHeaderSeg, healthSummary, channelSummary, priorityTip });
})();
