/* Login page: management key entry. */
(function () {
  'use strict';
  const CCAM = window.CCAM;
  const { h, icon, mascot, field, secretInput, replace, clear } = CCAM.ui;
  CCAM.pages = CCAM.pages || {};

  function configPathHint() {
    const p = (navigator.platform || '') + ' ' + (navigator.userAgent || '');
    if (/Mac/i.test(p)) return '~/Library/Application Support/cc-automux/config.json';
    if (/Linux/i.test(p)) return '~/.config/cc-automux/config.json';
    return '<user config dir>/cc-automux/config.json';
  }

  CCAM.pages.login = {
    render(ctx) {
      clear(ctx.root);
      document.title = 'Sign in · CC AutoMux';
      const keyWrap = secretInput({ id: 'mkey', placeholder: 'management key', attrs: { 'aria-label': 'Management key', autofocus: true } });
      const remember = h('input', { type: 'checkbox', id: 'remember', checked: CCAM.auth.isRemembered() });
      const err = h('div', { class: 'login-err', role: 'alert', hidden: true });
      const submit = h('button', { class: 'btn primary block', type: 'submit', disabled: true }, 'Sign in');
      const setBusy = (on) => { submit.disabled = on || !keyWrap.input.value.trim(); replace(submit, on ? [h('span', { class: 'spin' }), 'Checking…'] : 'Sign in'); };
      keyWrap.input.addEventListener('input', () => { submit.disabled = !keyWrap.input.value.trim(); err.hidden = true; });

      const form = h('form', { class: 'login-form', novalidate: true, onsubmit: async (e) => {
        e.preventDefault();
        const key = keyWrap.input.value.trim();
        if (!key) return;
        setBusy(true);
        try {
          await CCAM.auth.login(key, remember.checked);
          ctx.onSuccess && ctx.onSuccess();
        } catch (ex) {
          setBusy(false);
          const msg = ex.status === 401 ? 'That key was not accepted.' : ex.isNetwork ? 'Can’t reach CC AutoMux. Is the service running?' : ('Sign-in failed: ' + (ex.detail || ex.message));
          replace(err, icon('alert'), h('span', null, msg)); err.hidden = false;
          keyWrap.input.focus(); keyWrap.input.select();
        }
      } },
        field({ label: 'Management key', for: 'mkey', control: keyWrap }),
        h('label', { class: 'chk' }, remember, h('span', null, 'Remember on this device ', h('span', { class: 'opt' }, '· stored in this browser only'))),
        err,
        submit
      );

      const card = h('div', { class: 'card login-card' },
        h('h1', null, 'CC AutoMux'),
        h('p', { class: 'login-sub' }, 'Local management console'),
        ctx.reason ? h('div', { class: 'note warn', style: { marginBottom: '16px' } }, ctx.reason) : null,
        form,
        h('div', { class: 'login-hint' }, 'The management key was set when CC AutoMux was installed. It is kept in ', h('code', null, 'config.json'), ' under ', h('code', null, 'auth.management_key'), ':',
          h('code', { class: 'path' }, configPathHint())),
      );
      ctx.root.appendChild(h('div', { class: 'login-wrap' }, h('div', { class: 'login-box' },
        mascot('login-mascot'), card,
        h('div', { class: 'login-foot' }, h('span', { class: 'mono' }, ctx.version || ''), h('span', { class: 'sep' }, '·'), icon('lock'), h('span', null, 'Listens on 127.0.0.1 only')))));
      setTimeout(() => keyWrap.input.focus(), 30);
    }
  };
})();
