/* CC AutoMux Web UI — host bridge. A desktop shell may inject window.ccAutomuxHost before the app boots. */
(function () {
  'use strict';
  const CCAM = (window.CCAM = window.CCAM || {});
  const injected = window.ccAutomuxHost || {};
  CCAM.host = {
    // Management key provided by a desktop host; null in the browser form.
    bootstrapKey() { return typeof injected.bootstrapKey === 'function' ? injected.bootstrapKey() : null; },
    // Host-only capabilities (e.g. { autostart: { get(): Promise<boolean>, set(bool): Promise<void> } }).
    capabilities: injected.capabilities || {},
    openExternal(url) { if (typeof injected.openExternal === 'function') injected.openExternal(url); else window.open(url, '_blank', 'noopener'); },
    isDesktop: !!injected.isDesktop
  };
})();
