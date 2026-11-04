// Run against a built, versioned site.
// PLAYWRIGHT_CHANNEL=chrome node site/scripts/layout.browser.mjs
import assert from 'node:assert/strict';
import { chromium, firefox } from '@playwright/test';
const origin = process.env.LWS_SITE_URL || 'http://127.0.0.1:18432';
const engine = process.env.PLAYWRIGHT_BROWSER === 'firefox' ? firefox : chromium;
const browser = await engine.launch({
  headless: true,
  ...(process.env.PLAYWRIGHT_CHANNEL ? { channel: process.env.PLAYWRIGHT_CHANNEL } : {}),
});
const page = await browser.newPage({ reducedMotion: 'reduce' });

async function visit(prefix = 'stable') {
  const response = await page.goto(new URL(`/${prefix}/docs/overview/`, origin).href);
  assert.ok([200, 304].includes(response?.status()), `${prefix}: overview must exist`);
  await page.evaluate(() => document.fonts.ready);
  await page.locator('.navbar-logo').waitFor({ state: 'visible' });
  // Require the live observer, not just the CSS fallback used before JS loads.
  await page.waitForFunction(() => ['--lws-header-height', '--lws-logo-bottom', '--lws-footer-height']
    .every(name => Number.parseFloat(document.documentElement.style.getPropertyValue(name)) > 0));
}

async function assertLogoNavigation() {
  await visit('main');
  const logo = page.locator('.navbar-logo');
  const initial = await logo.screenshot();
  const asset = '**/images/lws-ds-logos.svg';
  // The navigation logo must paint with the document, even when the standalone
  // image is unavailable. Routing also disables the browser's HTTP image cache.
  await page.route(asset, route => route.abort());
  try {
    for (const path of ['/main/docs/concepts/disaggregatedset/scaling-during-rollout/', '/stable/docs/overview/']) {
      const response = await page.goto(new URL(path, origin).href);
      assert.equal(response?.status(), 200);
      await page.evaluate(() => document.fonts.ready);
      assert.ok(initial.equals(await logo.screenshot()), `${path}: navigation logo depends on a separate image load`);
    }
  } finally {
    await page.unroute(asset);
  }
}

async function scrollTo(fraction) {
  await page.evaluate(fraction => window.scrollTo({
    top: (document.documentElement.scrollHeight - innerHeight) * fraction,
    behavior: 'instant',
  }), fraction);
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
}

async function geometry() {
  return page.evaluate(() => {
    const rect = node => {
      const r = node.getBoundingClientRect();
      return { x: r.x, y: r.y, right: r.right, bottom: r.bottom, width: r.width, height: r.height };
    };
    const visible = node => node.getBoundingClientRect().width > 0 && node.getBoundingClientRect().height > 0;
    const header = document.querySelector('.td-navbar');
    const obstacles = [header, header.querySelector('.navbar-logo')].map(rect);
    const topLink = selector => {
      const node = [...document.querySelectorAll(`${selector} a`)].find(visible);
      if (!node) return null;
      const r = rect(node);
      const clearance = Math.max(0, ...obstacles.filter(o => o.right > r.x && o.x < r.right).map(o => o.bottom));
      const x = Math.max(1, Math.min(innerWidth - 1, r.x + r.width / 2));
      const y = r.y + Math.min(r.height / 2, 10);
      const hit = document.elementFromPoint(x, y);
      return { ...r, clearance, text: node.textContent.trim(), unobscured: Boolean(hit && node.contains(hit)) };
    };
    return {
      width: document.documentElement.clientWidth,
      scrollWidth: document.documentElement.scrollWidth,
      header: rect(header), logo: rect(header.querySelector('.navbar-logo')),
      footer: rect(document.querySelector('footer')),
      left: topLink('#td-section-nav'), right: topLink('.td-sidebar-toc'),
      title: rect(document.querySelector('.td-content h1')),
    };
  });
}

function assertFooter(layout, label) {
  assert.ok(layout.scrollWidth <= layout.width + 1, `${label}: horizontal document overflow`);
  assert.ok(Math.abs(layout.footer.x) <= 1, `${label}: footer has a left gap (${layout.footer.x}px)`);
  assert.ok(Math.abs(layout.footer.right - layout.width) <= 1, `${label}: footer has a right gap`);
}

