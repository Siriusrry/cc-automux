(function(){
  // Scope to the classifier-destination control so the global enable toggle (also
  // a .seg, but aria-label="Global mode") is not swept into setMode below.
  const seg = document.querySelectorAll('.seg[aria-label="Classifier destination"] button');
  const cc = document.getElementById('cc');
  // Cross-column migration: the two column wrappers + the Service card node, plus
  // the two media queries that gate the move. narrowCols mirrors the .cols CSS
  // breakpoint (max-width:880px → single column); reduceMotion gates the FLIP glide.
  const colLeft = document.getElementById('colLeft');
  const colRight = document.getElementById('colRight');
  const serviceCard = document.getElementById('serviceCard');
  const narrowCols = window.matchMedia('(max-width: 880px)');
  const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)');
  const entranceList = document.getElementById('entranceList');
  const acctList = document.getElementById('acctList');
  const acctErr = document.getElementById('acctErr');
  const savebar = document.getElementById('savebar');
  const enableSeg = document.getElementById('enableSeg');
  const liveChip = document.getElementById('liveChip');
  const uptimeVal = document.getElementById('uptimeVal');
  const rewritesVal = document.getElementById('rewritesVal');
  const clsNodeTitle = document.getElementById('clsNodeTitle');
  const clsNodeSub = document.getElementById('clsNodeSub');
  // Cached GET /admin/status so a form re-render (load/save/revert) can re-apply the
  // last-known entrance/account health to the rebuilt DOM at once, before the next
  // poll. uptimeBase/uptimeAt anchor the local per-second uptime ticker.
  let lastStatus = null, uptimeBase = null, uptimeAt = 0;

  // Dirty tracking: the save bar only appears once the form diverges from the
  // last-loaded config. `baseline` is the serialized clean state; null until the
  // first load completes (so edits during load don't flash the bar).
  let baseline = null;
  function snapshot(){ return JSON.stringify(collect()); }
  function checkDirty(){ savebar.hidden = baseline === null || snapshot() === baseline; }

  function setMode(mode){
    seg.forEach(x => x.setAttribute('aria-pressed', x.dataset.mode === mode ? 'true' : 'false'));
    cc.hidden = mode !== 'custom';
    // mirror this destination toggle in the hero schematic's classifier node —
    // "Single target" sends every classifier request to one global upstream; "Keep per
    // provider" leaves each provider on its own default. Driven from setMode so it
    // updates on both the toggle click AND on render (render → setMode), so a config
    // that loads in Single shows the right text. fitText squeezes the longer label to
    // the 216-unit box room (same as the host boxes) rather than letting it spill.
    if (mode === 'custom'){
      clsNodeTitle.textContent = 'classifier → single target';
      clsNodeSub.textContent = 'one global upstream';
    } else {
      clsNodeTitle.textContent = 'classifier → default';
      clsNodeSub.textContent = 'each provider keeps its own';
    }
    fitText(clsNodeTitle, HOST_TEXT_WIDTH);
    fitText(clsNodeSub, HOST_TEXT_WIDTH);
  }

  // Service-card cross-column migration. In "Single target" mode the
  // classifier card grows long and crowds the right column, so the Service card
  // migrates to the LEFT column beneath AnyRouter to rebalance; "Keep per provider"
  // sends it back to its home beneath Classifier in the right column. The column
  // widths are deliberately left as-is (1.05fr/.95fr — the chosen layout), so the card simply
  // takes its destination column's width. The move is a FLIP glide on a user toggle,
  // and instant (reparent only) on initial render and on breakpoint crossings.
  // serviceTargetCol resolves the home: LEFT only in custom mode AND a wide layout;
  // at/below the single-column breakpoint the move is a no-op so the stacked reading
  // order stays AnyRouter → CPA → Classifier → Service.
  function serviceTargetCol(){
    const custom = document.querySelector('.seg button[data-mode="custom"]').getAttribute('aria-pressed') === 'true';
    return (custom && !narrowCols.matches) ? colLeft : colRight;
  }
  let flipEnd = null;
  function placeServiceCard(animate){
    const dest = serviceTargetCol();
    if (serviceCard.parentNode === dest) return;
    // Reparent only — no animation — on initial paint, reduced-motion, and resize.
    // Drop any in-flight FLIP and clear its inline styles so nothing is left inverted.
    if (!animate || reduceMotion.matches){
      if (flipEnd){ serviceCard.removeEventListener('transitionend', flipEnd); flipEnd = null; }
      serviceCard.style.transition = serviceCard.style.transform = serviceCard.style.position = serviceCard.style.zIndex = '';
      dest.appendChild(serviceCard);
      return;
    }
    // FLIP. First: current rect (includes any in-flight transform → smooth chaining
    // on a rapid re-toggle). Last: rect after the reparent, measured with transforms
    // cleared. Invert: translate the card back to First. Play: transition to identity.
    if (flipEnd){ serviceCard.removeEventListener('transitionend', flipEnd); flipEnd = null; }
    const first = serviceCard.getBoundingClientRect();
    serviceCard.style.transition = 'none';
    serviceCard.style.transform = serviceCard.style.position = serviceCard.style.zIndex = '';
    dest.appendChild(serviceCard);
    const last = serviceCard.getBoundingClientRect();
    const dx = first.left - last.left, dy = first.top - last.top;
    if (dx === 0 && dy === 0){ serviceCard.style.transition = ''; return; }
    serviceCard.style.transform = 'translate(' + dx + 'px,' + dy + 'px)';
    // position:relative (no offset → no layout shift) lets z-index lift the gliding
    // card above the other column's cards while it crosses, so it never slides under them.
    serviceCard.style.position = 'relative';
    serviceCard.style.zIndex = '5';
    void serviceCard.offsetWidth; // commit the inverted position before transitioning
    serviceCard.style.transition = 'transform .42s cubic-bezier(.22,.61,.36,1)';
    serviceCard.style.transform = '';
    flipEnd = function(e){
      if (e.target !== serviceCard || e.propertyName !== 'transform') return;
      serviceCard.style.transition = serviceCard.style.transform = serviceCard.style.position = serviceCard.style.zIndex = '';
      serviceCard.removeEventListener('transitionend', flipEnd);
      flipEnd = null;
    };
    serviceCard.addEventListener('transitionend', flipEnd);
  }
  seg.forEach(b => b.addEventListener('click', () => { setMode(b.dataset.mode); placeServiceCard(true); }));
  // A resize across the single-column breakpoint re-homes the card instantly so the
  // layout/reading order stays correct (e.g. wide+custom → narrow returns it right).
  narrowCols.addEventListener('change', () => placeServiceCard(false));

  // Skip TLS verify and a custom CA are mutually exclusive (the backend 400s if both
  // are set). When skip-verify is checked, grey out + disable the CA input as a visual
  // signal; collect() also stops sending the CA path, so the UI can never build the
  // conflict. State-only (no value loss): unchecking restores the CA input as typed.
  const clsInsecure = document.getElementById('clsInsecure');
  const clsCa = document.getElementById('clsCa');
  function syncInsecure(){
    clsCa.disabled = clsInsecure.checked;
    clsCa.style.opacity = clsInsecure.checked ? '.5' : '';
  }
  clsInsecure.addEventListener('change', syncInsecure);

  // Global enable switch (Active / Pass-through). Off = passthrough: the shim
  // bypasses rotation + every classifier rewrite/fix + reassembly + shim-owned auth
  // (that bypass is server-side, disabled-mode handling). Here the toggle only drives its own pressed
  // state, the schematic dim, and the `enabled` field in the saved config — applied
  // like any other edit through the save bar. nil/missing enabled ⇒ on (default true).
  function applyEnableSchematic(on){
    document.querySelectorAll('.clsfx').forEach(el => { el.style.opacity = on ? '' : '.22'; });
  }
  function setEnabled(on){
    enableSeg.querySelectorAll('button').forEach(x =>
      x.setAttribute('aria-pressed', (x.dataset.enabled === 'true') === on ? 'true' : 'false'));
    applyEnableSchematic(on);
  }
  function enabledFromUI(){
    return enableSeg.querySelector('button[data-enabled="true"]').getAttribute('aria-pressed') === 'true';
  }
  enableSeg.querySelectorAll('button').forEach(b =>
    b.addEventListener('click', () => setEnabled(b.dataset.enabled === 'true')));

  function renumber(){
    entranceList.querySelectorAll('.entry .ord').forEach((o, i) => { o.textContent = i + 1; });
  }
  // suppress the browser's red spellcheck squiggle (and autofill / auto-cap /
  // auto-correct noise) on every text field. Applied once over the whole document on
  // load for the static inputs, and per-row inside addEntrance/addAccount so the
  // dynamically-created entrance/account inputs are covered too — no input is missed,
  // and the attributes are set before any value is assigned so the squiggle never shows.
  function killInputAssist(root){
    (root || document).querySelectorAll('input, textarea').forEach(el => {
      el.setAttribute('spellcheck', 'false');
      el.setAttribute('autocomplete', 'off');
      el.setAttribute('autocapitalize', 'off');
      el.setAttribute('autocorrect', 'off');
    });
  }
  function addEntrance(value){
    const el = document.createElement('div');
    el.className = 'entry';
    el.innerHTML = '<span class="ord"></span><input type="text" placeholder="https://…" /><button class="x" aria-label="Remove entrance">&times;</button>';
    killInputAssist(el);
    el.querySelector('input').value = value || '';
    entranceList.appendChild(el);
    renumber();
    return el;
  }

  function nextAcctLabel(){
    let max = 0;
    acctList.querySelectorAll('.acct .nm').forEach(n => {
      const m = /^acct-(\d+)$/.exec(n.value.trim());
      if (m) max = Math.max(max, parseInt(m[1], 10));
    });
    return 'acct-' + (max + 1);
  }
  // The account label is an editable input: its value feeds collect(), the health
  // pill match-by-label, and nextAcctLabel above. New rows default to the next free
  // acct-N; clearing the field is allowed — normalizeRuntimeConfig refills a blank
  // label whose key is set, and the uniqueness guard (markDuplicateLabels) blocks
  // saving two identical non-empty labels.
  function addAccount(label, key){
    const el = document.createElement('div');
    el.className = 'acct';
    el.innerHTML = '<div class="acct-top"><input class="nm" type="text" placeholder="label" aria-label="Account label" /><span class="pill acct-pill" hidden></span><span class="sid acct-sessions"></span><span class="sp"></span><button class="x" aria-label="Remove account">&times;</button></div><input class="key" type="text" placeholder="sk-…" />';
    killInputAssist(el);
    el.querySelector('.nm').value = label || nextAcctLabel();
    el.querySelector('.key').value = key || '';
    acctList.appendChild(el);
    return el;
  }

  // Uniqueness guard for account labels (labels key the /admin/status health
  // snapshot + UI pills, so duplicates would alias). markDuplicateLabels flags
  // every input sharing a non-empty trimmed label with .dup and returns the first
  // such label (else null). Blank labels are ignored — the backend auto-names a
  // blank label whose key is set. clearAcctErr removes the marks + inline message.
  function clearAcctErr(){
    acctErr.hidden = true;
    acctErr.textContent = '';
    acctList.querySelectorAll('.acct .nm.dup').forEach(n => n.classList.remove('dup'));
  }
  function markDuplicateLabels(){
    const groups = {};
    acctList.querySelectorAll('.acct .nm').forEach(nm => {
      const label = nm.value.trim();
      if (label) (groups[label] = groups[label] || []).push(nm);
    });
    let firstDup = null;
    Object.keys(groups).forEach(label => {
      if (groups[label].length > 1){
        if (firstDup === null) firstDup = label;
        groups[label].forEach(nm => nm.classList.add('dup'));
      }
    });
    return firstDup;
  }

  document.getElementById('addEntrance').addEventListener('click', () => addEntrance(''));
  entranceList.addEventListener('click', e => { const b = e.target.closest('.x'); if (b){ b.closest('.entry').remove(); renumber(); } });
  document.getElementById('addAcct').addEventListener('click', () => { addAccount(null, '').querySelector('.key').focus(); });
  acctList.addEventListener('click', e => { const b = e.target.closest('.x'); if (b){ b.closest('.acct').remove(); clearAcctErr(); } });
  // Editing a label clears stale duplicate marks/message; the guard re-runs on Save.
  acctList.addEventListener('input', e => { if (e.target.classList.contains('nm')) clearAcctErr(); });

  // Recompute dirty on any edit. Typing fires `input`; destination toggle and
  // add/remove of entrances/accounts fire `click` — defer those so the DOM
  // mutation from the original handler settles before we re-serialize.
  document.addEventListener('input', checkDirty);
  document.addEventListener('click', () => setTimeout(checkDirty, 0));

  function portOf(listen){ const i = (listen || '').lastIndexOf(':'); return i >= 0 ? listen.slice(i + 1) : ''; }
  function hostOf(u){ try { return new URL(u).host || u; } catch (_) { return u; } }

  // Each entrance/host rect is 256 user units wide with a 20-unit inset on each
  // side, leaving 216 units of text room. Long hosts (e.g. the 34-char FC entrance)
  // would otherwise spill past the rect; squeeze them to fit with textLength rather
  // than truncating, so every box stays the same size and the host stays fully legible.
  const HOST_TEXT_WIDTH = 216;
  // SHIM CORE box is 196 wide (x=300..496); its labels inset 22 on the left (x=322), so
  // mirror that on the right (max text right = 474 → 152 units) and squeeze the listen
  // line to fit rather than letting it spill past the rounded box.
  const CORE_TEXT_WIDTH = 152;
  function fitText(el, maxWidth){
    if (!el) return;
    el.removeAttribute('textLength');
    el.removeAttribute('lengthAdjust');
    let len = 0;
    try { len = el.getComputedTextLength(); } catch (_) { return; }
    if (len > maxWidth){
      el.setAttribute('textLength', maxWidth);
      el.setAttribute('lengthAdjust', 'spacingAndGlyphs');
    }
  }

  // The hero schematic mirrors the live config. Per-entrance live/standby health is applied
  // separately by applyEntranceHealth() from GET /admin/status (active entrance → emerald box
  // + flow; standby → grey static line); here we only render the hosts and listen address.
  function renderSchematic(cfg){
    const ar = cfg.anyrouter || {}, cpa = cfg.cpa || {};
    const entrances = ar.entrances || [];
    const coreListen = document.getElementById('coreListen');
    coreListen.textContent = 'classifier detect · :' + portOf(cfg.listen_addr);
    fitText(coreListen, CORE_TEXT_WIDTH);
    const ar1Host = document.getElementById('ar1Host');
    ar1Host.textContent = entrances[0] ? hostOf(entrances[0]) : '—';
    fitText(ar1Host, HOST_TEXT_WIDTH);
    const hasTwo = entrances.length >= 2;
    document.getElementById('ar2').style.display = hasTwo ? '' : 'none';
    document.getElementById('ar2Flow').style.display = hasTwo ? '' : 'none';
    if (hasTwo){
      const ar2Host = document.getElementById('ar2Host');
      ar2Host.textContent = hostOf(entrances[1]);
      fitText(ar2Host, HOST_TEXT_WIDTH);
    }
    const cpaHost = document.getElementById('cpaHost');
    cpaHost.textContent = cpa.upstream ? hostOf(cpa.upstream) : '—';
    fitText(cpaHost, HOST_TEXT_WIDTH);
  }

  function render(cfg){
    const ar = cfg.anyrouter || {}, cpa = cfg.cpa || {}, cls = cfg.classifier || {};
    document.getElementById('listenSub').textContent = cfg.listen_addr || '';
    document.getElementById('port').value = portOf(cfg.listen_addr);
    // Max log size: the config stores bytes, the desk shows/edits whole MB (the bytes-to-MB conversion).
    // GET /admin/config always returns a normalized positive cap, so divide to MB;
    // round so an odd hand-edited byte value still presents (and re-collects) as a
    // clean integer MB.
    document.getElementById('logMaxMb').value = Math.round((cfg.log_max_bytes || 0) / 1048576);
    entranceList.innerHTML = '';
    (ar.entrances || []).forEach(addEntrance);
    acctList.innerHTML = '';
    (ar.accounts || []).forEach(a => addAccount(a.label, a.key));
    clearAcctErr();
    document.getElementById('cpaUpstream').value = cpa.upstream || '';
    document.getElementById('cpaKey').value = cpa.key || '';
    document.getElementById('cpaCa').value = cpa.ca_path || '';
    document.getElementById('clsTarget').value = cls.target_base_url || '';
    document.getElementById('clsKey').value = cls.target_key || '';
    // Classifier-target shaping: type ("" ⇒ auto), self-signed CA, skip-verify.
    document.getElementById('clsType').value = cls.target_type || 'auto';
    document.getElementById('clsCa').value = cls.target_ca_path || '';
    clsInsecure.checked = cls.target_insecure_skip_verify === true;
    syncInsecure();
    document.getElementById('clsModel').value = cls.model_override || '';
    setMode((cls.target_base_url || '') ? 'custom' : 'default');
    // Home the Service card for the loaded mode with NO animation — only a user
    // toggle animates. An initial Single-target config thus paints with the card
    // already in the left column (the destination layout).
    placeServiceCard(false);
    // Global switch: on unless persisted explicitly false (nil/null/true ⇒ on).
    setEnabled(cfg.enabled !== false);
    renderSchematic(cfg);
    // Re-apply the last-known runtime health to the freshly rebuilt schematic boxes
    // and account cards so they aren't blank until the next poll.
    if (lastStatus){ applyEntranceHealth(lastStatus.entrances || []); applyAccountHealth(lastStatus.accounts || []); }
    // The loaded form is the clean baseline; the save bar stays hidden until an edit
    // makes the form diverge from it (and hides again after Revert/Save reloads).
    baseline = snapshot();
    checkDirty();
  }

  function collect(){
    const entrances = Array.from(entranceList.querySelectorAll('.entry input')).map(i => i.value.trim()).filter(Boolean);
    const accounts = Array.from(acctList.querySelectorAll('.acct'))
      .map(c => ({ label: c.querySelector('.nm').value.trim(), key: c.querySelector('.key').value.trim() }))
      .filter(a => a.key || a.label);
    const custom = document.querySelector('.seg button[data-mode="custom"]').getAttribute('aria-pressed') === 'true';
    const insecure = custom && clsInsecure.checked;
    return {
      listen_addr: '127.0.0.1:' + document.getElementById('port').value.trim(),
      enabled: enabledFromUI(),
      // Max log size: MB in the UI → bytes for the config (the bytes-to-MB conversion). collect() MUST
      // carry this. Without it the POST body omits log_max_bytes, the server decodes
      // it to 0, and normalizeRuntimeConfig resets a non-default cap to the 100 MB
      // default (and spuriously reports restart_required) on every Save. Round the MB
      // to an integer before scaling so the byte value is always a clean MB multiple
      // (matching the installer's mb_to_bytes); save-time validation gates a positive
      // integer MB before this is POSTed.
      log_max_bytes: Math.round(Number(document.getElementById('logMaxMb').value.trim())) * 1048576,
      anyrouter: { entrances: entrances, accounts: accounts },
      cpa: {
        upstream: document.getElementById('cpaUpstream').value.trim(),
        key: document.getElementById('cpaKey').value.trim(),
        ca_path: document.getElementById('cpaCa').value.trim()
      },
      classifier: {
        target_base_url: custom ? document.getElementById('clsTarget').value.trim() : '',
        target_key: custom ? document.getElementById('clsKey').value.trim() : '',
        model_override: document.getElementById('clsModel').value.trim(),
        // classifier-target fields only carry meaning with a custom target; default mode sends the
        // safe empties. CA path is suppressed while skip-verify is on (mutually
        // exclusive — the backend 400s if both are set), so they can never both ship.
        target_type: custom ? document.getElementById('clsType').value : '',
        target_ca_path: (custom && !insecure) ? clsCa.value.trim() : '',
        target_insecure_skip_verify: insecure
      }
    };
  }

  function toast(msg, ok){
    let t = document.getElementById('toast');
    if (!t){
      t = document.createElement('div');
      t.id = 'toast';
      t.style.cssText = 'position:fixed;left:50%;bottom:96px;transform:translateX(-50%);z-index:60;padding:11px 18px;border-radius:12px;font-weight:600;font-size:13.5px;color:#fff;box-shadow:var(--shadow);transition:opacity .4s;';
      document.body.appendChild(t);
    }
    t.textContent = msg;
    t.style.background = ok ? 'var(--emerald)' : 'var(--coral)';
    t.style.opacity = '1';
    clearTimeout(t._timer);
    t._timer = setTimeout(() => { t.style.opacity = '0'; }, 3200);
  }

  // A port change moves the listener to a new port; this page (still on the old
  // port) can't follow it after the shim re-execs, so show a persistent card with
  // the new desk URL for the user to open. The host is fixed to loopback
  // (127.0.0.1), matching the shim's listen contract. Unlike toast() this does not
  // auto-dismiss — it carries an actionable link, and the old page is about to go dark.
  function showMovedNotice(port){
    const url = 'http://127.0.0.1:' + port + '/admin';
    savebar.hidden = true;
    let n = document.getElementById('moved');
    if (!n){
      n = document.createElement('div');
      n.id = 'moved';
      n.style.cssText = 'position:fixed;left:50%;bottom:96px;transform:translateX(-50%);z-index:60;max-width:540px;'
        + 'padding:16px 20px;border-radius:14px;background:var(--card);border:1px solid var(--line);'
        + 'color:var(--paper);box-shadow:var(--shadow);font-size:14px;text-align:center;';
      document.body.appendChild(n);
    }
    n.textContent = '';
    const title = document.createElement('div');
    title.style.cssText = 'font-weight:700;margin-bottom:6px;';
    title.textContent = 'Saved · port changed — the shim is restarting on a new port.';
    const lead = document.createElement('div');
    lead.append('Open ');
    const link = document.createElement('a');
    link.href = url;
    link.textContent = url;
    link.style.cssText = 'color:var(--iris);font-family:ui-monospace,Menlo,monospace;font-weight:600;';
    lead.appendChild(link);
    n.appendChild(title);
    n.appendChild(lead);
  }

  // ---- Runtime status (GET /admin/status): top-bar chips + live entrance/account
  // health. Polled on an interval and degrades gracefully if the endpoint is
  // unreachable — the chips fall back to placeholders and the config form keeps
  // working (status is read-only state, never config).
  function fmtUptime(sec){
    sec = Math.max(0, Math.floor(sec));
    const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60), s = sec % 60;
    const p2 = n => String(n).padStart(2, '0');
    if (d > 0) return d + 'd ' + p2(h) + 'h';
    if (h > 0) return h + 'h ' + p2(m) + 'm';
    if (m > 0) return m + 'm ' + p2(s) + 's';
    return s + 's';
  }
  // Tick uptime locally each second from the last fetched uptime_seconds plus the
  // elapsed wall time since that fetch: smooth per-second growth without polling
  // every second, and robust to client/server clock skew (no start_time math).
  function tickUptime(){
    uptimeVal.textContent = uptimeBase === null ? '—' : fmtUptime(uptimeBase + (Date.now() - uptimeAt) / 1000);
  }

  // Entrance flow lines mirror the box health: the active entrance carries the animated
  // emerald flow; standby entrances get a static gray dashed line (no animation, so it
  // stays calm under prefers-reduced-motion too). Stroke color/width are presentation
  // attributes; the .flow class supplies the animated dash + linecap when active.
  function setFlowActive(p){
    if (!p) return;
    p.setAttribute('stroke', 'var(--emerald)');
    p.setAttribute('stroke-width', '3');
    p.removeAttribute('stroke-dasharray');
    p.removeAttribute('stroke-linecap');
    p.classList.add('flow');
  }
  function setFlowStandby(p){
    if (!p) return;
    p.classList.remove('flow');
    p.setAttribute('stroke', 'var(--line-2)');
    p.setAttribute('stroke-width', '2.5');
    p.setAttribute('stroke-dasharray', '3 8');
    p.setAttribute('stroke-linecap', 'round');
  }

  // Active entrance → emerald border + green dot + emerald flow line; the rest dim +
  // STANDBY tag + gray static line. status entrances[] are in pool order, which is config
  // order — the same order the schematic draws ar1/ar2 from — so they map by index. Boxes
  // the schematic is not showing (fewer than two entrances → ar2 hidden) are skipped.
  function applyEntranceHealth(entrances){
    [['ar1', 'ar1Box', 'ar1Dot', 'ar1Tag', 'ar1Flow', 'ar1Sub'],
     ['ar2', 'ar2Box', 'ar2Dot', 'ar2Tag', 'ar2Flow', 'ar2Sub']].forEach((ids, i) => {
      const grp = document.getElementById(ids[0]), box = document.getElementById(ids[1]);
      const dot = document.getElementById(ids[2]), tag = document.getElementById(ids[3]);
      const flow = document.getElementById(ids[4]), sub = document.getElementById(ids[5]);
      if (!grp || !box || grp.style.display === 'none') return;
      const e = entrances[i];
      if (!e){
        box.setAttribute('class', 'nr'); dot.style.display = 'none'; tag.style.display = 'none'; grp.style.opacity = '';
        setFlowStandby(flow); if (sub) sub.setAttribute('fill', 'var(--mist-2)');
        return;
      }
      const active = !!e.active;
      box.setAttribute('class', active ? 'nr em' : 'nr');
      dot.style.display = active ? '' : 'none';
      tag.style.display = active ? 'none' : '';
      grp.style.opacity = active ? '' : '.55';
      if (active) setFlowActive(flow); else setFlowStandby(flow);
      if (sub) sub.setAttribute('fill', active ? 'var(--emerald)' : 'var(--mist-2)');
    });
  }

  // Account pills mirror the relative-health model: Active (healthy) /
  // Deprioritized (soft steer-away — EWMA failRate exceeds the healthiest eligible
  // peer by > δ; sticky sessions keep their pin) / Evicted (401/403 hard cooldown,
  // coral + .ev card border), plus a sticky session count. Matched to form cards by
  // label; an unmatched card (unsaved, empty key, or otherwise not in the pool)
  // shows no pill.
  function applyAccountHealth(accounts){
    const byLabel = {};
    accounts.forEach(a => { if (a && a.label != null) byLabel[a.label] = a; });
    acctList.querySelectorAll('.acct').forEach(card => {
      const pill = card.querySelector('.acct-pill'), sess = card.querySelector('.acct-sessions');
      if (!pill || !sess) return;
      const a = byLabel[card.querySelector('.nm').value.trim()];
      if (!a){ pill.hidden = true; pill.textContent = ''; sess.textContent = ''; card.classList.remove('ev'); return; }
      let cls = 'live', text = 'Active';
      if (a.cooldown){ cls = 'evicted'; text = 'Evicted'; }
      else if (a.deprioritized){ cls = 'deprioritized'; text = 'Deprioritized'; }
      pill.className = 'pill acct-pill ' + cls;
      pill.textContent = text;
      pill.hidden = false;
      const n = a.sessions | 0;
      sess.textContent = n === 1 ? '1 session' : n + ' sessions';
      card.classList.toggle('ev', !!a.cooldown);
    });
  }

  function applyStatus(st){
    lastStatus = st;
    uptimeBase = typeof st.uptime_seconds === 'number' ? st.uptime_seconds : null;
    uptimeAt = Date.now();
    tickUptime();
    rewritesVal.textContent = typeof st.rewrites === 'number' ? st.rewrites : '—';
    applyEntranceHealth(st.entrances || []);
    applyAccountHealth(st.accounts || []);
    liveChip.classList.add('live');
  }

  function pollStatus(){
    fetch('/admin/status').then(r => r.ok ? r.json() : Promise.reject(r.status)).then(applyStatus).catch(() => {
      // Lost the shim/status endpoint: mute the Live chip (its dot drops with .live).
      // Keep the last uptime ticking; only blank values we never got.
      liveChip.classList.remove('live');
      if (uptimeBase === null) uptimeVal.textContent = '—';
      if (lastStatus === null) rewritesVal.textContent = '—';
    });
  }

  function load(){
    fetch('/admin/config').then(r => r.json()).then(render).catch(e => toast('Failed to load config: ' + e, false));
  }

  document.getElementById('revert').addEventListener('click', load);
  document.getElementById('save').addEventListener('click', () => {
    const port = Number(document.getElementById('port').value.trim());
    if (!Number.isInteger(port) || port < 1 || port > 65535){
      toast('Port must be a whole number between 1 and 65535', false);
      return;
    }
    // Max log size is edited in whole MB (converted to bytes in collect()). Reuse the
    // port field's numeric-input validation style: reject a non-integer or < 1 MB so
    // collect() never POSTs a zero/fractional cap.
    const logMaxMb = Number(document.getElementById('logMaxMb').value.trim());
    if (!Number.isInteger(logMaxMb) || logMaxMb < 1){
      toast('Max log size must be a whole number of MB (at least 1)', false);
      return;
    }
    // Block on duplicate non-empty account labels: they would alias each other in
    // the health snapshot/pills, and the backend rejects them too (400). Surface
    // the offending field(s) inline (coral border + message) plus a toast, since
    // the Save button sits far from the accounts card.
    clearAcctErr();
    const dupLabel = markDuplicateLabels();
    if (dupLabel){
      acctErr.textContent = 'Duplicate account label "' + dupLabel + '" — each account needs a unique label.';
      acctErr.hidden = false;
      toast('Fix duplicate account labels before saving', false);
      return;
    }
    fetch('/admin/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(collect()) })
      .then(async r => {
        const data = await r.json().catch(() => ({}));
        if (!r.ok){ toast(data.error ? 'Rejected: ' + data.error : 'Save rejected', false); return; }
        if (data.restarting){
          // A listen_addr or log_max_bytes change can't hot-swap (both bind only at
          // startup), so the shim re-execs itself to apply it. A same-port restart
          // (e.g. a log-size change) rebinds the SAME port, so the page can stay put
          // and the status poll reconnects once the new process is up. A port change
          // moves the listener, which this page (on the old port) can't follow — so
          // point the user at the new desk URL instead.
          const newPort = portOf(data.listen_addr || '');
          if (newPort && newPort !== location.port){
            baseline = snapshot(); checkDirty(); // change is persisted; hide the save bar
            showMovedNotice(newPort);
            return;
          }
          load();
          toast('Saved · restarting…', true);
          return;
        }
        load();
        toast('Saved', true);
      }).catch(e => toast('Save failed: ' + e, false));
  });

  // cover all static inputs once on load; dynamically-added entrance/account
  // rows are covered as they're built in addEntrance/addAccount.
  killInputAssist();
  load();
  // Runtime-status polling: fetch immediately, then every few seconds; tick the
  // uptime display every second so it grows visibly between polls.
  pollStatus();
  setInterval(pollStatus, 5000);
  setInterval(tickUptime, 1000);
})();
