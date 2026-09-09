/* CC AutoMux Web UI — formatting helpers. */
(function () {
  'use strict';
  const CCAM = (window.CCAM = window.CCAM || {});

  function pad(n, w) { return String(n).padStart(w || 2, '0'); }

  function duration(seconds) {
    seconds = Math.max(0, Math.floor(Number(seconds) || 0));
    const d = Math.floor(seconds / 86400), h = Math.floor((seconds % 86400) / 3600), m = Math.floor((seconds % 3600) / 60), s = seconds % 60;
    if (d > 0) return d + 'd ' + h + 'h';
    if (h > 0) return h + 'h ' + pad(m) + 'm';
    if (m > 0) return m + 'm ' + pad(s) + 's';
    return s + 's';
  }

  function parse(iso) { const d = new Date(iso); return isNaN(d.getTime()) ? null : d; }

  function timeShort(iso) {
    const d = parse(iso); if (!d) return '—';
    return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds()) + '.' + pad(d.getMilliseconds(), 3);
  }
  function clock(iso) {
    const d = parse(iso); if (!d) return '—';
    return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }
  function dateTime(iso) {
    const d = parse(iso); if (!d) return '—';
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }
  function relative(iso, now) {
    const d = parse(iso); if (!d) return '—';
    const diff = Math.round(((now || Date.now()) - d.getTime()) / 1000);
    const abs = Math.abs(diff);
    let text;
    if (abs < 5) text = 'just now';
    else if (abs < 60) text = abs + 's';
    else if (abs < 3600) text = Math.floor(abs / 60) + 'm';
    else if (abs < 86400) text = Math.floor(abs / 3600) + 'h';
    else text = Math.floor(abs / 86400) + 'd';
    if (text === 'just now') return text;
    return diff >= 0 ? text + ' ago' : 'in ' + text;
  }
  function untilShort(iso, now) {
    const d = parse(iso); if (!d) return '';
    const s = Math.max(0, Math.round((d.getTime() - (now || Date.now())) / 1000));
    if (s >= 3600) return Math.floor(s / 3600) + 'h ' + pad(Math.floor((s % 3600) / 60)) + 'm';
    if (s >= 60) return Math.floor(s / 60) + 'm ' + pad(s % 60) + 's';
    return s + 's';
  }

  function bytes(n) {
    n = Number(n) || 0;
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(n < 10240 ? 1 : 0) + ' KiB';
    if (n < 1024 * 1024 * 1024) return (n / 1048576).toFixed(n < 10485760 ? 1 : 0) + ' MiB';
    return (n / 1073741824).toFixed(1) + ' GiB';
  }
  function bytesToMB(n) { return Math.round((Number(n) || 0) / 1048576); }
  function mbToBytes(mb) { return Math.round(Number(mb) || 0) * 1048576; }

  function mask(value) {
    if (!value) return '';
    if (value.length <= 8) return '•'.repeat(value.length);
    return value.slice(0, 4) + '•'.repeat(Math.min(12, value.length - 7)) + value.slice(-3);
  }
  function middle(value, max) {
    value = String(value || ''); max = max || 22;
    if (value.length <= max) return value;
    const head = Math.ceil((max - 1) / 2), tail = Math.floor((max - 1) / 2);
    return value.slice(0, head) + '…' + value.slice(value.length - tail);
  }
  function host(url) {
    try { const u = new URL(url); return u.host + (u.pathname !== '/' ? u.pathname.replace(/\/$/, '') : ''); } catch (e) { return url || ''; }
  }
  function plural(n, word, words) { return n + ' ' + (n === 1 ? word : (words || word + 's')); }
  function cap(s) { s = String(s || ''); return s.charAt(0).toUpperCase() + s.slice(1); }
  function words(s) { return String(s || '').replace(/_/g, ' '); }
  function pretty(obj) { try { return JSON.stringify(obj, null, 2); } catch (e) { return String(obj); } }
  function priorityLabel(p) { return String(p); }

  function randomKey(bytesLen) {
    const arr = new Uint8Array(bytesLen || 32);
    (window.crypto || window.msCrypto).getRandomValues(arr);
    let s = ''; for (let i = 0; i < arr.length; i++) s += String.fromCharCode(arr[i]);
    return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }
  function uuid() {
    if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
    const b = new Uint8Array(16); crypto.getRandomValues(b); b[6] = (b[6] & 0x0f) | 0x40; b[8] = (b[8] & 0x3f) | 0x80;
    const h = Array.from(b, x => x.toString(16).padStart(2, '0')).join('');
    return h.slice(0, 8) + '-' + h.slice(8, 12) + '-' + h.slice(12, 16) + '-' + h.slice(16, 20) + '-' + h.slice(20);
  }

  // RFC3339 fractions have variable width. Retain nanoseconds instead of
  // sorting the strings or discarding precision through Date alone.
  function timestamp(iso) {
    const m = /^(.*T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/.exec(iso || '');
    if (!m) return [0, 0];
    return [Date.parse(m[1] + m[3]), Number((m[2] || '').padEnd(9, '0'))];
  }
  function compareRecords(a, b) {
    const x = timestamp(a.time), y = timestamp(b.time);
    return x[0] - y[0] || x[1] - y[1] || a.seq - b.seq;
  }

  CCAM.fmt = { compareRecords, pad, duration, timeShort, clock, dateTime, relative, untilShort, bytes, bytesToMB, mbToBytes, mask, middle, host, plural, cap, words, pretty, priorityLabel, randomKey, uuid };
})();