function assertSidebars(layout, label) {
  for (const side of ['left', 'right']) {
    const link = layout[side];
    if (side === 'right' && layout.width < 1200) {
      assert.equal(link, null, `${label}: right sidebar should stay hidden below desktop breakpoint`);
      continue;
    }
    assert.ok(link, `${label}: missing ${side} sidebar links`);
    assert.ok(link.y >= link.clearance - 1,
      `${label}: ${side} "${link.text}" starts at ${link.y.toFixed(1)}px, below-header boundary is ${link.clearance.toFixed(1)}px`);
    assert.ok(link.unobscured, `${label}: ${side} top link is covered or clipped`);
  }
}

async function assertDropdown(toggle, menu, label) {
  await page.locator(toggle).click();
  await page.locator(menu).waitFor({ state: 'visible' });
  const bounds = await page.locator(menu).boundingBox();
  const viewport = page.viewportSize();
  assert.ok(bounds.x >= -1 && bounds.x + bounds.width <= viewport.width + 1, `${label}: menu leaves viewport horizontally`);
  assert.ok(bounds.y >= 0 && bounds.y + bounds.height <= viewport.height + 1, `${label}: menu leaves viewport vertically`);
  await page.keyboard.press('Escape');
  await page.locator(menu).waitFor({ state: 'hidden' });
}

async function assertAnchor() {
  const hash = await page.locator('#TableOfContents a[href^="#"]').first().getAttribute('href');
  assert.ok(hash?.length > 1, 'Expected a real table-of-contents anchor');
  await page.evaluate(hash => { location.hash = hash; }, hash);
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  const heading = await page.evaluate(hash => {
    const node = document.getElementById(decodeURIComponent(hash.slice(1)));
    const range = document.createRange();
    range.selectNodeContents(node);
    // Docsy uses pseudo-element anchor offsets, so check actual heading text.
    const text = range.getBoundingClientRect();
    const header = document.querySelector('.td-navbar').getBoundingClientRect();
    return { top: text.top, bottom: text.bottom, clearance: Math.max(0, header.bottom), height: innerHeight };
  }, hash);
  assert.ok(heading.top >= heading.clearance - 1, 'Anchor heading is hidden under the header');
  assert.ok(heading.top < heading.height && heading.bottom > 0, 'Anchor heading is outside the viewport');
}

async function assertIndependentScroll(selector) {
  await page.locator(selector).evaluate(node => { node.scrollTop = 0; });
  const before = await page.locator(selector).evaluate(node => ({
    max: node.scrollHeight - node.clientHeight,
    overflow: getComputedStyle(node).overflowY,
    top: node.getBoundingClientRect().top,
    bottom: node.getBoundingClientRect().bottom,
    pageScroll: scrollY,
  }));
  assert.ok(before.max > 1, `${selector}: short-screen test must exercise a scrolling menu`);
  assert.match(before.overflow, /auto|scroll/, `${selector}: menu must scroll independently`);
  assert.ok(before.top >= 0 && before.bottom <= page.viewportSize().height + 1, `${selector}: scroll box leaves viewport`);
  await page.locator(selector).hover();
  await page.mouse.wheel(0, 120);
  await page.waitForFunction(selector => document.querySelector(selector).scrollTop > 0, selector);
  assert.equal(await page.evaluate(() => scrollY), before.pageScroll, `${selector}: scrolling menu moved the document`);
  const last = await page.locator(selector).evaluate(node => {
    node.scrollTop = node.scrollHeight;
    const link = [...node.querySelectorAll('a')].filter(link =>
      link.textContent.trim() && link.getBoundingClientRect().height > 0).at(-1);
    const item = link.getBoundingClientRect();
    const box = node.getBoundingClientRect();
    return { top: item.top, bottom: item.bottom, boxTop: box.top, boxBottom: box.bottom, text: link.textContent.trim() };
  });
  assert.ok(last.top >= last.boxTop - 1 && last.bottom <= last.boxBottom + 1,
    `${selector}: last link "${last.text}" is unreachable`);
  await page.locator(selector).evaluate(node => { node.scrollTop = 0; });
}

