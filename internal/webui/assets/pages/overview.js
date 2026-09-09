/* Overview page: banners, stats, routing map, summaries. */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  const { h, icon, replace, clear, pill, healthPill, healthDot, tag, banner, kv, skeleton, errorCard, tip, healthSummary, priorityTip } = CCAM.ui;
  const fmt = CCAM.fmt;
  const store = CCAM.store;
  CCAM.pages = CCAM.pages || {};

  const PROTOCOL_LABEL = { anthropic_messages: 'Anthropic Messages', openai_responses: 'OpenAI Responses', openai_compatible: 'OpenAI-compatible' };
  const SVG_NS = 'http://www.w3.org/2000/svg';

  function statusBanners(status, harness) {
    const out = [];
    if (!status) return out;
    if (status.logging && !status.logging.healthy) {
      out.push(banner('bad', 'Log writes are failing', ['CC AutoMux keeps serving, but nothing has been written to its log since ', h('b', null, fmt.dateTime(status.logging.last_failure_at)), ' (', fmt.plural(status.logging.failures, 'failure'), '). ', h('span', { class: 'mono' }, status.logging.last_error || '')],
        [h('a', { class: 'btn sm', href: '#/logs' }, 'Open logs')]));
    }
    if (status.restart && status.restart.in_progress) out.push(banner('warn', 'Restart in progress', 'A configuration change is being applied. In-flight requests were interrupted; the new process promotes the pending configuration once it binds.'));
    else if (status.restart && status.restart.state === 'failed' && status.restart.last_error) out.push(banner('warn', 'Last restart failed', ['The previous configuration and listener were restored. ', h('span', { class: 'mono' }, status.restart.last_error)], [h('a', { class: 'btn sm', href: '#/service' }, 'Service')]));
    if (!status.gateway_configured) out.push(banner('warn', 'Gateway key is not set', 'The Messages data plane returns 503 until a gateway key exists. Claude Code cannot route through CC AutoMux yet.', [h('a', { class: 'btn sm', href: '#/service' }, 'Set gateway key')]));
    if (harness && harness.state === 'out_of_sync') out.push(banner('warn', 'Claude Code settings drifted', ['A managed field in ', h('span', { class: 'mono' }, harness.resolved_settings_path), ' no longer matches the profile that was active. ', harness.last_invalidation_reason], [h('a', { class: 'btn sm', href: '#/claude-code' }, 'Review')]));
    if (harness && harness.state === 'state_error') out.push(banner('bad', 'Claude Code settings could not be checked', harness.last_invalidation_reason || 'The settings file could not be read.', [h('a', { class: 'btn sm', href: '#/claude-code' }, 'Review')]));
    return out;
  }

  function statsCard(status) {
    const g = status.global_health, c = status.channel_health;
    const capSeg = (text, dot) => h('span', null, dot ? h('i', { class: 'dot ' + dot }) : null, text);
    const stat = (cls, ico, label, explain, value, small, caption) => h('div', { class: 'stat' + (cls ? ' ' + cls : '') },
      h('div', { class: 's-h', 'data-tip': explain }, h('span', { class: 's-i' }, icon(ico)), h('span', { class: 's-l' }, label)),
      h('div', { class: 's-v' }, String(value), small ? h('small', null, small) : null),
      h('div', { class: 's-c' }, caption));
    return h('div', { class: 'card stats' },
      stat('', 'providers', 'Providers', 'Active providers are enabled and declare at least one model. Inactive ones are disabled or have no models and never receive traffic.',
        status.active_provider_count, '/ ' + status.provider_count, capSeg(status.enabled_provider_count + ' enabled · ' + status.inactive_provider_count + ' inactive')),
      stat('em', 'pulse', 'Healthy', 'Global health of the active providers. Degraded providers still receive requests; cooling providers are skipped until their retry time.',
        g.healthy, 'of ' + status.active_provider_count + ' active', [capSeg(c.degraded + ' degraded', 'warn'), capSeg((g.cooldown + c.cooldown) + ' cooling', 'bad'), capSeg(g.disabled + ' health off', 'mist')]),
      stat('amber', 'bolt', 'In flight', 'Messages requests being served right now, including retries and failovers still in progress.',
        status.active_data_requests, null, capSeg('Messages requests right now')),
      stat('iris', 'sessions', 'Sticky sessions', "Sessions currently pinned to a provider. A pin lasts one hour after the session's last request, so a conversation keeps its upstream.",
        status.sticky_assignment_count, null, capSeg('pinned to a provider · 1h sliding'))
    );
  }

  // ---- routing map ----
  function routingMap(config, healthList) {
    const byId = {}; (healthList || []).forEach(p => { byId[p.id] = p; });
    const providers = config.providers.slice().sort((a, b) => b.priority - a.priority);
    const tiers = [];
    providers.forEach(p => { let t = tiers.find(x => x.priority === p.priority); if (!t) { t = { priority: p.priority, items: [] }; tiers.push(t); } t.items.push(p); });
    const auto = config.auto_mode;
    const candidates = new Set(auto.mode === 'provider_pool' ? providers.filter(p => p.enabled && p.models.includes(auto.model)).map(p => p.id) : []);

    const nodes = {};
    const client = h('div', { class: 'rn client' }, h('div', { class: 'rn-body' }, h('span', { class: 't' }, 'Claude Code'), h('span', { class: 's' }, 'ANTHROPIC_BASE_URL → gateway')));
    const core = h('div', { class: 'rn core' }, h('div', { class: 'rn-body' }, h('span', { class: 'nt' }, 'AUTOMUX CORE'), h('span', { class: 't' }, 'detect · route · patch · failover'), h('span', { class: 's' }, config.service.listen_addr)));
    let tapText, tapSub;
    if (auto.mode === 'provider_pool') { tapText = 'classifier pool'; tapSub = auto.model + ' · ' + fmt.plural(candidates.size, 'candidate'); }
    else if (auto.mode === 'fixed_provider') { tapText = 'fixed classifier target'; tapSub = auto.model + ' · ' + (PROTOCOL_LABEL[auto.fixed_provider.protocol] || auto.fixed_provider.protocol); }
    else { tapText = 'auto mode off'; tapSub = 'classifier requests → 503'; }
    const tap = h('div', { class: 'rn tap' + (auto.mode === 'disabled' ? ' off' : '') }, h('div', { class: 'rn-body' }, h('span', { class: 'nt' }, 'CLASSIFIER TAP'), h('span', { class: 't' }, tapText), h('span', { class: 's' }, tapSub)));
    nodes.client = client; nodes.core = core; nodes.tap = tap;
    tip(client, 'Claude Code sends every request to the gateway: ANTHROPIC_BASE_URL points at CC AutoMux and ANTHROPIC_AUTH_TOKEN carries the gateway key.');
    tip(core, "Detects the request type, picks the highest healthy priority tier, applies the provider's patches and fails over to the next provider on error.");
    tip(tap, auto.mode === 'provider_pool' ? 'Requests recognised as the Claude Code security-monitor classifier are rewritten to the classifier model and routed to the providers that declare it.'
      : auto.mode === 'fixed_provider' ? 'Classifier requests bypass the pool and go to the fixed target using the configured protocol.'
      : 'Auto mode is off: classifier requests are answered with 503 instead of being routed.');

    const right = h('div', { class: 'rmap-col right' });
    tiers.forEach((t, i) => {
      const label = 'PRIORITY ' + fmt.priorityLabel(t.priority);
      const hint = i === 0 ? 'tried first' : i === tiers.length - 1 && tiers.length > 1 ? 'last resort' : 'fallback';
      const tier = h('div', { class: 'tier' }, h('div', { class: 'tier-l' }, h('span', null, label), h('em', null, hint)));
      t.items.forEach(p => {
        const hh = byId[p.id];
        const state = hh ? hh.global_health.state : 'unknown';
        const stat = hh ? hh.static_availability : (p.enabled ? (p.models.length ? 'active' : 'no_models') : 'disabled_provider');
        const node = h('div', { class: 'rn prov' + (stat !== 'active' ? ' disabled' : '') + (state === 'cooldown' ? ' cooldown' : '') + (candidates.has(p.id) ? ' candidate' : '') },
          healthDot(stat === 'active' ? state : 'unknown'),
          h('div', { class: 'rn-body' }, h('span', { class: 't' }, p.name), h('span', { class: 's' }, fmt.host(p.base_url))),
          stat === 'disabled_provider' ? pill('Disabled', 'mist', 'plain') : stat === 'no_models' ? pill('No models', 'warn', 'plain') : state === 'cooldown' && hh.global_health.cooldown_until ? pill('retry ' + fmt.untilShort(hh.global_health.cooldown_until), 'bad', 'plain') : candidates.has(p.id) ? tip(pill('classifier', 'iris', 'plain'), 'Declares the classifier model, so auto-mode classifier requests can be routed here.') : null);
        node.dataset.tier = String(i); node.dataset.state = state; node.dataset.static = stat;
        nodes['p:' + p.id] = node;
        if (hh) tip(node, healthSummary(hh), { live: state === 'cooldown' });
        tier.appendChild(node);
      });
      right.appendChild(tier);
    });
    if (auto.mode === 'fixed_provider') {
      const f = auto.fixed_provider;
      const fixedNode = h('div', { class: 'rn fixed' }, h('span', { class: 'dot iris' }), h('div', { class: 'rn-body' }, h('span', { class: 't' }, 'Fixed classifier target'), h('span', { class: 's' }, fmt.host(f.base_url) + ' · ' + f.protocol)), pill('classifier', 'iris', 'plain'));
      nodes.fixed = fixedNode;
      tip(fixedNode, 'Classifier requests go to this target regardless of pool health. It is not part of the Messages routing pool.');
      right.appendChild(h('div', { class: 'tier' }, h('div', { class: 'tier-l' }, h('span', null, 'OUTSIDE THE POOL'), h('em', null, 'classifier only')), fixedNode));
    }
    if (!providers.length) right.appendChild(h('div', { class: 'tier' }, h('div', { class: 'tier-l' }, h('span', null, 'NO PROVIDERS')), h('a', { class: 'rn prov disabled', href: '#/providers/new' }, h('div', { class: 'rn-body' }, h('span', { class: 't' }, 'Add a provider'), h('span', { class: 's' }, 'Anthropic Messages API')))));

    const lines = document.createElementNS(SVG_NS, 'svg'); lines.setAttribute('class', 'rmap-lines'); lines.setAttribute('aria-hidden', 'true');
    const grid = h('div', { class: 'rmap-grid' }, h('div', { class: 'rmap-col left' }, client, core, tap), right);
    const wrap = h('div', { class: 'rmap' }, lines, grid);

    function rel(el) { const r = wrap.getBoundingClientRect(); const b = el.getBoundingClientRect(); return { left: b.left - r.left, right: b.right - r.left, top: b.top - r.top, bottom: b.bottom - r.top, cx: b.left - r.left + b.width / 2, cy: b.top - r.top + b.height / 2 }; }
    function seg(d, cls, color) {
      const el = document.createElementNS(SVG_NS, 'path');
      el.setAttribute('d', d); el.setAttribute('stroke', color); el.setAttribute('class', cls);
      lines.appendChild(el);
    }
    function curve(a, b, cls, color) {
      const x1 = a.right, y1 = a.cy, x2 = b.left, y2 = b.cy;
      const dx = Math.max(24, (x2 - x1) * 0.5);
      seg('M' + x1 + ' ' + y1 + ' C' + (x1 + dx) + ' ' + y1 + ', ' + (x2 - dx) + ' ' + y2 + ', ' + x2 + ' ' + y2, cls, color);
    }
    function draw() {
      if (!wrap.isConnected || window.innerWidth <= 900) return;
      while (lines.firstChild) lines.removeChild(lines.firstChild);
      const r = wrap.getBoundingClientRect(); lines.setAttribute('viewBox', '0 0 ' + r.width + ' ' + r.height);
      const C = rel(client), K = rel(core), T = rel(tap);
      seg('M' + C.cx + ' ' + C.bottom + ' L' + K.cx + ' ' + K.top, 'flow', 'var(--emerald)');
      seg('M' + K.cx + ' ' + K.bottom + ' L' + T.cx + ' ' + T.top, 'flow', 'var(--iris)');
      const provs = Object.keys(nodes).filter(k => k.startsWith('p:')).map(k => nodes[k]);
      if (provs.length) {
        // Orthogonal bus: a short stub leaves the core, a vertical trunk spans the provider column, one branch per provider.
        const first = rel(provs[0]);
        const trunkX = K.right + Math.max(18, (first.left - K.right) * 0.5);
        const ys = provs.map(p => rel(p).cy);
        const top = Math.min(K.cy, ...ys), bottom = Math.max(K.cy, ...ys);
        seg('M' + K.right + ' ' + K.cy + ' L' + trunkX + ' ' + K.cy, 'flow', 'var(--iris)');
        seg('M' + trunkX + ' ' + top + ' L' + trunkX + ' ' + bottom, 'trunk', 'var(--line-2)');
        provs.forEach(n => {
          const p = rel(n);
          const topTier = n.dataset.tier === '0';
          const cls = n.dataset.static !== 'active' ? 'muted' : n.dataset.state === 'cooldown' ? 'standby' : topTier ? 'flow' : 'standby';
          const color = n.dataset.static !== 'active' ? 'var(--line-2)' : n.dataset.state === 'cooldown' ? 'var(--coral)' : topTier ? 'var(--iris)' : 'var(--line-2)';
          seg('M' + trunkX + ' ' + p.cy + ' L' + p.left + ' ' + p.cy, cls, color);
          const dot = document.createElementNS(SVG_NS, 'circle');
          dot.setAttribute('cx', trunkX); dot.setAttribute('cy', p.cy); dot.setAttribute('r', '3'); dot.setAttribute('fill', color); dot.setAttribute('class', n.dataset.static !== 'active' ? 'muted' : '');
          lines.appendChild(dot);
        });
      }
      if (nodes.fixed) curve(T, rel(nodes.fixed), 'flow', 'var(--iris)');
    }
    const ro = new ResizeObserver(() => draw());
    ro.observe(wrap);
    requestAnimationFrame(draw);
    wrap.redraw = draw;
    wrap.dispose = () => ro.disconnect();
    return wrap;
  }

  CCAM.pages.overview = {
    render(ctx) {
      ctx.title('Overview');
      ctx.subtitle(['Gateway at a glance · ', h('b', null, 'multi-provider routing'), ' and ', h('b', null, 'auto mode'), ' for Claude Code']);
      const banners = h('div', { class: 'banners' });
      let disposed = false, mapGeneration = 0;
      const statsHost = h('div', null, skeleton(1, 96));
      const mapCard = h('div', { class: 'card' }, h('div', { class: 'card-head' }, h('div', null, h('p', { class: 'eyebrow' }, 'Live routing'), h('p', { class: 'lede', style: { margin: 0 } }, 'Messages traffic goes to the highest healthy tier; equal priorities round-robin; a session stays on its provider.')), h('a', { class: 'btn sm', href: '#/providers' }, 'Manage providers', icon('chevron-right'))), skeleton(3, 60));
      const autoCard = h('div', { class: 'card' }, h('p', { class: 'eyebrow' }, 'Auto mode'), skeleton(3, 20));
      const ccCard = h('div', { class: 'card' }, h('p', { class: 'eyebrow' }, 'Claude Code'), skeleton(3, 20));
      ctx.root.appendChild(h('div', { class: 'stack' }, banners, statsHost, mapCard, h('div', { class: 'ov-summary' }, autoCard, ccCard)));

      let map = null, harness = null, config = null;
      const renderBanners = () => replace(banners, statusBanners(store.state.status, harness));
      const renderStats = () => { if (store.state.status) replace(statsHost, statsCard(store.state.status)); };
      const renderAuto = () => {
        const st = store.state.status; if (!st || !config) return;
        const a = st.auto_mode;
        const body = [h('p', { class: 'eyebrow' }, 'Auto mode')];
        if (a.mode === 'disabled') body.push(h('div', { class: 'ov-big' }, 'Off', pill('classifier → 503', 'mist', 'plain')), h('p', { class: 'ov-line' }, 'Claude Code auto mode needs a classifier route. Turn on a provider pool or a fixed target.'));
        else if (a.mode === 'provider_pool') {
          const cands = config.providers.filter(p => p.enabled && p.models.includes(a.model)).sort((x, y) => y.priority - x.priority);
          body.push(h('div', { class: 'ov-big' }, 'Provider pool', h('span', { class: 'mono' }, a.model)));
          body.push(h('p', { class: 'ov-line' }, cands.length ? [h('b', null, fmt.plural(cands.length, 'provider')), ' declare the classifier model. Requests stick per session and use the classifier health channel.'] : [h('b', { style: { color: 'var(--amber)' } }, 'No enabled provider declares this model'), ' — classifier requests will get 404 model_not_configured.']));
          body.push(h('div', { class: 'ov-cands' }, cands.map(p => tip(tag(p.name + ' · P' + fmt.priorityLabel(p.priority), 'iris'), priorityTip(p.priority)))));
        } else {
          body.push(h('div', { class: 'ov-big' }, 'Fixed target', h('span', { class: 'mono' }, a.model)));
          body.push(h('p', { class: 'ov-line' }, 'Protocol ', h('b', null, PROTOCOL_LABEL[a.fixed_provider_protocol] || a.fixed_provider_protocol), a.fixed_provider_protocol !== 'anthropic_messages' ? pill('not implemented · 501', 'warn', 'plain') : null));
          const lc = a.fixed_target_last_call;
          body.push(h('p', { class: 'ov-line' }, 'Last call: ', lc ? [pill(lc.gateway_status >= 200 && lc.gateway_status < 300 ? 'ok ' + lc.gateway_status : lc.gateway_status + ' ' + (lc.gateway_error || ''), lc.gateway_status < 300 ? 'ok' : 'bad', 'plain'), h('span', null, fmt.relative(lc.observed_at))] : h('span', { class: 'faint' }, 'none yet')));
        }
        body.push(h('div', { class: 'ov-foot' }, h('span', { class: 'faint', style: { fontSize: '12px' } }, 'Classifier requests are detected by the Claude Code security-monitor prompt.'), h('a', { class: 'btn sm', href: '#/auto-mode' }, 'Configure', icon('chevron-right'))));
        replace(autoCard, body);
      };
      const renderCC = () => {
        const body = [h('p', { class: 'eyebrow' }, 'Claude Code')];
        if (!harness) { body.push(skeleton(3, 20)); replace(ccCard, body); return; }
        const stateKind = { in_sync: 'ok', out_of_sync: 'warn', inactive: 'mist', state_error: 'bad' }[harness.state] || 'mist';
        const stateText = { in_sync: 'In sync', out_of_sync: 'Out of sync', inactive: 'No active profile', state_error: 'Check failed' }[harness.state] || harness.state;
        const active = config && harness.active_profile_id ? config.harnesses.claude_code.profiles.find(p => p.id === harness.active_profile_id) : null;
        body.push(h('div', { class: 'ov-big' }, active ? active.name : 'No profile active', pill(stateText, stateKind)));
        body.push(h('p', { class: 'ov-line' }, h('span', { class: 'mono trunc', style: { display: 'block', maxWidth: '100%' }, 'data-tip': harness.resolved_settings_path }, harness.resolved_settings_path)));
        if (active) {
          const mapTag = (slot, env, model) => tip(tag(slot + ' → ' + model), env + ' = ' + model + '\nWritten into settings.json while this profile is active.');
          body.push(h('div', { class: 'ov-cands' }, mapTag('haiku', 'ANTHROPIC_DEFAULT_HAIKU_MODEL', active.haiku_model), mapTag('sonnet', 'ANTHROPIC_DEFAULT_SONNET_MODEL', active.sonnet_model), mapTag('opus', 'ANTHROPIC_DEFAULT_OPUS_MODEL', active.opus_model), mapTag('fable', 'ANTHROPIC_DEFAULT_FABLE_MODEL', active.fable_model)));
        }
        else body.push(h('p', { class: 'ov-line' }, harness.last_invalidation_reason || 'Activate a model-mapping profile to write the gateway address, key and model names into settings.json.'));
        body.push(h('div', { class: 'ov-foot' }, h('span', { class: 'faint', style: { fontSize: '12px' } }, 'Telemetry ' + (harness.disable_telemetry ? 'disabled' : 'allowed') + ' · ' + fmt.plural(harness.profile_count, 'profile')), h('a', { class: 'btn sm', href: '#/claude-code' }, 'Manage profiles', icon('chevron-right'))));
        replace(ccCard, body);
      };
      const loadMap = async () => {
        const gen = ++mapGeneration;
        try {
          const [cfg, hl] = await Promise.all([store.config(), store.providerHealth()]);
          if (disposed || gen !== mapGeneration) return;
          config = cfg;
          if (map) map.dispose();
          map = routingMap(cfg, hl.providers);
          replace(mapCard, mapCard.firstChild, map, h('div', { class: 'rmap-legend' }, h('span', null, h('i', { class: 'em' }), 'client traffic'), h('span', null, h('i'), 'active tier / classifier'), h('span', null, h('i', { class: 'st' }), 'standby tier'), h('span', null, h('i', { class: 'co' }), 'cooling down')));
          renderAuto();
          renderCC();
        } catch (e) { if (disposed || gen !== mapGeneration) return; replace(mapCard, mapCard.firstChild, errorCard(e.detail || e.message, loadMap)); }
      };
      const loadHarness = async () => { try { harness = await store.harness(); } catch (e) { harness = null; } if (!disposed) { renderBanners(); renderCC(); } };

      renderBanners(); renderStats();
      const offStatus = store.on('status', () => { renderBanners(); renderStats(); renderAuto(); });
      const offInvalidate = store.on('invalidate', () => { loadMap(); loadHarness(); });
      const offFg = store.on('foreground', loadHarness);
      loadMap(); loadHarness();
      // Cooldown countdowns on the map are re-rendered from fresh diagnostics.
      const tick = setInterval(loadMap, 30000);
      return () => { disposed = true; mapGeneration++; offStatus(); offInvalidate(); offFg(); clearInterval(tick); if (map) map.dispose(); };
    }
  };
})();
