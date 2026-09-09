/* Claude Code harness: settings file, telemetry, model-mapping profiles. */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  const { h, icon, replace, pill, tag, field, input, seg, switchCtl, dialog, confirm, skeleton, errorCard, toast, empty, banner, tip, tipBlock, wrapTip } = CCAM.ui;
  const fmt = CCAM.fmt;
  const store = CCAM.store;
  const api = CCAM.api;
  CCAM.pages = CCAM.pages || {};

  const STATE = {
    in_sync: { text: 'In sync', kind: 'ok', desc: 'settings.json matches the active profile; every managed field was verified on the last read.' },
    out_of_sync: { text: 'Out of sync', kind: 'warn', desc: 'A managed field was changed outside CC AutoMux. The active profile was cleared; activate a profile to rewrite the managed fields.' },
    inactive: { text: 'No active profile', kind: 'mist', desc: 'CC AutoMux is not writing to this file. Activate a profile to point Claude Code at the gateway.' },
    state_error: { text: 'Check failed', kind: 'bad', desc: 'The settings file could not be read or the active state could not be persisted. The gateway keeps running.' }
  };
  const MANAGED = [
    ['ANTHROPIC_BASE_URL', 'gateway address'], ['ANTHROPIC_AUTH_TOKEN', 'gateway key'],
    ['ANTHROPIC_DEFAULT_HAIKU_MODEL', 'haiku mapping'], ['ANTHROPIC_DEFAULT_SONNET_MODEL', 'sonnet mapping'],
    ['ANTHROPIC_DEFAULT_OPUS_MODEL', 'opus mapping'], ['ANTHROPIC_DEFAULT_FABLE_MODEL', 'fable mapping'],
    ['CLAUDE_CODE_ATTRIBUTION_HEADER', 'telemetry'], ['DISABLE_FEEDBACK_COMMAND', 'telemetry'],
    ['DISABLE_ERROR_REPORTING', 'telemetry'], ['DISABLE_TELEMETRY', 'telemetry']
  ];
  const TELE_FIELDS = [
    ['CLAUDE_CODE_ATTRIBUTION_HEADER', '0', 'no client-type attribution text → better prompt cache hits'],
    ['DISABLE_FEEDBACK_COMMAND', '1', 'hides the /feedback command'],
    ['DISABLE_ERROR_REPORTING', '1', 'no error reports'],
    ['DISABLE_TELEMETRY', '1', 'no telemetry collection']
  ];
  const MODEL_FIELDS = [
    ['haiku_model', 'Haiku', 'ANTHROPIC_DEFAULT_HAIKU_MODEL', true], ['sonnet_model', 'Sonnet', 'ANTHROPIC_DEFAULT_SONNET_MODEL', true],
    ['opus_model', 'Opus', 'ANTHROPIC_DEFAULT_OPUS_MODEL', true], ['fable_model', 'Fable', 'ANTHROPIC_DEFAULT_FABLE_MODEL', true],
    ['subagent_model', 'Subagent', 'CLAUDE_CODE_SUBAGENT_MODEL', false], ['teammate_default_model', 'Teammate', 'teammateDefaultModel', false]
  ];

  function profileDialog(profile, models) {
    const draft = Object.assign({ name: '', haiku_model: '', sonnet_model: '', opus_model: '', fable_model: '', subagent_model: '', teammate_default_model: '' }, profile || {});
    const isNew = !profile;
    const fields = {};
    const list = h('datalist', { id: 'cc-models' }, models.map(m => h('option', { value: m })));
    fields.name = field({ label: 'Profile name', for: 'pf-name', control: input({ id: 'pf-name', sans: true, value: draft.name, placeholder: 'e.g. Daily', oninput: (e) => { draft.name = e.target.value; } }) });
    const mf = MODEL_FIELDS.map(([key, label, env, required]) => {
      const f = field({ label: label, optional: !required, for: 'pf-' + key, control: input({ id: 'pf-' + key, value: draft[key], list: 'cc-models', placeholder: required ? 'model name' : 'leave unset', oninput: (e) => { draft[key] = e.target.value; } }), help: h('code', null, env) });
      fields[key] = f; return f;
    });
    const form = h('div', { class: 'profile-form' }, list, fields.name, h('div', { class: 'row-2' }, mf[0], mf[1]), h('div', { class: 'row-2' }, mf[2], mf[3]),
      h('p', { class: 'help', style: { margin: '4px 0 14px' } }, 'Subagent model, when set, forces every subagent, agent-team and workflow agent to one model; the teammate default only applies to teammates without an explicit model and is overridden by the subagent model.'),
      h('div', { class: 'row-2' }, mf[4], mf[5]));
    return dialog({ title: isNew ? 'New profile' : 'Edit ' + profile.name, body: form, wide: true, focus: '#pf-name', actions: [
      { label: 'Cancel', value: null },
      { label: isNew ? 'Create profile' : 'Save profile', primary: true, onClick: async () => {
        Object.values(fields).forEach(f => f.setError(''));
        let bad = false;
        if (!draft.name.trim()) { fields.name.setError('Give the profile a name.'); bad = true; }
        MODEL_FIELDS.filter(m => m[3]).forEach(([key, label]) => { if (!draft[key].trim()) { fields[key].setError(label + ' mapping is required.'); bad = true; } });
        if (bad) return false;
        try {
          const body = { name: draft.name, haiku_model: draft.haiku_model, sonnet_model: draft.sonnet_model, opus_model: draft.opus_model, fable_model: draft.fable_model, subagent_model: draft.subagent_model, teammate_default_model: draft.teammate_default_model };
          if (isNew) return await api.post('/api/v1/harnesses/claude-code/profiles', body);
          return await api.put('/api/v1/harnesses/claude-code/profiles/' + profile.id, Object.assign({ id: profile.id }, body));
        } catch (e) {
          if (e.status === 409 && /name/.test(e.message)) fields.name.setError('Another profile already uses this name.');
          else if (e.field && fields[e.field.replace(/^profile\./, '')]) fields[e.field.replace(/^profile\./, '')].setError(e.detail);
          else fields.name.setError(e.detail || e.message);
          return false;
        }
      } }
    ] });
  }

  CCAM.pages['claude-code'] = {
    render(ctx) {
      ctx.title('Claude Code');
      ctx.subtitle(['Point Claude Code at the gateway and switch ', h('b', null, 'model-mapping profiles'), ' without editing settings.json by hand']);
      const host = h('div', null, skeleton(4, 60));
      ctx.root.appendChild(host);
      let disposed = false, generation = 0;
      let pathDraft = null, pathBaseline = null;
      const pathDirty = () => pathDraft && !store.same(pathDraft, pathBaseline);
      CCAM.router.setGuard(async () => pathDirty() ? confirm({ title: 'Discard changes?', text: 'The settings path has unsaved changes.', confirmLabel: 'Discard', danger: true }) : true);

      async function load(quiet) {
        const gen = ++generation;
        try {
          const [harness, profiles, providers] = await Promise.all([store.harness(), store.profiles(), store.providers()]);
          if (disposed || gen !== generation) return;
          if (!pathDirty()) {
            pathBaseline = { mode: harness.path_mode, value: harness.settings_path || '' };
            pathDraft = Object.assign({}, pathBaseline);
          }
          const active = host.contains(document.activeElement) ? document.activeElement : null;
          const focus = active && { id: active.id, start: active.selectionStart, end: active.selectionEnd };
          render(harness, profiles, providers);
          if (focus && focus.id) {
            const next = document.getElementById(focus.id);
            if (next) { next.focus(); if (focus.start !== null && next.setSelectionRange) next.setSelectionRange(focus.start, focus.end); }
          }
        } catch (e) { if (!disposed && gen === generation && !quiet) replace(host, errorCard(e.detail || e.message, load)); }
      }

      function render(harness, profiles, providers) {
        const models = Array.from(new Set(providers.flatMap(p => p.models))).sort();
        const status = store.state.status;
        const gatewayOk = status ? status.gateway_configured : true;
        const st = STATE[harness.state] || { text: harness.state, kind: 'mist', desc: '' };
        ctx.actions([h('button', { class: 'btn primary', type: 'button', onclick: async () => { const r = await profileDialog(null, models); if (r) { toast('Profile ' + r.name + ' created'); store.invalidate(); load(); } } }, icon('plus'), 'New profile')]);

        // ---- settings file card ----
        let pathMode = pathDraft.mode, pathValue = pathDraft.value;
        const pathIn = input({ id: 'cc-path', value: pathValue, placeholder: '/absolute/path/to/settings.json', oninput: (e) => { pathValue = pathDraft.value = e.target.value; applyBtn.hidden = !pathDirty(); } });
        const applyBtn = h('button', { class: 'btn primary', type: 'button', hidden: !pathDirty(), onclick: async () => {
          try {
            await api.put('/api/v1/harnesses/claude-code', { path_mode: pathMode, settings_path: pathMode === 'custom' ? pathValue : '' });
            pathBaseline = null; pathDraft = null; toast('Settings path updated'); store.invalidate(); load();
          } catch (e) { pathField.setError(e.detail || e.message); }
        } }, 'Apply path');
        const pathRow = h('div', { class: 'cc-path-row', hidden: pathMode !== 'custom' }, pathIn, applyBtn);
        const pathSeg = seg({ ariaLabel: 'Settings path', value: pathMode, options: [{ value: 'default', label: 'Default location' }, { value: 'custom', label: 'Custom path' }], onchange: (v) => {
          pathMode = pathDraft.mode = v; if (v === 'default') pathDraft.value = pathValue = ''; pathRow.hidden = v !== 'custom';
          const changed = pathDirty();
          applyBtn.hidden = !changed;
          if (v === 'default' && changed) { pathRow.hidden = false; pathIn.hidden = true; } else pathIn.hidden = false;
        } });
        if (pathMode === 'default' && pathDirty()) { pathRow.hidden = false; pathIn.hidden = true; }
        const pathField = field({ label: 'Settings file', control: [pathSeg, h('div', { style: { height: '10px' } }), pathRow],
          help: h('div', { class: 'cc-path' }, 'Resolves to ', h('code', null, harness.resolved_settings_path)) });
        const managed = h('details', { class: 'disc' }, h('summary', null, icon('chevron-right'), 'Managed fields'), h('div', { class: 'disc-body' },
          h('div', { class: 'cc-managed' }, MANAGED.map(([k, what]) => h('span', null, h('b', null, 'env.' + k), ' · ' + what)), h('span', { class: 'opt' }, h('b', null, 'env.CLAUDE_CODE_SUBAGENT_MODEL'), ' · optional'), h('span', { class: 'opt' }, h('b', null, 'teammateDefaultModel'), ' · optional, top level')),
          h('p', { class: 'help' }, 'Only these fields are written. Everything else in the file is preserved byte-for-byte in meaning. Before the first write to an existing file a one-time copy is saved as ', h('code', null, '.cc-automux.bak'), ' next to it.')));

        const teleSw = switchCtl({ checked: harness.disable_telemetry, label: 'Disable Claude Code telemetry', onchange: async (v, inp) => {
          inp.disabled = true;
          try { await api.put('/api/v1/harnesses/claude-code', { disable_telemetry: v }); toast(v ? 'Telemetry will be disabled on the next activation' : 'Telemetry allowed on the next activation'); store.invalidate(); load(); }
          catch (e) { inp.checked = !v; inp.disabled = false; toast('Could not update telemetry: ' + (e.detail || e.message), 'bad'); }
        } });
        teleSw.setAttribute('data-tip', TELE_FIELDS.map(f => f[0] + ' = "' + f[1] + '"  —  ' + f[2]).join('\n'));
        const teleBlock = h('div', { class: 'cc-tele' }, h('div', null, teleSw, h('div', { class: 'cc-tele-fields' }, TELE_FIELDS.flatMap(([k, v, d]) => [h('code', null, k + ' = "' + v + '"'), h('span', null, d)]))));

        const settingsCard = h('div', { class: 'card' },
          h('div', { class: 'card-head' }, h('p', { class: 'eyebrow' }, 'Settings file'), tip(pill(st.text, st.kind), () => {
            const active = profiles.find(x => x.active);
            return tipBlock({ title: st.text, rows: [['Settings file', harness.resolved_settings_path, 'mono'], ['Active profile', active ? active.name : 'none'], ['Telemetry', harness.disable_telemetry ? 'disabled on activation' : 'allowed']], note: harness.last_invalidation_reason || st.desc });
          })),
          h('div', { class: 'cc-state' }, h('span', { class: 'reason' }, harness.last_invalidation_reason ? harness.last_invalidation_reason : st.desc)),
          pathField, h('div', { style: { height: '14px' } }), managed,
          h('hr', { class: 'hr' }),
          h('p', { class: 'eyebrow' }, 'Telemetry'),
          h('p', { class: 'lede' }, 'Written together with the model mapping on every activation. Hover the switch to see the exact fields.'),
          teleBlock);

        // ---- profiles ----
        const banners = h('div', { class: 'banners' });
        if (!gatewayOk) banners.appendChild(banner('warn', 'Gateway key required', 'Activation writes the gateway key into settings.json as ANTHROPIC_AUTH_TOKEN, so a profile cannot be activated until the key is set.', [h('a', { class: 'btn sm', href: '#/service' }, 'Set gateway key')]));
        if (harness.state === 'out_of_sync' || harness.state === 'state_error') banners.appendChild(banner(harness.state === 'state_error' ? 'bad' : 'warn', st.text, harness.last_invalidation_reason || st.desc));

        const rows = profiles.length ? h('div', { class: 'rows' }, profiles.map(p => profileRow(p))) : empty({ title: 'No profiles yet', text: 'A profile names the models Claude Code should use for Haiku, Sonnet, Opus and Fable. Activating one writes those names, the gateway address and key into settings.json.', action: h('button', { class: 'btn primary', type: 'button', onclick: async () => { const r = await profileDialog(null, models); if (r) { store.invalidate(); load(); } } }, icon('plus'), 'New profile') });
        function profileRow(p) {
          const isActive = harness.state === 'in_sync' && p.active;
          const map = h('div', { class: 'p-map' }, MODEL_FIELDS.map(([key, label, env, required]) => (required || p[key]) ? h('div', { class: 'mp' + (required ? '' : ' opt'), 'data-tip': env + ' = ' + p[key] + '\n' + (required ? 'Written into settings.json while this profile is active.' : 'Optional override, written only when set.') }, h('span', { class: 'k' }, label), h('span', { class: 'v' }, p[key])) : null));
          const activate = h('button', { class: 'btn sm' + (isActive ? '' : ' primary'), type: 'button', disabled: isActive || !gatewayOk, onclick: async () => {
            activate.disabled = true; replace(activate, h('span', { class: 'spin' }), 'Activating…');
            try {
              const r = await api.post('/api/v1/harnesses/claude-code/profiles/' + p.id + '/activate', {});
              toast(r.idempotent ? p.name + ' was already active' : 'Activated ' + p.name + ' · settings.json updated'); store.invalidate(); load();
            } catch (e) {
              const msg = e.code === 'gateway_not_configured' ? 'Set a gateway key before activating a profile.' : e.code === 'harness_config_io_failed' ? 'settings.json could not be written: ' + (e.detail || '') : e.code === 'restart_in_progress' ? 'CC AutoMux is restarting — try again shortly.' : (e.detail || e.message);
              toast('Activation failed: ' + msg, 'bad'); load();
            }
          } }, isActive ? [icon('check'), 'Active'] : [icon('play'), 'Activate']);
          return h('div', { class: 'profile' + (isActive ? ' active' : '') },
            h('div', null, h('div', { class: 'p-name' }, p.name, isActive ? tip(pill('Active · verified', 'ok'), 'settings.json currently contains exactly this mapping; it is re-checked on load and when the window regains focus.') : null), h('div', { class: 'p-sub' }, (p.subagent_model || p.teammate_default_model) ? 'with optional overrides' : 'four required mappings')),
            map,
            h('div', { class: 'p-acts' },
              wrapTip(activate, !gatewayOk ? 'Set a gateway key before activating a profile.' : isActive ? 'Active — settings.json matches this profile.' : 'Writes this mapping, the gateway address and key into settings.json.'),
              h('button', { class: 'ib bordered', type: 'button', 'aria-label': 'Edit ' + p.name, 'data-tip': 'Edit profile', onclick: async () => { const r = await profileDialog(p, models); if (r) { toast('Saved ' + r.name); store.invalidate(); load(); } } }, icon('edit')),
              wrapTip(h('button', { class: 'ib bordered danger', type: 'button', 'aria-label': 'Delete ' + p.name, disabled: isActive, onclick: async () => {
                const ok = await confirm({ title: 'Delete ' + p.name + '?', text: 'The mapping is removed from CC AutoMux. settings.json is not touched.', confirmLabel: 'Delete', danger: true });
                if (!ok) return;
                try { await api.del('/api/v1/harnesses/claude-code/profiles/' + p.id); toast(p.name + ' deleted'); store.invalidate(); load(); } catch (e) { toast('Delete failed: ' + (e.detail || e.message), 'bad'); }
              } }, icon('trash')), isActive ? 'Switch profiles before deleting the active one.' : 'Delete profile')));
        }
        const profilesCard = h('div', { class: 'card' },
          h('div', { class: 'card-head' }, h('div', null, h('p', { class: 'eyebrow' }, 'Model mapping profiles'), h('p', { class: 'lede', style: { margin: 0 } }, 'Claude Code only knows Haiku, Sonnet, Opus and Fable. A profile maps each to a model your providers serve; the gateway then routes by that name.')), pill(fmt.plural(profiles.length, 'profile'), 'mist', 'plain')),
          banners, rows);

        replace(host, h('div', { class: 'stack' }, settingsCard, profilesCard));
      }

      load();
      const offFg = store.on('foreground', () => load(true));
      const offInv = store.on('invalidate', () => load(true));
      const offStatus = store.on('status', () => { /* gateway_configured changes re-render on next load */ });
      return () => { disposed = true; offFg(); offInv(); offStatus(); };
    }
  };
})();
