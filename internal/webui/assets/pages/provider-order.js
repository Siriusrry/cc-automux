/* Provider list sorting: pointer and keyboard share one transient order. */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  CCAM.providerOrder = function (opts) {
    let drag = null, frame = 0, disposed = false;
    const animations = new Set();
    const reduced = window.matchMedia('(prefers-reduced-motion: reduce)');
    const announce = text => { opts.live.textContent = text; };
    function begin(id, handle, pointer) {
      if (disposed || drag || !opts.canStart()) return false;
      animations.forEach(a => a.cancel()); animations.clear();
      const tier = opts.tier(id);
      if (!tier || tier.ids.length < 2) return false;
      const rects = new Map(tier.ids.map(key => [key, opts.row(key).getBoundingClientRect()]));
      const row = opts.row(id), rect = rects.get(id), bounds = tier.rows.getBoundingClientRect();
      drag = { id, handle, tier, rects, order: tier.ids.slice(), pointer, startY: pointer ? pointer.y + window.scrollY : 0, startX: pointer ? pointer.x : 0,
        scrollY: window.scrollY, top: rect.top + window.scrollY, bounds, lifted: !pointer, valid: true, delta: 0 };
      if (!pointer) lift();
      opts.onState(id);
      return true;
    }
    function lift() {
      drag.lifted = true;
      drag.tier.rows.classList.add('sorting');
      drag.tier.rows.classList.toggle('sorting-pointer', !!drag.pointer);
      opts.row(drag.id).classList.add('sorting-item');
      drag.handle.setAttribute('aria-pressed', 'true');
      announce('Picked up ' + opts.name(drag.id) + '. Use arrow keys to move, Enter to save, Escape to cancel.');
    }
    function positions() {
      const d = drag, first = d.rects.get(d.tier.ids[0]);
      const gap = d.tier.ids.length > 1 ? d.rects.get(d.tier.ids[1]).top - first.bottom : 0;
      let top = first.top;
      const result = new Map();
      d.order.forEach(id => { result.set(id, top); top += d.rects.get(id).height + gap; });
      return result;
    }
    function paint() {
      if (!drag || !drag.lifted) return;
      const d = drag, target = positions();
      d.order.forEach(id => {
        const delta = id === d.id && d.pointer ? d.delta : target.get(id) - d.rects.get(id).top;
        opts.row(id).style.transform = 'translate3d(0,' + delta + 'px,0)';
      });
      opts.row(d.id).classList.toggle('sort-invalid', !d.valid);
    }
    function pointerFrame() {
      frame = 0;
      if (!drag || !drag.pointer || !drag.lifted) return;
      const d = drag, { x, y } = d.pointer;
      const edge = 64, speed = 14;
      const scroll = y < edge ? -speed * Math.min(1, (edge - y) / edge) : y > innerHeight - edge ? speed * Math.min(1, (y - innerHeight + edge) / edge) : 0;
      const beforeScroll = window.scrollY;
      if (scroll && d.valid) window.scrollBy(0, scroll);
      const docY = y + window.scrollY;
      d.delta = docY - d.startY;
      d.valid = x >= d.bounds.left && x <= d.bounds.right && docY >= d.bounds.top + d.scrollY && docY <= d.bounds.bottom + d.scrollY;
      if (d.valid) {
        const center = d.top + d.delta + d.rects.get(d.id).height / 2;
        const others = d.tier.ids.filter(id => id !== d.id);
        const index = others.filter(id => center > d.rects.get(id).top + d.scrollY + d.rects.get(id).height / 2).length;
        d.order = others.slice(); d.order.splice(index, 0, d.id);
      }
      paint();
      if (window.scrollY !== beforeScroll) frame = requestAnimationFrame(pointerFrame);
    }
    function animateFrom(before, nodes) {
      if (disposed || reduced.matches) return;
      const duration = parseFloat(getComputedStyle(opts.live).getPropertyValue('--motion-position')) || 240;
      const easing = getComputedStyle(opts.live).getPropertyValue('--motion-ease').trim();
      const after = nodes.map(node => node.getBoundingClientRect());
      nodes.forEach((node, i) => {
        const dy = before[i].top - after[i].top;
        if (!before[i].height || Math.abs(dy) < 0.5) return;
        const animation = node.animate([{ transform: 'translateY(' + dy + 'px)' }, { transform: 'translateY(0)' }], { duration, easing });
        animations.add(animation); animation.finished.catch(() => {}).finally(() => animations.delete(animation));
      });
    }
    function finish(commit) {
      if (!drag) return;
      cancelAnimationFrame(frame); frame = 0;
      const d = drag; drag = null;
      const changed = d.lifted && d.valid && commit && d.order.some((id, i) => id !== d.tier.ids[i]);
      const order = changed ? d.order : d.tier.ids;
      const nodes = order.map(opts.row), before = nodes.map(node => node.getBoundingClientRect());
      d.tier.rows.classList.remove('sorting', 'sorting-pointer');
      nodes.forEach(node => { node.classList.remove('sorting-item', 'sort-invalid'); node.style.transform = ''; });
      order.forEach((id, i) => { const node = opts.row(id); if (d.tier.rows.children[i] !== node) d.tier.rows.insertBefore(node, d.tier.rows.children[i] || null); });
      d.handle.setAttribute('aria-pressed', 'false');
      if (d.pointer && d.handle.hasPointerCapture(d.pointer.id)) d.handle.releasePointerCapture(d.pointer.id);
      d.handle.focus({ preventScroll: true });
      animateFrom(before, nodes);
      if (changed) { announce('Saving provider order.'); opts.onCommit(order, d.tier.configETag, d.id); }
      else { announce('Order unchanged.'); opts.onState(null); }
    }
    function bind(id, handle) {
      handle.addEventListener('click', e => { e.preventDefault(); e.stopPropagation(); });
      handle.addEventListener('pointerdown', e => {
        if (e.button !== 0 || !begin(id, handle, { id: e.pointerId, x: e.clientX, y: e.clientY })) return;
        e.preventDefault(); handle.focus(); handle.setPointerCapture(e.pointerId);
      });
      handle.addEventListener('pointermove', e => {
        if (!drag || !drag.pointer || drag.pointer.id !== e.pointerId) return;
        drag.pointer.x = e.clientX; drag.pointer.y = e.clientY;
        if (!drag.lifted && Math.hypot(e.clientY + window.scrollY - drag.startY, e.clientX - drag.startX) >= 5) lift();
        if (!frame && drag.lifted) frame = requestAnimationFrame(pointerFrame);
      });
      handle.addEventListener('pointerup', e => {
        if (!drag || !drag.pointer || drag.pointer.id !== e.pointerId) return;
        cancelAnimationFrame(frame); frame = 0;
        drag.pointer.x = e.clientX; drag.pointer.y = e.clientY; pointerFrame(); finish(true);
      });
      handle.addEventListener('pointercancel', () => finish(false));
      handle.addEventListener('lostpointercapture', () => finish(false));
      handle.addEventListener('keydown', e => {
        if (e.key === 'Escape' || e.key === 'Tab') { if (drag) { if (e.key === 'Escape') e.preventDefault(); finish(false); } return; }
        if (e.key === ' ' || e.key === 'Enter') { e.preventDefault(); if (drag) finish(true); else begin(id, handle, null); return; }
        if (!drag || drag.pointer || !['ArrowUp', 'ArrowDown'].includes(e.key)) return;
        e.preventDefault();
        const from = drag.order.indexOf(id), to = Math.max(0, Math.min(drag.order.length - 1, from + (e.key === 'ArrowUp' ? -1 : 1)));
        drag.order.splice(from, 1); drag.order.splice(to, 0, id); paint();
        announce(opts.name(id) + ', position ' + (to + 1) + ' of ' + drag.order.length + '.');
      });
    }
    const cancel = () => finish(false);
    const escape = e => { if (e.key === 'Escape' && drag) { e.preventDefault(); finish(false); } };
    window.addEventListener('resize', cancel); document.addEventListener('keydown', escape);
    return { bind, reconcile(nodes, mutate) { const before = nodes.map(node => node.getBoundingClientRect()); mutate(); animateFrom(before, nodes); }, destroy() { disposed = true; finish(false); cancelAnimationFrame(frame); animations.forEach(a => a.cancel()); animations.clear(); window.removeEventListener('resize', cancel); document.removeEventListener('keydown', escape); } };
  };
})();
