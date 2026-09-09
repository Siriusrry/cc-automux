/* Logs: history + live stream with a client-owned seam, bounded rows, buffered pause and drop notices. */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  const { h, icon, replace, clear, pill, tag, field, input, select, switchCtl, kv, banner, empty, skeleton, toast, mascot } = CCAM.ui;
  const fmt = CCAM.fmt;
  const store = CCAM.store;
  const api = CCAM.api;
  CCAM.pages = CCAM.pages || {};

  const KIND_TIP = {
    forward: 'Forwarded — the request was sent to this provider.',
    success: "Success — the upstream answered and the provider's health channel was marked healthy.",
    failure: 'Failure — the request could not be completed. Expand the record for details.',
    failover: 'Failover — after this failure the request moved on to the next provider.'
  };
  const EVENT_TIP = {
    listening: 'Service event — the listener bound and started serving.',
    pending_rejected: 'Service event — a pending configuration was rejected on restart; the previous configuration stays active.',
    restart_failed: 'Service event — the process could not re-execute; the previous listener was restored.'
  };
  const RT_TIP = {
    normal: 'Normal Messages request — routed through the provider pool by priority.',
    classifier: 'Classifier request — recognised by the Claude Code security-monitor prompt and routed by auto mode.'
  };
  const ST_TIP = {
    live: 'Streaming — new records are appended as they are written.',
    paused: 'Following is paused while you read; new records queue until you jump to the bottom.',
    connecting: 'Opening the live stream…',
    reconnecting: 'The stream disconnected; reconnecting.',
    'history only': 'Live streaming is off — showing history only.'
  };

  const PAGE = 200;
  const BUFFER_MAX = 2000;
  const LEVELS = ['INFO', 'WARN', 'ERROR'];
  const KINDS = ['forward', 'success', 'failure', 'failover'];
  const EVENTS = ['listening', 'pending_rejected', 'restart_failed'];
  const KNOWN = ['time', 'level', 'msg', 'seq', 'kind', 'event', 'truncated', 'ref'];

  function keyOf(r) { return r.time + '|' + r.seq; }
  const cmp = fmt.compareRecords;

  function toggleGroup(values, selected, onchange, kindClass) {
    const wrap = h('div', { class: 'togg', role: 'group' });
    values.forEach(v => {
      const b = h('button', { type: 'button', 'aria-pressed': String(selected.includes(v)), onclick: () => { const on = b.getAttribute('aria-pressed') !== 'true'; b.setAttribute('aria-pressed', String(on)); onchange(v, on); } },
        kindClass ? h('span', { class: 'dot ' + (kindClass[v] || 'mist') }) : null, v);
      wrap.appendChild(b);
    });
    wrap.sync = (sel) => wrap.querySelectorAll('button').forEach((b, i) => b.setAttribute('aria-pressed', String(sel.includes(values[i]))));
    return wrap;
  }

  CCAM.pages.logs = {
    render(ctx) {
      ctx.title('Logs');
      ctx.subtitle(['Structured gateway and service records · ', h('b', null, 'history'), ' from the log files and ', h('b', null, 'live'), ' records as they are written']);
      const state = {
        filter: { level: [], kind: [], event: [], request_type: [], provider_id: [], model: [], session_id: [], http_status: [], since: '', until: '' },
        live: true, following: true, buffer: [], reloadRequired: false, dropped: 0,
        stream: null, records: new Map(), hasMore: false, nextCursor: null, loading: false, malformed: 0, providers: [], conn: 'off', generation: 0
      };
      let disposed = false, retryCount = 0, retryTimer = null;

      // ---- toolbar ----
      const liveSw = switchCtl({ checked: true, label: 'Live', class: 'ok', tip: 'Stream new records as they are written. Off shows history only.', onchange: (v) => { state.live = v; reload(); } });
      const levelTg = toggleGroup(LEVELS, [], (v, on) => setMulti('level', v, on));
      const kindTg = toggleGroup(KINDS, [], (v, on) => setMulti('kind', v, on), { forward: 'iris', success: 'ok', failure: 'bad', failover: 'warn' });
      const providerSel = select({ value: '', options: [{ value: '', label: 'Any provider' }], onchange: (e) => { state.filter.provider_id = e.target.value ? [e.target.value] : []; reload(); } });
      providerSel.setAttribute('aria-label', 'Provider');
      const moreBtn = h('button', { class: 'btn sm', type: 'button', 'aria-expanded': 'false', onclick: () => { more.hidden = !more.hidden; moreBtn.setAttribute('aria-expanded', String(!more.hidden)); } }, icon('filter'), 'More filters');
      const clearBtn = h('button', { class: 'btn sm quiet', type: 'button', onclick: () => { Object.keys(state.filter).forEach(k => { state.filter[k] = Array.isArray(state.filter[k]) ? [] : ''; }); syncControls(); reload(); } }, 'Clear');
      const toolbar = h('div', { class: 'lg-toolbar' }, liveSw, h('span', { class: 'sep' }), levelTg, kindTg, providerSel, h('span', { class: 'grow' }), moreBtn, clearBtn);

      const typeTg = toggleGroup(['normal', 'classifier'], [], (v, on) => setMulti('request_type', v, on));
      const eventSel = select({ value: '', options: [{ value: '', label: 'Any service event' }].concat(EVENTS.map(e => ({ value: e, label: e }))), onchange: (e) => { state.filter.event = e.target.value ? [e.target.value] : []; reload(); } });
      const modelIn = input({ value: '', placeholder: 'exact model name', list: 'lg-models', onchange: (e) => { state.filter.model = e.target.value.trim() ? [e.target.value.trim()] : []; reload(); } });
      const sessionIn = input({ value: '', placeholder: 'session id', onchange: (e) => { state.filter.session_id = e.target.value.trim() ? [e.target.value.trim()] : []; reload(); } });
      const statusIn = input({ value: '', placeholder: '429, 502', onchange: (e) => { state.filter.http_status = e.target.value.split(/[,\s]+/).filter(Boolean); reload(); } });
      const sinceIn = input({ type: 'datetime-local', value: '', attrs: { step: '1' }, onchange: (e) => { state.filter.since = e.target.value; reload(); } });
      const untilIn = input({ type: 'datetime-local', value: '', attrs: { step: '1' }, onchange: (e) => { state.filter.until = e.target.value; reload(); } });
      // The card is as wide as the console below; the controls inside are content-sized and left-aligned. Columns 3
      // and 4 share the date-picker width, so Model and HTTP status sit exactly above Since and Until, and Session ID
      // spans the toggle and the select. Hints live in label info icons so both rows keep the same height.
      sinceIn.setAttribute('aria-label', 'Since'); untilIn.setAttribute('aria-label', 'Until');
      const info = (text) => h('span', { class: 'lab-i', 'data-tip': text }, icon('info'));
      const more = h('div', { class: 'lg-more', hidden: true }, h('div', { class: 'card lg-filters' },
        field({ class: 'f-type', label: 'Request type', control: typeTg }),
        field({ class: 'f-event', label: 'Service event', control: eventSel }),
        field({ class: 'f-model', label: 'Model', control: [modelIn, h('datalist', { id: 'lg-models' })] }),
        field({ class: 'f-status', label: 'HTTP status', labelExtra: info('Comma-separated; any listed status matches.'), control: statusIn }),
        field({ class: 'f-session', label: 'Session ID', control: sessionIn }),
        field({ class: 'f-range', label: 'Time range', labelExtra: info('Both ends inclusive, in your local time.'),
          control: h('div', { class: 'lg-range' }, sinceIn, h('label', { class: 'to', for: untilIn.id }, 'to'), untilIn) })));
      const chips = h('div', { class: 'lg-chips' });

      function setMulti(name, v, on) { const arr = state.filter[name]; if (on && !arr.includes(v)) arr.push(v); if (!on) state.filter[name] = arr.filter(x => x !== v); reload(); }
      function syncControls() {
        levelTg.sync(state.filter.level); kindTg.sync(state.filter.kind); typeTg.sync(state.filter.request_type);
        providerSel.value = state.filter.provider_id[0] || ''; eventSel.value = state.filter.event[0] || '';
        modelIn.value = state.filter.model[0] || ''; sessionIn.value = state.filter.session_id[0] || ''; statusIn.value = state.filter.http_status.join(', ');
        sinceIn.value = state.filter.since; untilIn.value = state.filter.until;
      }
      function renderChips() {
        const out = [];
        const add = (k, v, remove) => out.push(h('span', { class: 'fchip' }, h('span', { class: 'k' }, k), h('span', { class: 'mono' }, v), h('button', { type: 'button', 'aria-label': 'Remove filter', onclick: remove }, icon('x'))));
        ['level', 'kind', 'event', 'request_type', 'model', 'session_id', 'http_status'].forEach(k => state.filter[k].forEach(v => add(k.replace('_', ' '), k === 'session_id' ? fmt.middle(v, 18) : v, () => { state.filter[k] = state.filter[k].filter(x => x !== v); syncControls(); reload(); })));
        state.filter.provider_id.forEach(v => { const p = state.providers.find(x => x.id === v); add('provider', p ? p.name : v, () => { state.filter.provider_id = []; syncControls(); reload(); }); });
        if (state.filter.since) add('since', state.filter.since.replace('T', ' '), () => { state.filter.since = ''; syncControls(); reload(); });
        if (state.filter.until) add('until', state.filter.until.replace('T', ' '), () => { state.filter.until = ''; syncControls(); reload(); });
        replace(chips, out);
      }
      function queryString(history) {
        const q = new URLSearchParams();
        ['level', 'kind', 'event', 'request_type', 'provider_id', 'model', 'session_id', 'http_status'].forEach(k => state.filter[k].forEach(v => q.append(k, v)));
        if (state.filter.since) q.append('since', new Date(state.filter.since).toISOString());
        if (state.filter.until) q.append('until', new Date(state.filter.until).toISOString());
        if (history) q.append('limit', String(PAGE));
        return q.toString();
      }

      // ---- console ----
      const stEl = h('span', { class: 'st off' }, 'off');
      const countEl = h('span', null, '0 records');
      const malformedEl = h('span', { hidden: true });
      const head = h('div', { class: 'console-head' }, h('span', null, 'log stream · ', stEl), h('div', { class: 'meta' }, countEl, malformedEl, h('span', { class: 'dots', 'data-tip': 'Severity colours: coral ERROR · amber WARN · emerald INFO' }, h('i', { style: { background: 'var(--coral)' } }), h('i', { style: { background: 'var(--amber)' } }), h('i', { style: { background: 'var(--emerald)' } }))));
      const alerts = h('div', { class: 'console-alerts' });
      const olderRow = h('div', { class: 'lg-older', hidden: true }, h('button', { class: 'btn sm', type: 'button', 'data-tip': 'Fetch the page of history older than the oldest record shown.', onclick: () => loadOlder() }, icon('arrow-up'), 'Load older'));
      const endRow = h('div', { class: 'lg-end', hidden: true }, 'Beginning of the retained log');
      const rows = h('div', { class: 'lg-rows' });
      const emptyEl = h('div', { hidden: true });
      const newPill = h('div', { class: 'lg-new', hidden: true }, h('button', { class: 'btn sm primary', type: 'button', onclick: () => jumpToBottom() }, icon('arrow-down'), h('span', { class: 'lg-new-n' }, '')));
      const stream = h('div', { class: 'stream', tabindex: '0', 'aria-label': 'Log records' }, olderRow, endRow, rows, emptyEl, newPill);
      const consoleCard = h('div', { class: 'card tight console' }, head, alerts, stream);
      const loggingBanner = h('div', { class: 'banners' });
      ctx.root.appendChild(h('div', null, loggingBanner, toolbar, more, chips, consoleCard));

      function setConn(c) {
        state.conn = c;
        const text = c === 'live' ? (state.following ? 'live' : 'paused') : c === 'connecting' ? 'connecting' : c === 'bad' ? 'reconnecting' : 'history only';
        stEl.textContent = text; stEl.className = 'st ' + (c === 'live' ? (state.following ? 'live' : 'paused') : c === 'bad' ? 'bad' : 'off'); stEl.dataset.tip = ST_TIP[text] || '';
      }
      function renderLoggingBanner() {
        const st = store.state.status;
        const out = [];
        if (st && st.logging && !st.logging.healthy) out.push(banner('bad', 'Log writes are failing', ['Nothing has been written since ', h('b', null, fmt.dateTime(st.logging.last_failure_at)), ' (', fmt.plural(st.logging.failures, 'failure'), '). Records shown here stop at the last successful write; the gateway keeps serving. ', h('span', { class: 'mono' }, st.logging.last_error || '')]));
        replace(loggingBanner, out);
      }
      function renderAlerts() {
        const out = [];
        if (state.dropped) out.push(banner('bad', 'Live records were dropped', 'The browser fell behind and the service discarded ' + fmt.plural(state.dropped, 'live record') + '. They are still in the log files — reload the view to read them back.', [h('button', { class: 'btn sm', type: 'button', onclick: () => reload() }, icon('refresh'), 'Reload view')]));
        if (state.reloadRequired && !state.following) out.push(banner('warn', 'Too many records buffered', 'More than ' + BUFFER_MAX + ' live records arrived while you were scrolled up. Returning to the bottom reloads the view instead of appending.'));
        replace(alerts, out);
      }
      function updateCounts() {
        countEl.textContent = fmt.plural(state.records.size, 'record');
        malformedEl.hidden = !state.malformed; malformedEl.textContent = state.malformed ? state.malformed + ' malformed skipped' : '';
        olderRow.hidden = !state.hasMore; endRow.hidden = state.hasMore || !state.records.size;
        emptyEl.hidden = !!state.records.size;
        if (!state.records.size) replace(emptyEl, empty({ title: state.loading ? 'Loading…' : 'No matching records', text: state.loading ? '' : 'Nothing in the retained log matches these filters' + (state.live ? '; new matching records will appear here as they are written.' : '.'), mascot: !state.loading }));
      }

      // ---- rows ----
      function summary(r) {
        const parts = [];
        if (r.msg === 'service') { parts.push(h('span', { class: 'p' }, r.event || 'service')); if (r.listen_addr) parts.push(h('span', { class: 'm' }, r.listen_addr)); }
        else {
          if (r.provider_name) parts.push(h('span', { class: 'p' }, r.provider_name));
          if (r.model) parts.push(h('span', { class: 'm' }, r.model));
          if (r.request_type) parts.push(h('span', { class: 'rt ' + r.request_type, 'data-tip': RT_TIP[r.request_type] || '' }, r.request_type));
          if (r.http_status) parts.push(h('span', { class: 'hs' + (r.http_status >= 400 ? ' bad' : '') }, 'HTTP ' + r.http_status));
          if (r.attempt) parts.push(h('span', { class: 'm' }, 'attempt ' + r.attempt));
          if (r.kind === 'failover' && r.next_provider_name) parts.push(h('span', { class: 'arrow' }, '→ ' + r.next_provider_name + ' (attempt ' + r.next_attempt + ')'));
          if (r.patch_id) parts.push(h('span', { class: 'm' }, 'patch ' + r.patch_id + '/' + r.patch_stage));
        }
        return h('div', { class: 'sum' }, parts);
      }
      function row(r) {
        const kind = r.msg === 'service' ? 'service' : (r.kind || 'gateway');
        const errText = r.raw_error || r.error || '';
        const el = h('div', { class: 'lg', dataset: { kind, key: keyOf(r) }, role: 'button', tabindex: '0', 'aria-expanded': 'false' },
          h('span', { class: 'ts', 'data-tip': r.time + (r.seq !== undefined ? '\nseq ' + r.seq : '') }, fmt.timeShort(r.time)),
          h('span', { class: 'lvl ' + r.level }, r.level),
          h('span', { class: 'kind ' + kind, 'data-tip': r.msg === 'service' ? (EVENT_TIP[r.event] || 'Service event.') : (KIND_TIP[r.kind] || '') }, r.msg === 'service' ? 'service' : r.kind),
          h('div', { class: 'body' }, summary(r), errText ? h('div', { class: 'err' }, errText) : null),
          icon('chevron-right', 'exp'));
        const toggle = () => {
          const open = el.classList.toggle('open'); el.setAttribute('aria-expanded', String(open));
          const existing = el.querySelector('.lg-detail');
          if (!open) { if (existing) existing.remove(); return; }
          el.appendChild(detail(r));
        };
        el.addEventListener('click', (e) => { if (e.target.closest('.lg-detail')) return; toggle(); });
        el.addEventListener('keydown', (e) => { if (e.target !== el) return; if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); } });
        return el;
      }
      function detail(r) {
        const rowsKv = Object.keys(r).filter(k => !KNOWN.includes(k)).map(k => [k, typeof r[k] === 'object' ? JSON.stringify(r[k]) : String(r[k])]);
        rowsKv.unshift(['time', r.time], ['seq', String(r.seq)]);
        const box = h('div', { class: 'lg-detail' }, kv(rowsKv, { compact: true }));
        if (r.truncated) {
          const names = Object.keys(r.truncated);
          const total = names.reduce((n, k) => n + r.truncated[k], 0);
          const btn = h('button', { class: 'btn sm', type: 'button', onclick: async () => {
            btn.disabled = true; replace(btn, h('span', { class: 'spin' }), 'Loading…');
            try {
              const full = await api.get('/api/v1/logs/record?ref=' + encodeURIComponent(r.ref));
              replace(trunc, icon('check'), 'Complete record loaded · ' + fmt.bytes(total));
              box.appendChild(h('pre', { class: 'code wrap' }, fmt.pretty(full)));
            } catch (e) {
              if (e.status === 404) { trunc.classList.add('gone'); replace(trunc, icon('info'), 'This record has been rotated out of the log files; only the bounded summary remains.'); }
              else { btn.disabled = false; replace(btn, 'Show complete'); toast('Could not load the record: ' + (e.detail || e.message), 'bad'); }
            }
          } }, 'Show complete');
          const trunc = h('div', { class: 'lg-trunc' }, icon('alert'), 'Truncated · ' + names.join(', ') + ' · ' + fmt.bytes(total) + ' withheld', btn);
          box.appendChild(trunc);
        } else box.appendChild(h('pre', { class: 'code wrap' }, fmt.pretty(r)));
        return box;
      }

      // ---- scroll / follow ----
      const atBottom = () => stream.scrollHeight - stream.scrollTop - stream.clientHeight <= 14;
      function setFollowing(on) {
        if (state.following === on) return;
        state.following = on; setConn(state.conn); renderAlerts();
        if (on) { newPill.hidden = true; }
      }
      stream.addEventListener('scroll', () => {
        if (atBottom()) { if (!state.following) resumeFromBuffer(); }
        else if (state.following) setFollowing(false);
      });
      function jumpToBottom() { stream.scrollTop = stream.scrollHeight; resumeFromBuffer(); }
      function resumeFromBuffer() {
        if (state.reloadRequired) { setFollowing(true); reload(); return; }
        const buf = state.buffer; state.buffer = [];
        setFollowing(true);
        if (buf.length) { buf.forEach(r => insert(r)); updateCounts(); }
        stream.scrollTop = stream.scrollHeight;
      }
      function insert(r) {
        const key = keyOf(r);
        if (state.records.has(key)) return false;
        state.records.set(key, r);
        // Append when newest, otherwise place by (time, seq).
        const last = rows.lastElementChild;
        if (!last || cmp(state.records.get(last.dataset.key), r) < 0) { rows.appendChild(row(r)); return true; }
        let node = rows.firstElementChild;
        while (node && cmp(state.records.get(node.dataset.key), r) < 0) node = node.nextElementSibling;
        rows.insertBefore(row(r), node);
        return true;
      }
      function onLiveRecord(r) {
        if (state.records.has(keyOf(r))) return;
        if (state.following) { insert(r); updateCounts(); stream.scrollTop = stream.scrollHeight; return; }
        if (state.reloadRequired) return;
        state.buffer.push(r);
        if (state.buffer.length > BUFFER_MAX) { state.buffer = []; state.reloadRequired = true; renderAlerts(); newPill.hidden = true; return; }
        newPill.hidden = false; newPill.querySelector('.lg-new-n').textContent = fmt.plural(state.buffer.length, 'new record');
      }

      // ---- seam: subscribe first, then history, merge ----
      function teardown() { if (state.stream) { state.stream.close(); state.stream = null; } }
      async function reload(retrying) {
        if (retrying !== true) retryCount = 0;
        clearTimeout(retryTimer);
        const gen = ++state.generation;
        teardown();
        state.records.clear(); state.buffer = []; state.reloadRequired = false; state.dropped = 0; state.hasMore = false; state.nextCursor = null; state.malformed = 0; state.loading = true;
        clear(rows); renderAlerts(); renderChips(); updateCounts(); setFollowing(true);
        const pending = [];
        let subscription = null;
        if (state.live) {
          setConn('connecting');
          subscription = state.stream = api.stream('/api/v1/logs/stream?' + queryString(false), {
            onOpen: () => { if (gen === state.generation) setConn('live'); },
            onRecord: (r) => { if (gen !== state.generation) return; if (state.loading) { if (pending.length < BUFFER_MAX) pending.push(r); else { state.dropped++; renderAlerts(); } } else onLiveRecord(r); },
            onDropped: (n) => { if (gen !== state.generation) return; state.dropped += n; renderAlerts(); },
            onClose: (err) => {
              if (gen !== state.generation) return;
              state.stream = null;
              if (err && err.status === 422) { setConn('bad'); toast('Filter rejected: ' + (err.detail || err.message), 'bad'); return; }
              setConn('bad');
              if (retryCount < 1) { retryCount++; retryTimer = setTimeout(() => { if (gen === state.generation && state.live) reload(true); }, 2500); }
              else { alerts.appendChild(banner('warn', 'Live stream disconnected', 'Showing retained records. Retry to reconnect and refresh history.', [h('button', { class: 'btn sm', type: 'button', onclick: () => reload() }, 'Retry')])); }
            }
          });
        } else setConn('off');
        try {
          if (subscription) await subscription.ready;
          if (gen !== state.generation) return;
          const page = await api.get('/api/v1/logs?' + queryString(true));
          if (gen !== state.generation) return;
          state.loading = false;
          state.hasMore = page.has_more; state.nextCursor = page.next_cursor || null; state.malformed = page.skipped_malformed || 0;
          const items = page.items.slice().sort(cmp);
          items.concat(pending).sort(cmp).forEach(r => insert(r));
          updateCounts();
          stream.scrollTop = stream.scrollHeight;
        } catch (e) {
          if (gen !== state.generation) return;
          state.loading = false; updateCounts();
          if (e.status === 422) toast('Filter rejected: ' + (e.detail || e.message), 'bad');
          else replace(emptyEl, h('div', { class: 'empty' }, h('h3', null, 'Could not load history'), h('p', null, e.detail || e.message), h('button', { class: 'btn', type: 'button', onclick: reload }, icon('refresh'), 'Retry')));
          emptyEl.hidden = false;
        }
      }
      async function loadOlder() {
        if (!state.hasMore || !state.nextCursor) return;
        const btn = olderRow.querySelector('button'); btn.disabled = true; replace(btn, h('span', { class: 'spin' }), 'Loading…');
        const gen = state.generation;
        try {
          const page = await api.get('/api/v1/logs?' + queryString(true) + '&cursor=' + encodeURIComponent(state.nextCursor));
          if (gen !== state.generation) return;
          const before = stream.scrollHeight;
          page.items.slice().sort(cmp).forEach(r => insert(r));
          state.hasMore = page.has_more; state.nextCursor = page.next_cursor || null; state.malformed += page.skipped_malformed || 0;
          updateCounts();
          stream.scrollTop += stream.scrollHeight - before;
          if (!page.items.length && !page.has_more) { state.hasMore = false; updateCounts(); }
        } catch (e) { toast('Could not load older records: ' + (e.detail || e.message), 'bad'); }
        btn.disabled = false; replace(btn, icon('arrow-up'), 'Load older');
      }

      // ---- boot ----
      store.providers().then(list => { state.providers = list; providerSel.setOptions([{ value: '', label: 'Any provider' }].concat(list.map(p => ({ value: p.id, label: p.name })))); replace(more.querySelector('#lg-models'), Array.from(new Set(list.flatMap(p => p.models))).sort().map(m => h('option', { value: m }))); renderChips(); }).catch(() => {});
      renderLoggingBanner();
      const offStatus = store.on('status', renderLoggingBanner);
      reload();
      return () => { disposed = true; state.generation++; clearTimeout(retryTimer); teardown(); offStatus(); };
    }
  };
})();
