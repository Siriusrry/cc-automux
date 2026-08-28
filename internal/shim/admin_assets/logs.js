(function(){
  // Poll cadence for the incremental tail, and a bounded display window. The page keeps
  // at most MAX_PER_STREAM rows of EACH stream (stdout and stderr capped independently,
  // so All ≤ 2*MAX_PER_STREAM). The backend ring holds up to ~2000 lines, but building
  // thousands of DOM nodes freezes the page, so every incoming batch is sliced to the
  // most recent MAX_PER_STREAM per stream BEFORE nodes are created (the initial load can
  // return the whole ~2000-line ring at once) and trimmed the same way after appending.
  // Full history stays in the rotated log files / scripts/logs.sh.
  const POLL_MS = 2000;
  const MAX_PER_STREAM = 100;

  const streamEl = document.getElementById('stream');
  const rowsEl = document.getElementById('rows');
  const emptyEl = document.getElementById('empty');
  const emptyH = document.getElementById('emptyH');
  const emptyS = document.getElementById('emptyS');
  const conn = document.getElementById('conn');
  const qInput = document.getElementById('q');
  const followInput = document.getElementById('follow');
  const tabs = Array.from(document.querySelectorAll('.tabs button'));
  const counters = { all: document.getElementById('cAll'), stdout: document.getElementById('cOut'), stderr: document.getElementById('cErr') };

  let cursor = 0;            // last seq seen; sent as ?after= so we only fetch newer lines
  let inFlight = false;      // guard overlapping polls (both would re-fetch the same window)
  let connected = null;      // null until the first response, then bool
  let follow = true;         // auto-stick to the newest line
  let activeStream = 'all';  // stream tab filter
  let query = '';            // lowercased text filter
  const counts = { stdout: 0, stderr: 0 }; // rows currently in the DOM, per stream

  // ---- timestamp + message ---------------------------------------------------
  // logEntry.text keeps the logger's own "YYYY/MM/DD HH:MM:SS " (LstdFlags) prefix;
  // the ts column already shows the time, so strip the prefix from the message to
  // avoid showing it twice. Lines without the prefix are shown verbatim.
  const PREFIX_RE = /^\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)? /;
  function pad2(n){ return String(n).padStart(2, '0'); }
  function clockOf(entry){
    const d = new Date(entry && entry.ts);
    if (entry && entry.ts && !isNaN(d.getTime())) return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
    // Fall back to the time embedded in the logger prefix if ts is missing/unparseable.
    const m = /^\d{4}\/\d{2}\/\d{2} (\d{2}:\d{2}:\d{2})/.exec((entry && entry.text) || '');
    return m ? m[1] : '';
  }
  function messageOf(text){ return text.replace(PREFIX_RE, ''); }

  // ---- filtering + empty state ----------------------------------------------
  function rowMatches(row){
    const okStream = activeStream === 'all' || row.dataset.stream === activeStream;
    const okQ = !query || row._search.indexOf(query) !== -1;
    return okStream && okQ;
  }
  function applyFilterAll(){
    let visible = 0;
    rowsEl.querySelectorAll('.ln').forEach(row => {
      const match = rowMatches(row);
      row.classList.toggle('hide', !match);
      if (match) visible++;
    });
    updateEmpty(visible);
    maybeScroll();
  }
  function visibleCount(){ return rowsEl.querySelectorAll('.ln:not(.hide)').length; }
  // The empty placeholder is dynamic: distinguishes "nothing buffered yet", "buffered
  // but filtered out", and "endpoint unreachable with nothing to show". Subtitles are
  // developer-authored constants (no user/log data interpolated), safe via innerHTML.
  function updateEmpty(visible){
    if (visible === undefined) visible = visibleCount();
    if (visible > 0){ emptyEl.style.display = 'none'; return; }
    emptyEl.style.display = '';
    const total = counts.stdout + counts.stderr;
    if (total > 0){
      emptyH.textContent = 'No matching lines';
      emptyS.innerHTML = 'No buffered lines match this stream and filter. Clear the filter or switch tabs to see more.';
    } else if (connected === false){
      emptyH.textContent = 'Can’t reach the shim';
      emptyS.innerHTML = 'The log endpoint is unreachable right now. Retrying automatically — buffered lines reappear once it is back.';
    } else {
      emptyH.textContent = 'Waiting for log lines';
      emptyS.innerHTML = 'Live lines appear as the shim handles traffic. Only lines produced after start-up show here — full history is in <code>~/Library/Logs/cc-auto-mode-shim</code> or <code>./scripts/logs.sh</code>.';
    }
  }

  // ---- rendering -------------------------------------------------------------
  function makeRow(entry){
    const stream = entry && entry.stream === 'stderr' ? 'stderr' : 'stdout';
    const text = entry && typeof entry.text === 'string' ? entry.text : '';
    const row = document.createElement('div');
    row.className = 'ln';
    row.dataset.stream = stream;
    const ts = document.createElement('span'); ts.className = 'ts'; ts.textContent = clockOf(entry);
    const lvl = document.createElement('span'); lvl.className = 'lvl'; lvl.textContent = stream;
    // textContent (never innerHTML) for log text: it is arbitrary and must not be
    // interpreted as markup.
    const msg = document.createElement('span'); msg.className = 'msg'; msg.textContent = messageOf(text);
    row.appendChild(ts); row.appendChild(lvl); row.appendChild(msg);
    row._search = (stream + ' ' + text).toLowerCase();
    return row;
  }
  // Keep only the most recent MAX_PER_STREAM entries of EACH stream from a raw batch,
  // preserving chronological order, before any DOM node is built. Batch entries are
  // always newer than rows already in the DOM (seq > cursor, and the DOM is cleared on
  // restart), so anything past the last MAX_PER_STREAM of its stream here could never
  // survive trimOverflow() anyway — slicing first is what stops the initial ~2000-line
  // load from building (then discarding) thousands of nodes and freezing the page.
  function capRecentPerStream(entries){
    let nOut = 0, nErr = 0;
    const keep = [];
    for (let i = entries.length - 1; i >= 0; i--){
      const e = entries[i];
      if (e && e.stream === 'stderr'){ if (nErr >= MAX_PER_STREAM) continue; nErr++; }
      else { if (nOut >= MAX_PER_STREAM) continue; nOut++; }
      keep.push(e);
    }
    keep.reverse();
    return keep;
  }
  // Per-stream display cap: keep at most MAX_PER_STREAM rows of each stream in the DOM.
  // Walk oldest→newest and drop the oldest rows of whichever stream is over cap (stdout
  // and stderr capped independently, never as one combined total), stopping as soon as
  // both are within cap.
  function trimOverflow(){
    let row = rowsEl.firstElementChild;
    while (row && (counts.stdout > MAX_PER_STREAM || counts.stderr > MAX_PER_STREAM)){
      const next = row.nextElementSibling;
      const s = row.dataset.stream === 'stderr' ? 'stderr' : 'stdout';
      if (counts[s] > MAX_PER_STREAM){
        counts[s]--;
        rowsEl.removeChild(row);
      }
      row = next;
    }
  }
  function appendEntries(entries){
    const frag = document.createDocumentFragment();
    capRecentPerStream(entries).forEach(e => {
      const row = makeRow(e);
      row.classList.toggle('hide', !rowMatches(row));
      counts[row.dataset.stream]++;
      frag.appendChild(row);
    });
    rowsEl.appendChild(frag);
    trimOverflow();
    updateCounts();
    updateEmpty();
    maybeScroll();
  }
  function clearRows(){
    rowsEl.textContent = '';
    counts.stdout = 0; counts.stderr = 0;
    updateCounts();
  }
  function updateCounts(){
    counters.stdout.textContent = counts.stdout;
    counters.stderr.textContent = counts.stderr;
    counters.all.textContent = counts.stdout + counts.stderr;
  }

  // ---- Follow mode -----------------------------------------------------------
  // Auto-stick to the bottom while Follow is on; scrolling up pauses it (preserving
  // position) and scrolling back to the bottom resumes it, keeping the toggle in sync.
  function atBottom(){ return streamEl.scrollHeight - streamEl.scrollTop - streamEl.clientHeight <= 8; }
  function maybeScroll(){ if (follow) streamEl.scrollTop = streamEl.scrollHeight; }
  function setFollow(on){
    follow = on;
    if (followInput.checked !== on) followInput.checked = on;
    if (on) maybeScroll();
  }
  followInput.addEventListener('change', () => setFollow(followInput.checked));
  streamEl.addEventListener('scroll', () => {
    const bottom = atBottom();
    if (bottom && !follow) setFollow(true);
    else if (!bottom && follow) setFollow(false);
  });

  // ---- tabs + filter ---------------------------------------------------------
  tabs.forEach(b => b.addEventListener('click', () => {
    tabs.forEach(x => x.setAttribute('aria-pressed', x === b ? 'true' : 'false'));
    activeStream = b.dataset.stream;
    applyFilterAll();
  }));
  qInput.addEventListener('input', () => { query = qInput.value.trim().toLowerCase(); applyFilterAll(); });

  // ---- polling ---------------------------------------------------------------
  function setConnected(ok){
    connected = ok;
    conn.textContent = ok ? '· live' : '· reconnecting…';
    conn.classList.toggle('off', !ok);
  }
  function pollLogs(){
    if (inFlight) return;
    inFlight = true;
    fetch('/admin/logs/tail?after=' + cursor)
      .then(r => r.ok ? r.json() : Promise.reject(r.status))
      .then(data => {
        setConnected(true);
        const entries = data && Array.isArray(data.entries) ? data.entries : [];
        const head = data && typeof data.head === 'number' ? data.head : cursor;
        if (head < cursor){
          // head went backwards ⇒ the shim restarted (seq resets to 1). Drop the stale
          // pre-restart lines and reload from the fresh buffer on the next poll.
          clearRows();
          cursor = 0;
        } else {
          if (entries.length) appendEntries(entries);
          cursor = head;
        }
        updateEmpty();
      })
      .catch(() => {
        // Endpoint unreachable: mirror admin.html's pollStatus().catch() — degrade
        // gracefully, keep the buffered lines, flag the connection, let the interval retry.
        setConnected(false);
        updateEmpty();
      })
      .finally(() => { inFlight = false; });
  }

  updateCounts();
  updateEmpty();
  pollLogs();
  setInterval(pollLogs, POLL_MS);
})();
