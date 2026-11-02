// Run against a built, versioned site.
// PLAYWRIGHT_CHANNEL=chrome node site/scripts/code-theme.browser.mjs
import assert from 'node:assert/strict';
import { chromium } from '@playwright/test';
const origin = process.env.LWS_SITE_URL || 'http://127.0.0.1:18432';
const browser = await chromium.launch({
  headless: true,
  ...(process.env.PLAYWRIGHT_CHANNEL ? { channel: process.env.PLAYWRIGHT_CHANNEL } : {}),
});

async function visit(page, path) {
  const response = await page.goto(new URL(path, origin).href, { waitUntil: 'load' });
  assert.equal(response?.status(), 200, `${path}: page must exist`);
  assert.ok(await page.locator('.td-content pre').count(), `${path}: must contain code`);
}

// Measure the rendered cascade, including alpha backgrounds and inherited opacity,
// rather than matching SCSS strings. This catches Hugo inline styles and Prism's
// later-loaded stylesheet, both of which caused the original regression.
async function inspect(page, selector = '.td-content pre') {
  return page.evaluate(selector => {
    const rgba = value => {
      const match = value.match(/^rgba?\(([^)]+)\)$/);
      if (!match) throw new Error(`Unsupported computed color: ${value}`);
      const values = match[1].split(/[,\s/]+/).map(Number);
      return [values[0], values[1], values[2], values[3] ?? 1];
    };
    const over = (front, back) => front.slice(0, 3)
      .map((value, index) => value * front[3] + back[index] * (1 - front[3]));
    const luminance = color => color.slice(0, 3).map(value => {
      const s = value / 255;
      return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
    }).reduce((sum, value, index) => sum + value * [0.2126, 0.7152, 0.0722][index], 0);
    const ratio = (a, b) => (Math.max(luminance(a), luminance(b)) + 0.05)
      / (Math.min(luminance(a), luminance(b)) + 0.05);
    const sample = element => {
      const chain = [];
      for (let node = element; node; node = node.parentElement) chain.unshift(node);
      let background = [255, 255, 255];
      let opacity = 1;
      for (const node of chain) {
        const style = getComputedStyle(node);
        background = over(rgba(style.backgroundColor), background);
        opacity *= Number(style.opacity);
      }
      const style = getComputedStyle(element);
      const ink = rgba(style.color);
      ink[3] *= opacity;
      const selection = getComputedStyle(element, '::selection');
      return {
        name: `${element.tagName.toLowerCase()}.${element.className}`,
        text: element.textContent.trim().slice(0, 65),
        color: style.color,
        background: style.backgroundColor,
        backgroundAlpha: rgba(style.backgroundColor)[3],
        backgroundLuminance: luminance(background),
        contrast: ratio(over(ink, background), background),
        shadow: style.textShadow,
        selectionContrast: ratio(rgba(selection.color), rgba(selection.backgroundColor)),
      };
    };
    return [...document.querySelectorAll(selector)].map(pre => ({
      base: sample(pre),
      runs: [...pre.querySelectorAll('code, .token, span[style]')].map(sample),
    }));
  }, selector);
}

function assertDark(blocks, label) {
  assert.ok(blocks.length, `${label}: no code blocks checked`);
  for (const { base, runs } of blocks) {
    assert.ok(base.backgroundLuminance < 0.1, `${label}: ${base.name} has a light background`);
    for (const item of [base, ...runs]) {
      assert.ok(item.contrast >= 4.5,
        `${label}: ${item.name} "${item.text}" contrast ${item.contrast.toFixed(2)} < 4.5`);
      assert.equal(item.shadow, 'none', `${label}: ${item.name} has a text shadow`);
      assert.ok(item.selectionContrast >= 4.5, `${label}: unreadable selected code`);
      if (item.name.includes('token')) {
        assert.equal(item.backgroundAlpha, 0, `${label}: ${item.name} has a token background`);
      }
    }
  }
  return Math.min(...blocks.flatMap(({ base, runs }) => [base, ...runs]).map(item => item.contrast));
}

async function selectTheme(page, preference, expected = preference) {
  await page.locator('#bd-theme').click();
  const option = page.locator(`[data-bs-theme-value="${preference}"]`);
  await option.click();
  await page.waitForFunction(theme => document.documentElement.dataset.bsTheme === theme, expected);
  assert.equal(await option.getAttribute('aria-pressed'), 'true');
}