async function assertFooterResize() {
  await page.setViewportSize({ width: 1440, height: 480 });
  await visit();
  const initial = await page.locator('footer').evaluate(node => node.getBoundingClientRect().height);
  await page.locator('footer .container-fluid').evaluate(node => {
    const text = document.createElement('p');
    text.id = 'layout-footer-wrapping-fixture';
    text.textContent = 'Additional footer text verifies wrapping and navigation clearance. '.repeat(12);
    node.append(text);
  });
  await page.waitForFunction(initial => {
    const height = document.querySelector('footer').getBoundingClientRect().height;
    return height > initial && Number.parseFloat(document.documentElement.style.getPropertyValue('--lws-footer-height')) === Math.ceil(height);
  }, initial);
  await scrollTo(1);
  const layout = await geometry();
  assertFooter(layout, 'Wrapped footer');
  assertSidebars(layout, 'Wrapped footer');
  await page.locator('#layout-footer-wrapping-fixture').evaluate(node => node.remove());
  await page.waitForFunction(initial => Number.parseFloat(document.documentElement.style.getPropertyValue('--lws-footer-height')) === Math.ceil(initial), initial);
}

try {
  await assertLogoNavigation();
  let positions = 0;
  for (const prefix of ['main', 'stable', 'v0.11']) {
    for (const viewport of [{ width: 2560, height: 1440 }, { width: 1440, height: 1000 },
      { width: 1024, height: 768 }, { width: 768, height: 1000 }]) {
      await page.setViewportSize(viewport);
      await visit(prefix);
      for (const fraction of [0, 0.5, 1]) {
        await scrollTo(fraction);
        const layout = await geometry();
        const label = `${prefix} ${viewport.width}×${viewport.height}, scroll ${fraction}`;
        assertFooter(layout, label);
        assertSidebars(layout, label);
        if (fraction === 0) assert.ok(layout.title.y >= layout.header.bottom, `${label}: title under header`);
        positions++;
      }
      await assertDropdown('.docs-version-toggle', '.docs-version-menu', 'Versions');
      assert.ok(await page.locator('.docs-version-menu a[href*="/main/"]').count(), 'Missing main version link');
      assert.ok(await page.locator('.docs-version-menu a[href*="/stable/"]').count(), 'Missing stable version link');
      await assertDropdown('#bd-theme', '.lws-theme-control .dropdown-menu', 'Theme');
      await assertAnchor();
    }
  }

  await page.setViewportSize({ width: 1440, height: 360 });
  await visit();
  for (const fraction of [0, 0.5, 1]) {
    await scrollTo(fraction);
    assertSidebars(await geometry(), `Short desktop, scroll ${fraction}`);
    positions++;
  }
  await scrollTo(0.5);
  await assertIndependentScroll('#td-section-nav');
  await assertIndependentScroll('.td-sidebar-toc');
  await assertFooterResize();
  await page.setViewportSize({ width: 1440, height: 1000 });
  await visit();
  for (const theme of ['light', 'dark']) {
    await page.evaluate(theme => { document.documentElement.dataset.bsTheme = theme; }, theme);
    await assertDropdown('.docs-version-toggle', '.docs-version-menu', `${theme}: versions`);
    await assertDropdown('#bd-theme', '.lws-theme-control .dropdown-menu', `${theme}: theme`);
  }

  for (const width of [391, 320]) {
    await page.setViewportSize({ width, height: 844 });
    await visit();
    const toggle = page.locator('.docs-section-toggle');
    assert.equal(await toggle.getAttribute('aria-expanded'), 'false');
    await toggle.click();
    await page.locator('#td-section-nav').waitFor({ state: 'visible' });
    assert.equal(await toggle.getAttribute('aria-expanded'), 'true');
    const opened = await geometry();
    assertFooter(opened, `Mobile ${width}: menu open`);
    assert.ok(opened.left.y >= opened.header.bottom - 1, 'Mobile menu is under the header');
    assert.ok(opened.left.unobscured, 'Mobile menu top link is obscured');
    assert.equal(opened.right, null, 'Desktop table of contents should remain hidden on mobile');
    await toggle.click();
    await page.locator('#td-section-nav').waitFor({ state: 'hidden' });
    assert.equal(await toggle.getAttribute('aria-expanded'), 'false');
    await assertDropdown('.docs-version-toggle', '.docs-version-menu', 'Mobile versions');
    await assertDropdown('#bd-theme', '.lws-theme-control .dropdown-menu', 'Mobile theme');
    await scrollTo(1);
    assertFooter(await geometry(), `Mobile ${width}: page end`);
  }

  console.log(`Layout passed: ${positions} desktop scroll positions across main/stable/v0.11, `
    + 'logo across document navigation without an image load, live layout observer and footer wrapping, full-width footer, independent short-screen sidebars, '
    + 'anchors, tablet layout, 391/320px mobile navigation and dropdowns in both themes.');
} finally {
  await browser.close();
}
