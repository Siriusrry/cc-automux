/* CC AutoMux Web UI — DOM helpers and shared component factories. */
(function () {
  'use strict';
  const CCAM = (window.CCAM = window.CCAM || {});

  const BOOL_PROPS = new Set(['disabled', 'checked', 'hidden', 'readOnly', 'required', 'selected', 'open', 'multiple', 'autofocus']);

  function h(tag, attrs) {
    const el = document.createElement(tag);
    if (attrs) {
      for (const key of Object.keys(attrs)) {
        const v = attrs[key];
        if (v === undefined || v === null || v === false) { if (BOOL_PROPS.has(key)) el[key] = false; continue; }
        if (key === 'class') el.className = v;
        else if (key === 'text') el.textContent = v;
        else if (key === 'html') el.innerHTML = v; // trusted, developer-authored markup only
        else if (key === 'dataset') Object.assign(el.dataset, v);
        else if (key === 'style') Object.assign(el.style, v);
        else if (key === 'value') el.value = v;
        else if (key.startsWith('on') && typeof v === 'function') el.addEventListener(key.slice(2).toLowerCase(), v);
        else if (BOOL_PROPS.has(key)) el[key] = !!v;
        else el.setAttribute(key, v === true ? '' : v);
      }
    }
    for (let i = 2; i < arguments.length; i++) append(el, arguments[i]);
    return el;
  }
  function append(el, child) {
    if (child === null || child === undefined || child === false || child === true) return;
    if (Array.isArray(child)) { child.forEach(c => append(el, c)); return; }
    if (child instanceof Node) { el.appendChild(child); return; }
    el.appendChild(document.createTextNode(String(child)));
  }
  function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }
  function replace(el) { clear(el); for (let i = 1; i < arguments.length; i++) append(el, arguments[i]); return el; }
  function frag() { const f = document.createDocumentFragment(); for (let i = 0; i < arguments.length; i++) append(f, arguments[i]); return f; }

  function icon(name, cls) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'ico' + (cls ? ' ' + cls : ''));
    svg.setAttribute('aria-hidden', 'true');
    const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
    use.setAttribute('href', '#i-' + name);
    svg.appendChild(use);
    return svg;
  }
  function mascot(cls) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'mascot' + (cls ? ' ' + cls : ''));
    svg.setAttribute('viewBox', '0 0 69 45');
    svg.setAttribute('role', 'img');
    svg.setAttribute('aria-label', 'CC AutoMux mascot');
    const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
    use.setAttribute('href', '#mascot');
    svg.appendChild(use);
    return svg;
  }

  // ---- pills / tags ----
  const HEALTH_KIND = { healthy: 'ok', degraded: 'warn', cooldown: 'bad', half_open: 'iris', unknown: 'mist', disabled: 'ghost' };
  const HEALTH_TEXT = { healthy: 'Healthy', degraded: 'Degraded', cooldown: 'Cooldown', half_open: 'Half-open', unknown: 'Unknown', disabled: 'Health off' };
  function pill(text, kind, extra) { return h('span', { class: 'pill ' + (kind || 'mist') + (extra ? ' ' + extra : '') }, text); }
  function healthPill(state, suffix) { return pill((HEALTH_TEXT[state] || CCAM.fmt.cap(CCAM.fmt.words(state))) + (suffix ? ' · ' + suffix : ''), HEALTH_KIND[state] || 'mist'); }
  function healthDot(state) { const k = HEALTH_KIND[state] || 'mist'; return h('span', { class: 'dot ' + (k === 'ghost' ? 'mist' : k), 'data-tip': HEALTH_TEXT[state] || state }); }
  function tag(text, kind) { return h('span', { class: 'tag' + (kind ? ' ' + kind : '') }, text); }

  // ---- toast ----
  let toastHost = null;
  function toast(message, kind) {
    if (!toastHost) { toastHost = h('div', { class: 'toasts', role: 'status', 'aria-live': 'polite' }); document.body.appendChild(toastHost); }
    const iconName = kind === 'bad' ? 'alert' : kind === 'warn' ? 'alert' : 'check';
    const el = h('div', { class: 'toast ' + (kind || 'ok') }, icon(iconName), h('span', null, message));
    toastHost.appendChild(el);
    setTimeout(() => { el.style.opacity = '0'; el.style.transition = 'opacity 180ms'; setTimeout(() => el.remove(), 200); }, kind === 'bad' ? 6500 : 4000);
    return el;
  }

  // ---- dialog ----
  function dialog(opts) {
    return new Promise(resolve => {
      const dlg = h('dialog', { class: 'dlg' + (opts.wide ? ' wide' : '') });
      const inner = h('div', { class: 'dlg-in' });
      if (opts.title) inner.appendChild(h('h3', null, opts.title));
      if (opts.text) inner.appendChild(h('p', { class: 'dlg-text' }, opts.text));
      if (opts.body) append(inner, opts.body);
      const acts = h('div', { class: 'dlg-acts' });
      let settled = false;
      const finish = (value) => { if (settled) return; settled = true; dlg.close(); dlg.remove(); resolve(value); };
      (opts.actions || [{ label: 'Cancel', value: false }, { label: 'OK', value: true, primary: true }]).forEach(a => {
        const b = h('button', { class: 'btn' + (a.primary ? ' primary' : '') + (a.danger ? ' danger' : ''), type: 'button', onclick: async () => {
          if (a.onClick) { const r = await a.onClick(); if (r === false) return; finish(r === undefined ? a.value : r); return; }
          finish(a.value);
        } }, a.label);
        acts.appendChild(b);
      });
      inner.appendChild(acts);
      dlg.appendChild(inner);
      dlg.addEventListener('cancel', (e) => { e.preventDefault(); finish(false); });
      dlg.addEventListener('click', (e) => { if (e.target === dlg && !opts.modalOnly) finish(false); });
      document.body.appendChild(dlg);
      dlg.showModal();
      const focusTarget = opts.focus ? inner.querySelector(opts.focus) : inner.querySelector('input, select, textarea, .btn.primary');
      if (focusTarget) focusTarget.focus();
    });
  }
  function confirm(opts) {
    return dialog({ title: opts.title, text: opts.text, actions: [
      { label: opts.cancelLabel || 'Cancel', value: false },
      { label: opts.confirmLabel || 'Confirm', value: true, primary: !opts.danger, danger: !!opts.danger }
    ] });
  }

  // ---- fields ----
  let fieldSeq = 0;
  function field(opts) {
    const wrap = h('div', { class: 'field' + (opts.class ? ' ' + opts.class : '') });
    let lab = null;
    if (opts.label) {
      lab = h('label', { class: 'lab', for: opts.for }, opts.label);
      if (opts.optional) lab.appendChild(h('span', { class: 'opt' }, '· optional'));
      if (opts.labelExtra) append(lab, opts.labelExtra);
      wrap.appendChild(lab);
    }
    append(wrap, opts.control);
    if (lab) {
      // Point the label at the first real form control inside the field, assigning an id when it has none.
      const target = wrap.querySelector('input, select, textarea, button[role="combobox"]');
      if (target) { if (!target.id) target.id = 'fld-' + (++fieldSeq); lab.htmlFor = target.id; } else lab.removeAttribute('for');
    }
    const help = h('div', { class: 'help', hidden: !opts.help });
    if (opts.help) append(help, opts.help);
    wrap.appendChild(help);
    const err = h('div', { class: 'err', hidden: true, role: 'alert' });
    wrap.appendChild(err);
    wrap.setError = (msg) => { if (msg) { err.textContent = msg; err.hidden = false; wrap.classList.add('invalid'); } else { err.hidden = true; wrap.classList.remove('invalid'); } };
    return wrap;
  }
  const NOAUTO = { spellcheck: 'false', autocomplete: 'off', autocapitalize: 'off', autocorrect: 'off' };
  let inputSeq = 0;
  function input(opts) {
    const el = h('input', Object.assign({ class: 'in' + (opts.sans ? ' sans' : ''), type: opts.type || 'text' }, NOAUTO, opts.attrs || {}));
    el.id = opts.id || ('in-' + (++inputSeq));
    if (opts.placeholder) el.placeholder = opts.placeholder;
    if (opts.value !== undefined) el.value = opts.value;
    if (opts.disabled) el.disabled = true;
    if (opts.oninput) el.addEventListener('input', opts.oninput);
    if (opts.onchange) el.addEventListener('change', opts.onchange);
    if (opts.list) attachSuggest(el, opts.list);
    return el;
  }
  function secretInput(opts) {
    const inp = input(Object.assign({}, opts, { type: 'password' }));
    const wrap = h('div', { class: 'in-wrap' }, inp);
    const eye = h('button', { class: 'ib', type: 'button', 'aria-label': 'Show value', 'data-tip': 'Show value', 'aria-pressed': 'false', onclick: () => {
      const show = inp.type === 'password'; inp.type = show ? 'text' : 'password'; eye.setAttribute('aria-pressed', show ? 'true' : 'false');
      eye.setAttribute('aria-label', show ? 'Hide value' : 'Show value'); eye.dataset.tip = show ? 'Hide value' : 'Show value'; replace(eye, icon(show ? 'eye-off' : 'eye')); tipRefresh(eye);
    } }, icon('eye'));
    wrap.appendChild(eye);
    if (opts.copy) wrap.appendChild(copyButton(() => inp.value));
    if (opts.extra) append(wrap, opts.extra);
    wrap.input = inp;
    return wrap;
  }
  function copyButton(getText, label) {
    const b = h('button', { class: 'ib', type: 'button', 'aria-label': label || 'Copy', 'data-tip': label || 'Copy' }, icon('copy'));
    b.addEventListener('click', async () => {
      const text = typeof getText === 'function' ? getText() : getText;
      try { await navigator.clipboard.writeText(text); replace(b, icon('check')); toast('Copied'); } catch (e) { toast('Copy failed', 'bad'); }
      setTimeout(() => replace(b, icon('copy')), 1400);
    });
    return b;
  }
  // ---- dropdown popover (shared by select() and input suggestions) ----
  // One floating listbox at a time. Options keep focus on the anchor (pointerdown is prevented), so
  // blur handlers never race the click; the active option is exposed through aria-activedescendant.
  let popEl = null, popOwner = null, popOnClose = null;
  function popShow(anchor, items, opts) {
    popHide();
    const list = h('div', { class: 'sel-pop' + (opts.mono ? ' mono' : ''), role: 'listbox', id: 'ui-pop' });
    let active = opts.activeIndex !== undefined ? opts.activeIndex : items.findIndex(it => it.selected);
    const nodes = items.map((it, i) => {
      const o = h('div', { class: 'sel-o', role: 'option', id: 'ui-pop-' + i, 'aria-selected': String(!!it.selected), 'aria-disabled': it.disabled ? 'true' : undefined },
        h('span', { class: 'sel-l' }, it.label), it.hint ? h('span', { class: 'sel-h' }, it.hint) : null, icon('check', 'sel-ok'));
      if (!it.disabled) {
        o.addEventListener('pointerdown', (e) => e.preventDefault());
        o.addEventListener('click', () => pick(i));
        o.addEventListener('mousemove', () => { if (active !== i) setActive(i, false); });
      }
      return o;
    });
    nodes.forEach(n => list.appendChild(n));
    (anchor.closest('dialog[open]') || document.body).appendChild(list);
    list.style.fontSize = getComputedStyle(anchor).fontSize;
    function place() {
      const a = anchor.getBoundingClientRect();
      list.style.minWidth = Math.round(a.width) + 'px';
      const w = list.offsetWidth, ht = list.offsetHeight;
      const vw = document.documentElement.clientWidth, vh = window.innerHeight;
      const above = a.bottom + 6 + ht > vh - 8 && a.top - 6 - ht >= 8;
      list.style.left = Math.max(8, Math.min(Math.round(a.left), vw - 8 - w)) + 'px';
      list.style.top = (above ? Math.round(a.top - 6 - ht) : Math.round(a.bottom + 6)) + 'px';
      list.classList.toggle('above', above);
    }
    function setActive(i, scroll) {
      active = i;
      nodes.forEach((n, k) => n.classList.toggle('active', k === i));
      if (i >= 0) anchor.setAttribute('aria-activedescendant', 'ui-pop-' + i); else anchor.removeAttribute('aria-activedescendant');
      if (scroll && i >= 0) nodes[i].scrollIntoView({ block: 'nearest' });
    }
    function move(delta) {
      if (!items.length) return;
      let i = active < 0 && delta < 0 ? 0 : active;
      for (let n = 0; n < items.length; n++) { i = (i + delta + items.length) % items.length; if (!items[i].disabled) break; }
      setActive(i, true);
    }
    function pick(i) { if (i < 0 || !items[i] || items[i].disabled) return; const it = items[i]; popHide(); opts.onPick(it, i); }
    place();
    requestAnimationFrame(() => list.classList.add('show'));
    setActive(active, true);
    anchor.setAttribute('aria-expanded', 'true');
    popEl = list; popOwner = anchor;
    popOnClose = () => { anchor.removeAttribute('aria-activedescendant'); anchor.setAttribute('aria-expanded', 'false'); if (opts.onClose) opts.onClose(); };
    return { move, setActive, pickActive: () => pick(active), get active() { return active; } };
  }
  function popHide() {
    if (!popEl) return;
    const el = popEl, done = popOnClose;
    popEl = null; popOwner = null; popOnClose = null;
    el.remove();
    if (done) done();
  }
  document.addEventListener('pointerdown', (e) => { if (popEl && !popEl.contains(e.target) && !(popOwner && popOwner.contains(e.target))) popHide(); }, true);
  document.addEventListener('scroll', (e) => { if (popEl && !popEl.contains(e.target)) popHide(); }, true);
  window.addEventListener('resize', () => popHide());
  window.addEventListener('blur', () => popHide());

  // Custom select: a button trigger sized to its longest option (hidden sizer stacked under the value) and
  // a themed listbox. Exposes the native-like surface callers use: .value, change events, setOptions().
  let selectSeq = 0;
  function select(opts) {
    const id = opts.id || ('sel-' + (++selectSeq));
    let options = (opts.options || []).slice();
    let value = opts.value;
    const valueEl = h('span', { class: 'sel-v' });
    const sizer = h('span', { class: 'sel-sizer', 'aria-hidden': 'true' });
    const el = h('button', { class: 'sel in', type: 'button', id, role: 'combobox', 'aria-haspopup': 'listbox', 'aria-expanded': 'false', disabled: !!opts.disabled },
      h('span', { class: 'sel-box' }, valueEl, sizer), icon('chevron-down', 'sel-c'));
    let pop = null, typed = '', typedTimer = 0;
    const current = () => options.find(o => String(o.value) === String(value));
    function render() {
      const cur = current();
      valueEl.textContent = cur ? cur.label : (options.length ? options[0].label : '');
      replace(sizer, options.map(o => h('span', null, o.label)));
    }
    function setValue(v, emit) {
      const changed = String(v) !== String(value);
      value = v; render();
      if (emit && changed) { if (opts.onchange) opts.onchange({ target: el, value: v }); el.dispatchEvent(new Event('change', { bubbles: true })); }
    }
    function open() {
      if (el.disabled || pop) return;
      pop = popShow(el, options.map(o => ({ label: o.label, hint: o.hint, value: o.value, disabled: !!o.disabled, selected: String(o.value) === String(value) })), {
        onPick: (it) => setValue(it.value, true), onClose: () => { pop = null; } });
    }
    el.addEventListener('click', () => { if (pop) popHide(); else open(); });
    el.addEventListener('blur', () => { if (pop) popHide(); });
    el.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); if (!pop) open(); else pop.move(e.key === 'ArrowDown' ? 1 : -1); return; }
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); if (!pop) open(); else pop.pickActive(); return; }
      if (e.key === 'Home' || e.key === 'End') { if (pop) { e.preventDefault(); pop.setActive(e.key === 'Home' ? 0 : options.length - 1, true); } return; }
      if (e.key === 'Escape' || e.key === 'Tab') {
        if (pop) { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); } popHide(); }
        return;
      }
      if (e.key.length === 1 && !e.metaKey && !e.ctrlKey && !e.altKey) {
        // Type-ahead like a native select: jump to the first option whose label starts with what was typed.
        clearTimeout(typedTimer); typed += e.key.toLowerCase(); typedTimer = setTimeout(() => { typed = ''; }, 600);
        const i = options.findIndex(o => !o.disabled && String(o.label).toLowerCase().startsWith(typed));
        if (i >= 0) { if (pop) pop.setActive(i, true); else setValue(options[i].value, true); }
        e.preventDefault();
      }
    });
    Object.defineProperty(el, 'value', { get: () => value, set: (v) => setValue(v, false) });
    el.setOptions = (next) => { options = next.slice(); if (!current()) value = options.length ? options[0].value : undefined; render(); };
    render();
    return el;
  }

  // Suggestions for text inputs, replacing the native <datalist> popup. The <datalist id> stays the data
  // source so callers keep populating it; matching is a case-insensitive substring of the typed value.
  function attachSuggest(inp, listId) {
    inp.dataset.list = listId;
    inp.setAttribute('role', 'combobox'); inp.setAttribute('aria-autocomplete', 'list'); inp.setAttribute('aria-expanded', 'false');
    let pop = null;
    const values = () => { const dl = document.getElementById(listId); return dl ? Array.from(dl.querySelectorAll('option')).map(o => o.value).filter(Boolean) : []; };
    function open() {
      const q = inp.value.trim().toLowerCase();
      const all = values();
      const hits = (q ? all.filter(v => v.toLowerCase().includes(q)) : all).slice(0, 12);
      if (pop) popHide();
      if (!hits.length || (hits.length === 1 && hits[0] === inp.value)) return;
      pop = popShow(inp, hits.map(v => ({ label: v, value: v, selected: v === inp.value })), { mono: true, activeIndex: -1,
        onPick: (it) => { inp.value = it.value; inp.dispatchEvent(new Event('input', { bubbles: true })); inp.dispatchEvent(new Event('change', { bubbles: true })); inp.focus(); },
        onClose: () => { pop = null; } });
    }
    inp.addEventListener('focus', open);
    inp.addEventListener('input', open);
    inp.addEventListener('click', () => { if (!pop) open(); });
    inp.addEventListener('blur', () => { if (pop) popHide(); });
    inp.addEventListener('keydown', (e) => {
      if (!pop) { if (e.key === 'ArrowDown') { e.preventDefault(); open(); } return; }
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); pop.move(e.key === 'ArrowDown' ? 1 : -1); }
      else if (e.key === 'Enter') { if (pop.active >= 0) { e.preventDefault(); pop.pickActive(); } else popHide(); }
      else if (e.key === 'Escape' || e.key === 'Tab') {
        if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); }
        popHide();
      }
    });
  }

  function seg(opts) {
    const wrap = h('div', { class: 'seg' + (opts.class ? ' ' + opts.class : ''), role: 'radiogroup', 'aria-label': opts.ariaLabel || '' });
    let value = opts.value;
    // An icon-only option has no text node to name it, so it takes its accessible name from the hover text (or,
    // failing that, its value). A tooltip is not an accessible name.
    const buttons = opts.options.map(o => h('button', { type: 'button', role: 'radio', 'aria-checked': String(o.value === value), dataset: { value: o.value }, disabled: !!o.disabled, 'data-tip': o.title || undefined, 'aria-label': o.label ? undefined : (o.title || o.aria || String(o.value)) },
      o.icon ? icon(o.icon) : null, o.label, o.count !== undefined ? h('span', { class: 'seg-c' }, String(o.count)) : null));
    const setValue = (v, emit) => {
      value = v; buttons.forEach(b => { b.setAttribute('aria-checked', String(b.dataset.value === String(v))); b.tabIndex = b.dataset.value === String(v) ? 0 : -1; });
      if (emit && opts.onchange) opts.onchange(v);
    };
    buttons.forEach((b, i) => {
      b.addEventListener('click', () => setValue(b.dataset.value, true));
      b.addEventListener('keydown', (e) => {
        if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft' && e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
        e.preventDefault();
        const dir = (e.key === 'ArrowRight' || e.key === 'ArrowDown') ? 1 : -1;
        let j = i; do { j = (j + dir + buttons.length) % buttons.length; } while (buttons[j].disabled && j !== i);
        buttons[j].focus(); setValue(buttons[j].dataset.value, true);
      });
      wrap.appendChild(b);
    });
    setValue(value, false);
    wrap.setValue = (v) => setValue(v, false);
    wrap.getValue = () => value;
    return wrap;
  }

  let switchSeq = 0;
  function switchCtl(opts) {
    const inp = h('input', { type: 'checkbox', checked: !!opts.checked, disabled: !!opts.disabled, id: opts.id || ('sw-' + (++switchSeq)), name: opts.name || opts.id || ('sw-' + switchSeq) });
    const lab = h('label', { class: 'sw' + (opts.class ? ' ' + opts.class : ''), 'data-tip': opts.tip || undefined }, inp, h('span', { class: 'track' }),
      opts.label ? h('span', null, opts.label, opts.sub ? h('span', { class: 'sw-sub' }, ' · ' + opts.sub) : null) : null);
    if (opts.onchange) inp.addEventListener('change', () => opts.onchange(inp.checked, inp));
    lab.input = inp;
    return lab;
  }

  function chipEditor(opts) {
    let values = (opts.values || []).slice();
    const inp = h('input', Object.assign({ type: 'text', placeholder: opts.placeholder || '' }, NOAUTO));
    if (opts.list) inp.setAttribute('list', opts.list);
    const wrap = h('div', { class: 'in-wrap chips-in', onclick: (e) => { if (e.target === wrap) inp.focus(); } });
    const render = () => {
      clear(wrap);
      values.forEach((v, i) => wrap.appendChild(h('span', { class: 'ch' }, v, h('button', { type: 'button', 'aria-label': 'Remove ' + v, onclick: () => { values.splice(i, 1); render(); emit(); } }, icon('x')))));
      wrap.appendChild(inp);
    };
    const emit = () => opts.onchange && opts.onchange(values.slice());
    const commit = () => {
      const raw = inp.value.split(/[,\s]+/).map(s => s.trim()).filter(Boolean);
      let changed = false;
      raw.forEach(v => { if (!values.includes(v)) { values.push(v); changed = true; } });
      inp.value = '';
      if (changed) { render(); emit(); inp.focus(); }
    };
    inp.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); commit(); }
      else if (e.key === 'Backspace' && !inp.value && values.length) { values.pop(); render(); emit(); inp.focus(); }
    });
    inp.addEventListener('blur', commit);
    render();
    wrap.getValues = () => values.slice();
    wrap.setValues = (v) => { values = v.slice(); render(); };
    return wrap;
  }

  function kv(rows, opts) {
    const dl = h('dl', { class: 'kv' + (opts && opts.compact ? ' compact' : '') });
    rows.forEach(r => {
      if (!r) return;
      dl.appendChild(h('dt', null, r[0]));
      const dd = h('dd', { class: r[2] || '' });
      append(dd, r[1] === undefined || r[1] === null || r[1] === '' ? h('span', { class: 'faint' }, '—') : r[1]);
      dl.appendChild(dd);
    });
    return dl;
  }

  function banner(kind, title, text, actions) {
    const b = h('div', { class: 'banner ' + kind, role: kind === 'bad' ? 'alert' : 'status' }, icon(kind === 'info' ? 'info' : 'alert'),
      h('div', { class: 'b-body' }, title ? h('div', { class: 'b-title' }, title) : null, h('div', null, text)));
    if (actions && actions.length) b.appendChild(h('div', { class: 'b-actions' }, actions));
    return b;
  }

  function empty(opts) {
    return h('div', { class: 'empty' }, opts.mascot === false ? null : mascot(), h('h3', null, opts.title), h('p', null, opts.text), opts.action || null);
  }
  function skeleton(lines, height) {
    const s = h('div', { class: 'stack-skel' });
    for (let i = 0; i < (lines || 3); i++) s.appendChild(h('div', { class: 'skel', style: { height: (height || 44) + 'px', marginTop: i ? '10px' : '0', width: (100 - (i % 3) * 8) + '%' } }));
    return s;
  }
  function errorCard(message, retry) {
    return h('div', { class: 'card err-card' }, h('div', { class: 'banner bad' }, icon('alert'), h('div', { class: 'b-body' }, h('div', { class: 'b-title' }, 'Could not load'), h('div', null, message)),
      retry ? h('div', { class: 'b-actions' }, h('button', { class: 'btn sm', type: 'button', onclick: retry }, icon('refresh'), 'Retry')) : null));
  }

  function savebar(opts) {
    const msg = h('div', { class: 'msg' }, h('span', { class: 'dot' }), h('span', { class: 'msg-text' }, 'Unsaved changes'));
    const revert = h('button', { class: 'btn', type: 'button', onclick: () => opts.onRevert && opts.onRevert() }, 'Revert');
    const save = h('button', { class: 'btn primary', type: 'button', onclick: () => opts.onSave && opts.onSave() }, 'Save changes');
    const bar = h('div', { class: 'savebar', hidden: true }, h('div', { class: 'in-bar' }, msg, h('div', { class: 'acts' }, revert, save)));
    document.body.appendChild(bar);
    const ctl = {
      el: bar,
      show(on) { bar.hidden = !on; if (!on) ctl.setError(''); },
      setError(text) { msg.classList.toggle('error', !!text); msg.querySelector('.msg-text').textContent = text || 'Unsaved changes'; },
      busy(on) { save.disabled = on; revert.disabled = on; replace(save, on ? [h('span', { class: 'spin' }), 'Saving…'] : 'Save changes'); },
      destroy() { bar.remove(); }
    };
    return ctl;
  }

  function overlay(title, text) {
    const el = h('div', { class: 'overlay', role: 'alert' }, h('div', { class: 'box' }, h('span', { class: 'spin' }), h('div', null, h('h3', null, title), h('p', null, text))));
    document.body.appendChild(el);
    return { update(t, x) { el.querySelector('h3').textContent = t; if (x !== undefined) el.querySelector('p').textContent = x; }, close() { el.remove(); } };
  }

  // ---- tooltip ----
  // One shared floating card. Anchors either carry `data-tip="text"` (\n breaks lines) or register
  // rich/lazy content with tip(el, content). Shows on hover or keyboard focus after a short delay and
  // hides on leave, blur, Escape, scroll, resize or a pointer press elsewhere.
  const TIP_DELAY = 120, TIP_WARM = 300;
  const HOVER_NONE = window.matchMedia ? window.matchMedia('(hover: none)') : { matches: false };
  let tipEl = null, tipAnchor = null, tipPending = null, tipTimer = 0, tipTick = 0, tipHiddenAt = 0, tipPointer = null;

  function tipContentOf(anchor) {
    if (anchor._tip !== undefined) return typeof anchor._tip === 'function' ? anchor._tip(anchor) : anchor._tip;
    return anchor.dataset.tip || '';
  }
  function tipRender(anchor) {
    const body = tipEl.firstChild;
    clear(body);
    const content = tipContentOf(anchor);
    if (content === null || content === undefined || content === '') return false;
    if (typeof content === 'string') { body.classList.add('text'); body.textContent = content; }
    else { body.classList.remove('text'); append(body, content); }
    return true;
  }
  function tipPlace(anchor) {
    const pad = 8, gap = 9;
    const a = anchor.getBoundingClientRect();
    const w = tipEl.offsetWidth, ht = tipEl.offsetHeight;
    const vw = document.documentElement.clientWidth, vh = window.innerHeight;
    let below = anchor.dataset.tipPos === 'bottom';
    if (!below && a.top - gap - ht < pad) below = true;
    if (below && a.bottom + gap + ht > vh - pad && a.top - gap - ht >= pad) below = false;
    const cx = a.left + a.width / 2;
    const left = Math.max(pad, Math.min(Math.round(cx - w / 2), vw - pad - w));
    const top = below ? Math.round(a.bottom + gap) : Math.round(a.top - gap - ht);
    tipEl.style.left = left + 'px'; tipEl.style.top = top + 'px';
    tipEl.classList.toggle('below', below);
    tipEl.lastChild.style.left = Math.max(14, Math.min(w - 14, Math.round(cx - left))) + 'px';
  }
  function tipShow(anchor) {
    if (!tipEl) tipEl = h('div', { class: 'tip', role: 'tooltip', id: 'ui-tip' }, h('div', { class: 'tip-in' }), h('i', { class: 'tip-arrow', 'aria-hidden': 'true' }));
    // Inside an open modal the tooltip has to live in the top layer with the dialog.
    const hostEl = anchor.closest('dialog[open]') || document.body;
    if (tipEl.parentNode !== hostEl) hostEl.appendChild(tipEl);
    if (!tipRender(anchor)) return;
    tipAnchor = anchor; tipPending = null;
    if (!anchor.hasAttribute('aria-describedby')) { anchor.setAttribute('aria-describedby', tipEl.id); anchor._tipDescribed = true; }
    tipPlace(anchor);
    tipEl.classList.add('show');
    clearInterval(tipTick);
    tipTick = setInterval(() => {
      if (tipAnchor !== anchor) { clearInterval(tipTick); return; }
      if (!anchor.isConnected) {
        // A polling page re-rendered under the pointer: continue with whatever hover target is now beneath it.
        const under = tipPointer ? tipAnchorFrom(document.elementFromPoint(tipPointer.x, tipPointer.y)) : null;
        if (under) { tipAnchor = null; tipShow(under); } else tipHide();
        return;
      }
      if (anchor._tipLive) { tipRender(anchor); tipPlace(anchor); }
    }, 1000);
  }
  function tipHide() {
    clearTimeout(tipTimer); tipTimer = 0; tipPending = null;
    clearInterval(tipTick); tipTick = 0;
    if (tipAnchor) {
      if (tipAnchor._tipDescribed) { tipAnchor.removeAttribute('aria-describedby'); tipAnchor._tipDescribed = false; }
      tipAnchor = null; tipHiddenAt = Date.now();
    }
    if (tipEl) tipEl.classList.remove('show');
  }
  function tipAnchorFrom(target) {
    let el = target instanceof Element ? target : null;
    while (el && el !== document.body) { if (el._tip !== undefined || el.hasAttribute('data-tip')) return el; el = el.parentElement; }
    return null;
  }
  function tipSchedule(anchor, immediate) {
    if (anchor === tipAnchor || anchor === tipPending) return;
    clearTimeout(tipTimer); tipTimer = 0;
    const warm = tipAnchor !== null || Date.now() - tipHiddenAt < TIP_WARM;
    if (tipAnchor) tipHide();
    if (immediate || warm) { tipShow(anchor); return; }
    tipPending = anchor;
    tipTimer = setTimeout(() => {
      tipTimer = 0; tipPending = null;
      const target = anchor.isConnected ? anchor : (tipPointer ? tipAnchorFrom(document.elementFromPoint(tipPointer.x, tipPointer.y)) : null);
      if (target) tipShow(target);
    }, TIP_DELAY);
  }
  document.addEventListener('mousemove', (e) => { tipPointer = { x: e.clientX, y: e.clientY }; }, { passive: true });
  document.addEventListener('mouseover', (e) => {
    if (HOVER_NONE.matches) return;
    const anchor = tipAnchorFrom(e.target);
    if (anchor) tipSchedule(anchor, false);
    else if (tipAnchor || tipPending) tipHide();
  });
  document.addEventListener('mouseout', (e) => {
    const anchor = tipAnchor || tipPending;
    if (!anchor || !anchor.contains(e.target)) return;
    if (e.relatedTarget && anchor.contains(e.relatedTarget)) return;
    tipHide();
  });
  document.addEventListener('focusin', (e) => { const anchor = tipAnchorFrom(e.target); if (anchor && e.target.matches(':focus-visible')) tipSchedule(anchor, true); });
  document.addEventListener('focusout', (e) => { if (tipAnchor && tipAnchor.contains(e.target)) tipHide(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') tipHide(); });
  document.addEventListener('pointerdown', (e) => { if (tipAnchor && !tipAnchor.contains(e.target)) tipHide(); });
  document.addEventListener('scroll', () => { if (tipAnchor || tipPending) tipHide(); }, true);
  window.addEventListener('resize', () => { if (tipAnchor) tipHide(); });

  // Rich or lazy tooltip content: a string, Node, array, or a function returning one. Functions run on
  // every show, and once a second while visible when `live` is set, so countdowns stay current.
  function tip(el, content, opts) {
    el._tip = content;
    el._tipLive = !!(opts && opts.live);
    if (opts && opts.pos) el.dataset.tipPos = opts.pos;
    return el;
  }
  function tipRefresh(el) { if (tipAnchor === el) { tipRender(el); tipPlace(el); } }
  // Disabled controls receive no pointer events; wrap them so the explanation still shows.
  function wrapTip(el, text) { return h('span', { class: 'tipwrap', 'data-tip': text }, el); }
  // Structured tooltip body: title line, key/value rows, mono list, muted note.
  function tipBlock(spec) {
    const out = h('div', { class: 'tip-b' });
    if (spec.title) out.appendChild(h('div', { class: 'tip-t' }, spec.title));
    if (spec.rows && spec.rows.some(Boolean)) {
      const dl = h('dl', { class: 'tip-kv' });
      spec.rows.forEach(r => { if (!r) return; dl.appendChild(h('dt', null, r[0])); dl.appendChild(h('dd', { class: r[2] || '' }, r[1] === undefined || r[1] === null || r[1] === '' ? '—' : r[1])); });
      out.appendChild(dl);
    }
    if (spec.list && spec.list.length) out.appendChild(h('ul', { class: 'tip-list' }, spec.list.map(x => h('li', null, x))));
    if (spec.note) out.appendChild(h('div', { class: 'tip-n' }, spec.note));
    return out;
  }

  // Parse "providers[0].base_url: must not be empty" into a field path and message.
  function splitFieldError(message) {
    const m = /^([a-zA-Z0-9_.\[\]]+):\s*(.*)$/.exec(message || '');
    if (!m) return { field: null, message: message };
    return { field: m[1].replace(/^providers\[\d+\]\./, '').replace(/^auto_mode\./, '').replace(/^harnesses\.claude_code\./, '').replace(/^fixed_provider\./, 'fixed.'), message: m[2] };
  }

  CCAM.ui = { closeOverlays() { popHide(); tipHide(); }, h, append, clear, replace, frag, icon, mascot, pill, healthPill, healthDot, tag, toast, dialog, confirm, field, input, secretInput, copyButton, select, seg, switchCtl, chipEditor, kv, banner, empty, skeleton, errorCard, savebar, overlay, splitFieldError, tip, tipBlock, tipRefresh, wrapTip, HEALTH_KIND, HEALTH_TEXT, NOAUTO };
})();
