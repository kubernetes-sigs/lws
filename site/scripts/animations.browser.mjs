// Run against the versioned Hugo preview: LWS_SITE_URL=... npm run test:animations
import assert from 'node:assert/strict';
import { chromium } from '@playwright/test';

const origin = process.env.LWS_SITE_URL || 'http://127.0.0.1:18446';
const prefix = process.env.LWS_DOCS_PREFIX ?? '/main';
const browser = await chromium.launch({ headless: true,
  ...(process.env.PLAYWRIGHT_CHANNEL ? { channel: process.env.PLAYWRIGHT_CHANNEL } : {}) });
const page = await browser.newPage({ reducedMotion: 'reduce', viewport: { width: 1600, height: 1100 } });
const errors = [];
page.on('pageerror', error => errors.push(error.message));
const seekTo = (seek, time) => seek.evaluate((input, value) => {
  input.value = String(value);
  input.dispatchEvent(new Event('input', { bubbles: true }));
}, time);
const value = seek => seek.evaluate(input => Number(input.value));
let examples = 0;
try {
  for (const slug of ['rollout-strategy', 'llm-d-integration', 'scaling-during-rollout']) {
    const response = await page.goto(`${origin}${prefix}/docs/concepts/disaggregatedset/${slug}/`);
    assert.equal(response.status(), 200);
    const figures = page.locator('[data-animation-source]');
    assert.ok(await figures.count() > 0, `${slug}: missing Canvas`);
    for (const figure of await figures.all()) {
      examples++;
      await figure.scrollIntoViewIfNeeded();
      const play = figure.locator('[data-animation-play]');
      await play.waitFor();
      await page.waitForFunction(button => !button.disabled, await play.elementHandle());
      assert.equal(await play.textContent(), 'Play', 'Reduced motion starts paused');
      const canvas = figure.locator('canvas'), seek = figure.locator('[data-animation-seek]');
      const asset = await (await page.request.get(new URL(await figure.getAttribute('data-animation-source'), origin).href)).json();
      const duration = asset.frames.reduce((total, frame) => total + frame.duration, 0);
      assert.ok(Math.abs(Number(await seek.getAttribute('max')) - duration) < .001);
      assert.equal(await canvas.getAttribute('width'), String(asset.width));
      let time = 0;
      for (const frame of asset.frames) {
        await seekTo(seek, time + frame.duration / 2);
        assert.equal(await figure.locator('[data-animation-summary]').textContent(), frame.caption);
        time += frame.duration;
      }
      await seekTo(seek, 0);
      await figure.locator('[data-animation-next]').click();
      assert.ok(Math.abs(await value(seek) - asset.frames[0].duration) < .011);
      await figure.locator('[data-animation-previous]').click();
      assert.equal(await value(seek), 0);
      const pixel = () => canvas.evaluate(node => [...node.getContext('2d').getImageData(0, 0, 1, 1).data]);
      await page.evaluate(() => { document.documentElement.dataset.bsTheme = 'light'; });
      const light = await pixel();
      await page.evaluate(() => { document.documentElement.dataset.bsTheme = 'dark'; });
      assert.notDeepEqual(await pixel(), light, 'Theme changes the drawing');
      if (process.env.LWS_SCREENSHOT_DIR) {
        const name = (await figure.getAttribute('data-animation-source')).split('/').at(-1).replace('.json', '');
        await canvas.screenshot({ path: `${process.env.LWS_SCREENSHOT_DIR}/${name}-dark.png` });
      }
      await figure.locator('[data-animation-speed]').selectOption('4');
      await play.click();
      await page.waitForTimeout(300);
      assert.ok(await value(seek) > .5, 'Playback and speed advance presentation time');
      await play.click();
      const paused = await value(seek);
      await page.waitForTimeout(100);
      assert.equal(await value(seek), paused, 'Pause stops playback');
      await figure.locator('[data-animation-loop]').uncheck();
      await seekTo(seek, duration - .05);
      await play.click();
      await page.waitForFunction(button => button.textContent === 'Play', await play.elementHandle());
      assert.ok(Math.abs(await value(seek) - duration) < .011, 'Nonlooping playback ends at final state');
      assert.equal(await figure.locator('[data-animation-error]').isVisible(), false);
    }
  }
  // Exercise automatic playback on the same real documentation, without relying
  // on any particular animation's content or the surrounding page's length.
  await page.emulateMedia({ reducedMotion: 'no-preference' });
  await page.reload();
  const automatic = page.locator('[data-animation-source]').first();
  await automatic.scrollIntoViewIfNeeded();
  const play = automatic.locator('[data-animation-play]');
  const seek = automatic.locator('[data-animation-seek]');
  const hasLabel = async text => page.waitForFunction(([button, label]) => button.textContent === label,
    [await play.elementHandle(), text]);
  await hasLabel('Pause');
  const started = await value(seek);
  await page.waitForTimeout(150);
  assert.ok(await value(seek) > started, 'Visible non-reduced-motion playback starts automatically');

  await automatic.evaluate(figure => {
    const spacer = document.createElement('div');
    spacer.dataset.animationTestSpacer = '';
    spacer.style.height = '200vh';
    figure.before(spacer);
    window.scrollTo(0, 0);
  });
  await hasLabel('Play');
  const offscreen = await value(seek);
  await page.waitForTimeout(150);
  assert.equal(await value(seek), offscreen, 'Offscreen animation stops its clock');
  await automatic.scrollIntoViewIfNeeded();
  await hasLabel('Pause');
  await play.click();
  await hasLabel('Play');
  await page.evaluate(() => window.scrollTo(0, 0));
  await automatic.scrollIntoViewIfNeeded();
  await page.waitForTimeout(150);
  assert.equal(await play.textContent(), 'Play', 'Scrolling never overrides a manual pause');
  await page.locator('[data-animation-test-spacer]').evaluate(node => node.remove());
  await automatic.scrollIntoViewIfNeeded();

  const loop = automatic.locator('[data-animation-loop]');
  assert.equal(await loop.isChecked(), true, 'Automatic playback loops by default');
  const end = Number(await seek.getAttribute('max'));
  await seekTo(seek, end - .05);
  await play.click();
  await page.waitForFunction(([input, end]) => Number(input.value) < end / 2,
    [await seek.elementHandle(), end]);
  assert.equal(await play.textContent(), 'Pause', 'Looping continues across the final state');
  await play.click();

  const fullscreen = automatic.locator('[data-animation-fullscreen]');
  if (await fullscreen.isVisible()) {
    const player = automatic.locator('[data-animation-player]');
    await fullscreen.click();
    await page.waitForFunction(node => document.fullscreenElement === node, await player.elementHandle());
    await page.waitForFunction(button => button.getAttribute('aria-label') === 'Exit fullscreen', await fullscreen.elementHandle());
    assert.equal(await fullscreen.getAttribute('aria-label'), 'Exit fullscreen');
    assert.ok(await player.evaluate(node => node.scrollHeight <= innerHeight + 1), 'Fullscreen fits Canvas and controls');
    await fullscreen.click();
    await page.waitForFunction(() => !document.fullscreenElement);
    await page.waitForFunction(button => button.getAttribute('aria-label') === 'Enter fullscreen', await fullscreen.elementHandle());
    assert.equal(await fullscreen.getAttribute('aria-label'), 'Enter fullscreen');
  }

  await page.setViewportSize({ width: 320, height: 844 });
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'Mobile page fits viewport');
  const first = page.locator('[data-animation-source]').first();
  await first.scrollIntoViewIfNeeded();
  assert.ok(await first.locator('[data-animation-play]').isVisible());
  assert.deepEqual(errors, []);
  console.log(`Verified ${examples} inline examples: all captions/states, themes, controls, autoplay, offscreen/manual pause, loop, fullscreen, reduced motion, and 320px layout.`);
} finally {
  await browser.close();
}