async function addFixture(page) {
  return page.evaluate(() => {
    const fixture = document.createElement('section');
    fixture.id = 'code-theme-fixture';
    const block = (language, text) => {
      const pre = document.createElement('pre');
      // Deliberately reproduce the inline Tango background from Hugo.
      pre.style.backgroundColor = '#f8f8f8';
      if (language) pre.className = `language-${language}`;
      const code = document.createElement('code');
      if (language) code.className = `language-${language}`;
      code.textContent = text;
      pre.append(code);
      fixture.append(pre);
      return code;
    };
    block('yaml', 'spec:\n  replicas: 4\n  enabled: true\n  strategy: Immediate\n  label: "ready"\n# comment');
    block('shell', 'VERSION=1.0\necho "$VERSION" | grep --color=never ready\n# comment');
    // JSON is not bundled in Docsy's Prism grammar. Verify its plain-text fallback.
    block('json', '{"replicas": 4, "ready": true, "name": "example"}');
    block('', 'Untyped output: Ready 4/4; Starting 0');
    const plainPre = document.createElement('pre');
    plainPre.textContent = 'Plain pre without a code wrapper';
    fixture.append(plainPre);
    document.querySelector('.td-content').append(fixture);
    fixture.querySelectorAll('code[class]').forEach(code => window.Prism.highlightElement(code));

    // Cover every standard Prism token and the compound selectors used by its
    // CSS/markup grammars, independent of which docs happen to contain them.
    const tokenNames = ['comment', 'prolog', 'doctype', 'cdata', 'punctuation', 'namespace',
      'property', 'tag', 'boolean', 'number', 'constant', 'symbol', 'deleted', 'selector',
      'attr-name', 'string', 'char', 'builtin', 'inserted', 'operator', 'entity', 'url',
      'atrule', 'attr-value', 'keyword', 'function', 'class-name', 'regex', 'important',
      'variable', 'key atrule', 'boolean important', 'parameter variable', 'assign-left variable',
      'unknown-future-token'];
    const tokens = block('css', '');
    tokens.parentElement.classList.add('style');
    for (const name of tokenNames) {
      const span = document.createElement('span');
      span.className = `token ${name}`;
      span.textContent = `${name}\n`;
      tokens.append(span);
    }
    const inline = document.createElement('code');
    inline.id = 'code-theme-inline';
    inline.textContent = 'ordinary inline code';
    fixture.append(inline);
    return { tokens: tokenNames.length, jsonGrammar: Boolean(window.Prism.languages.json) };
  });
}

try {
  const context = await browser.newContext({ colorScheme: 'dark' });
  await context.addInitScript(() => localStorage.setItem('td-color-theme', 'dark'));
  const page = await context.newPage();
  let checked = 0;
  let minimum = Infinity;
  for (const prefix of ['main', 'stable', 'v0.11']) {
    const paths = ['installation/', 'troubleshooting/'];
    if (prefix !== 'v0.11') paths.push('concepts/disaggregatedset/scaling-during-rollout/');
    for (const path of paths) {
      const url = `/${prefix}/docs/${path}`;
      await visit(page, url);
      await page.waitForFunction(() => window.Prism && document.documentElement.dataset.bsTheme === 'dark');
      const blocks = await inspect(page);
      minimum = Math.min(minimum, assertDark(blocks, url));
      checked += blocks.length;
    }
  }

  await visit(page, '/main/docs/concepts/disaggregatedset/scaling-during-rollout/');
  // Assert the exact value that was invisible in the reported screenshot.
  assert.match(await page.locator('.td-content pre').first().textContent(), /Immediate/);
  const fixture = await addFixture(page);
  minimum = Math.min(minimum, assertDark(await inspect(page, '#code-theme-fixture pre'), 'Prism fixture'));
  assert.ok(fixture.tokens > 30);
  assert.equal(await page.locator('#code-theme-inline').evaluate(node => getComputedStyle(node).display), 'inline');

  // Light must restore the upstream palette, not retain our dark overrides.
  // Its inherited Prism token palette is outside this dark-mode regression.
  await selectTheme(page, 'light');
  const light = (await inspect(page, '.td-content > .highlight pre, .td-content .highlight pre'))[0]?.base
    ?? (await inspect(page))[0].base;
  assert.ok(light.backgroundLuminance > 0.8, 'Light: expected light code background');
  assert.ok(light.contrast >= 4.5, 'Light: expected readable base code');
  await selectTheme(page, 'dark');
  assertDark(await inspect(page), 'Dark after Light');
  await selectTheme(page, 'auto', 'dark');
  await page.emulateMedia({ colorScheme: 'light' });
  await page.waitForFunction(() => document.documentElement.dataset.bsTheme === 'light');
  assert.deepEqual((await inspect(page))[0].base, light, 'Auto/light must restore Light styles');
  await page.emulateMedia({ colorScheme: 'dark' });
  await page.waitForFunction(() => document.documentElement.dataset.bsTheme === 'dark');
  assertDark(await inspect(page), 'Auto/dark');
  await selectTheme(page, 'dark');
  await page.emulateMedia({ colorScheme: 'light' });
  assert.equal(await page.locator('html').getAttribute('data-bs-theme'), 'dark', 'Explicit Dark must ignore OS changes');
  await context.close();

  // With page scripts disabled, Hugo's original inline-colored spans survive.
  // Setting the root attribute through DevTools isolates their dark CSS fallback.
  const noJS = await browser.newContext({ javaScriptEnabled: false });
  const fallback = await noJS.newPage();
  await visit(fallback, '/main/docs/installation/');
  await fallback.evaluate(() => { document.documentElement.dataset.bsTheme = 'dark'; });
  assert.equal(await fallback.evaluate(() => typeof window.Prism), 'undefined');
  assert.ok(await fallback.locator('.td-content pre span[style*="color:"]').count(), 'No-JS: expected Hugo inline colors');
  assertDark(await inspect(fallback), 'No-JS Tango fallback');
  await noJS.close();

  console.log(`Code themes passed: ${checked} real blocks across main/stable/v0.11, `
    + `${fixture.tokens} Prism token types, YAML/shell/JSON/untyped fixtures, Light/Dark/Auto and no-JS fallback. `
    + `Lowest dark contrast: ${minimum.toFixed(2)}:1. JSON grammar bundled: ${fixture.jsonGrammar}.`);
} finally {
  await browser.close();
}
