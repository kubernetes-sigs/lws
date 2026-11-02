// Keep Docsy's native Light / Dark / Auto menu and the page theme in sync.
(() => {
  const root = document.documentElement;
  const key = 'td-color-theme';
  const media = matchMedia('(prefers-color-scheme: dark)');
  const valid = value => value === 'light' || value === 'dark' || value === 'auto';
  const storedPreference = () => {
    try {
      const value = localStorage.getItem(key);
      return valid(value) ? value : 'auto';
    } catch (_) { return 'auto'; }
  };
  // Retain an explicit choice for this page even when browser storage is blocked.
  let preference = storedPreference();
  const applyTheme = (focus = false) => {
    root.dataset.bsTheme = preference === 'auto' ? (media.matches ? 'dark' : 'light') : preference;
    root.removeAttribute('data-theme-init');
    for (const button of document.querySelectorAll('[data-bs-theme-value]')) {
      const active = button.dataset.bsThemeValue === preference;
      button.classList.toggle('active', active);
      button.setAttribute('aria-pressed', String(active));
      if (active) {
        const icon = button.querySelector('svg use')?.getAttribute('href');
        if (icon) document.querySelector('.theme-icon-active use')?.setAttribute('href', icon);
      }
    }
    const switcher = document.querySelector('#bd-theme');
    const label = preference === 'auto' ? 'Auto (system)' : preference === 'dark' ? 'Dark' : 'Light';
    switcher?.setAttribute('aria-label', `Color theme: ${label}`);
    if (focus) switcher?.focus();
  };
  applyTheme();

  const initialize = () => {
    applyTheme();
    for (const button of document.querySelectorAll('[data-bs-theme-value]')) {
      button.addEventListener('click', () => {
        const value = button.dataset.bsThemeValue;
        if (!valid(value)) return;
        preference = value;
        try { localStorage.setItem(key, value); } catch (_) {}
        applyTheme(true);
      });
    }
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', initialize, {once: true});
  else initialize();
  media.addEventListener('change', () => {
    if (preference === 'auto') applyTheme();
  });
  window.addEventListener('storage', event => {
    if (event.key === key || event.key === null) {
      preference = storedPreference();
      applyTheme();
    }
  });
})();
