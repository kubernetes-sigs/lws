// Run against a Hugo preview: LWS_SITE_URL=... node scripts/animation-seek.browser.mjs
import assert from 'node:assert/strict';
import { chromium, firefox } from '@playwright/test';

const origin = process.env.LWS_SITE_URL || 'http://127.0.0.1:18446';
const prefix = process.env.LWS_DOCS_PREFIX ?? '/main';
const drawing = {
  version: 1, width: 1920, height: 1080, title: 'Fractional seeking regression',
  primitives: [{type: 'rect', x: 0, y: 0, width: 1920, height: 1080, fill: '#ffffff'}],
  frames: [
    {duration: 1.003, caption: 'Initial state', items: [0]},
    {duration: .001, caption: 'One millisecond state', items: [0]},
    {duration: .0025, caption: 'Two and a half millisecond state', items: [0]},
    {duration: 19.09075, caption: 'Final state', items: [0]},
  ],
};
const duration = drawing.frames.reduce((total, frame) => total + frame.duration, 0);
const engine = process.env.PLAYWRIGHT_BROWSER === 'firefox' ? firefox : chromium;
const browser = await engine.launch({headless: true,
  ...(process.env.PLAYWRIGHT_CHANNEL ? {channel: process.env.PLAYWRIGHT_CHANNEL} : {})});
const page = await browser.newPage({reducedMotion: 'reduce', viewport: {width: 1600, height: 1100}});
const errors = [];
page.on('pageerror', error => errors.push(error.message));
try {
  await page.route('**/animations/fractional.json', route => route.fulfill({json: drawing}));
  const response = await page.goto(`${origin}${prefix}/docs/concepts/disaggregatedset/rollout-strategy/`);
  assert.equal(response.status(), 200);
  const figure = page.locator('[data-animation-source]').first();
  await figure.scrollIntoViewIfNeeded();
  const seek = figure.locator('[data-animation-seek]');
  await page.waitForFunction(input => !input.disabled, await seek.elementHandle());
  const summary = figure.locator('[data-animation-summary]');
  const assertTime = async expected => assert.ok(Math.abs(Number(await seek.inputValue()) - expected) < 1e-12,
    `Expected an exact presentation time of ${expected}, got ${await seek.inputValue()}`);
  const seekTo = time => seek.evaluate((input, value) => {
    input.value = String(value);
    input.dispatchEvent(new Event('input', {bubbles: true}));
  }, time);
  assert.equal(await seek.getAttribute('step'), 'any');
  assert.equal(Number(await seek.getAttribute('max')), duration);

  await seek.focus();
  await seek.press('End');
  await assertTime(duration);
  assert.equal(await summary.textContent(), 'Final state');
  await seek.press('Home');
  await assertTime(0);
  assert.equal(await summary.textContent(), 'Initial state');
  // Dragging to the far edge must reach a duration that is not a 10ms multiple.
  const bounds = await seek.boundingBox();
  await page.mouse.move(bounds.x + 1, bounds.y + bounds.height / 2);
  await page.mouse.down();
  await page.mouse.move(bounds.x + bounds.width - 1, bounds.y + bounds.height / 2);
  await page.mouse.up();
  await assertTime(duration);

  for (const [time, caption] of [[1.0035, 'One millisecond state'], [1.005, 'Two and a half millisecond state']]) {
    await seekTo(time);
    await assertTime(time);
    assert.equal(await summary.textContent(), caption, 'Sub-10ms states remain individually seekable');
  }
  await seekTo(0);
  await figure.locator('[data-animation-next]').click();
  await assertTime(1.003);
  assert.equal(await summary.textContent(), 'One millisecond state');
  await figure.locator('[data-animation-next]').click();
  await assertTime(1.004);
  assert.equal(await summary.textContent(), 'Two and a half millisecond state');

  // step="any" has browser-dependent native arrow increments, so keyboard
  // scrubbing promises explicit one-second steps while retaining the fraction.
  await seekTo(2.0035);
  await seek.focus();
  for (const [key, time] of [['ArrowRight', 3.0035], ['ArrowLeft', 2.0035], ['ArrowUp', 3.0035], ['ArrowDown', 2.0035]]) {
    await seek.press(key);
    await assertTime(time);
  }
  await seekTo(duration - .3);
  await seek.press('ArrowRight');
  await assertTime(duration);
  await seekTo(.3);
  await seek.press('ArrowLeft');
  await assertTime(0);
  assert.deepEqual(errors, []);
  console.log('Verified exact fractional endpoints, pointer dragging, sub-10ms states, and one-second keyboard scrubbing.');
} finally {
  await browser.close();
}
