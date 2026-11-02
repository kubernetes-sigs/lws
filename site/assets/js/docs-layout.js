// Keep sticky navigation below the header and above the footer, including when
// text wraps or the combined logo extends below the navbar. The trace player
// reserves footer space only while it is visible, keeping the scenario list tall.
(() => {
  const navbar = document.querySelector('.td-navbar--docs');
  const logo = navbar?.querySelector('.navbar-logo');
  const footer = document.querySelector('.td-outer > footer');
  const subheader = document.querySelector('.trace-player-subheader');
  if (!navbar || !footer || !('ResizeObserver' in window)) return;

  const root = document.documentElement;
  const measure = () => {
    const header = navbar.getBoundingClientRect();
    const footerBounds = footer.getBoundingClientRect();
    const logoBottom = logo ? logo.getBoundingClientRect().bottom - header.top : header.height;
    const sizes = {
      '--lws-header-height': header.height,
      '--lws-logo-bottom': Math.max(header.height, logoBottom),
      '--lws-footer-height': footerBounds.height,
    };
    if (subheader) {
      sizes['--lws-subheader-height'] = subheader.getBoundingClientRect().height;
      sizes['--lws-trace-footer-overlap'] = Math.max(0, innerHeight - footerBounds.top);
    }
    for (const [name, size] of Object.entries(sizes)) {
      const value = `${Math.ceil(size)}px`;
      if (root.style.getPropertyValue(name) !== value) root.style.setProperty(name, value);
    }
  };
  let queued = false;
  const schedule = () => {
    if (queued) return;
    queued = true;
    requestAnimationFrame(() => {queued = false; measure();});
  };
  const observer = new ResizeObserver(measure);
  [navbar, logo, footer, subheader].filter(Boolean).forEach(element => observer.observe(element));
  if (subheader) {
    window.addEventListener('scroll', schedule, {passive: true});
    window.addEventListener('resize', schedule);
    document.addEventListener('animation-ready', schedule);
  }
  measure();
})();
