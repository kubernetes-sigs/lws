// Run against the versioned Hugo preview: LWS_SITE_URL=... npm run test:animations
import assert from 'node:assert/strict';
import { chromium, firefox } from '@playwright/test';

const origin = process.env.LWS_SITE_URL || 'http://127.0.0.1:18446';
const prefix = process.env.LWS_DOCS_PREFIX ?? '/main';
const engine = process.env.PLAYWRIGHT_BROWSER === 'firefox' ? firefox : chromium;
const browser = await engine.launch({ headless: true,
  ...(process.env.PLAYWRIGHT_CHANNEL ? { channel: process.env.PLAYWRIGHT_CHANNEL } : {}) });
const page = await browser.newPage({ reducedMotion: 'reduce', viewport: { width: 1600, height: 1100 } });
const errors = [];
page.on('pageerror', error => errors.push(error.message));
const downloads = [];
page.on('request', request => {if (new URL(request.url()).pathname.match(/\/animations\/.*\.json$/)) downloads.push(request.url());});
const seekTo = (seek, time) => seek.evaluate((input, value) => {
  input.value = String(value);
  input.dispatchEvent(new Event('input', { bubbles: true }));
}, time);
const value = seek => seek.evaluate(input => Number(input.value));
let examples = 0;
try {
  for (const slug of ['rollout-strategy', 'llm-d-integration', 'scaling-during-rollout']) {
    downloads.length = 0;
    const response = await page.goto(`${origin}${prefix}/docs/concepts/disaggregatedset/${slug}/`);
    assert.equal(response.status(), 200);
    const figures = page.locator('[data-animation-source]');
    assert.ok(await figures.count() > 0, `${slug}: missing animation`);
    await page.waitForFunction(() => [...document.querySelectorAll('[data-animation-play]')].every(button => !button.disabled));
    for (const figure of await figures.all()) {
      const url = new URL(await figure.getAttribute('data-animation-source'), origin).href;
      assert.equal(downloads.filter(request => request === url).length, 1, 'Each drawing downloads once before scrolling; fetch reuses its preload');
    }
    for (const figure of await figures.all()) {
      examples++;
      await figure.scrollIntoViewIfNeeded();
      const play = figure.locator('[data-animation-play]');
      await play.waitFor();
      await page.waitForFunction(button => !button.disabled, await play.elementHandle());
      assert.equal(await play.textContent(), 'Play', 'Reduced motion starts paused');
      assert.equal(await figure.locator('[data-animation-loop]').isChecked(), true, 'Looping is enabled on a fresh page');
      const seek = figure.locator('[data-animation-seek]');
      const surface = figure.locator('[data-animation-svg]');
      assert.ok(await surface.isVisible());
      const asset = await (await page.request.get(new URL(await figure.getAttribute('data-animation-source'), origin).href)).json();
      const duration = asset.frames.reduce((total, frame) => total + frame.duration, 0);
      assert.ok(Math.abs(Number(await seek.getAttribute('max')) - duration) < .001);
      assert.equal(await surface.getAttribute('viewBox'), `0 0 ${asset.width} ${asset.height}`);
      let time = 0;
      for (const frame of asset.frames) {
        const sampleTime = time + frame.duration / 2;
        await seekTo(seek, sampleTime);
        assert.equal(await figure.locator('[data-animation-summary]').textContent(), frame.caption);
        assert.equal(await surface.locator(':scope > g').first().locator('use').count(), frame.items.length);
        if (asset.motions?.length) {
          const renderedTime = await value(seek); // Range inputs may round their assigned value.
          const actual = await surface.locator(':scope > g').last().locator('use').evaluateAll(nodes => nodes
            .filter(node => node.getAttribute('display') !== 'none').map(node => {
              const matrix = node.transform.baseVal.getItem(0).matrix; return [matrix.e, matrix.f];
            }));
          const expected = asset.motions.flatMap(({keyframes}) => {
            if (renderedTime < keyframes[0][0] || renderedTime > keyframes.at(-1)[0]) return [];
            const i = Math.min(keyframes.findLastIndex(key => key[0] <= renderedTime), keyframes.length - 2);
            const [a, b] = [keyframes[i], keyframes[i + 1]], t = (renderedTime - a[0]) / (b[0] - a[0]);
            return [[a[1] + (b[1] - a[1]) * t, a[2] + (b[2] - a[2]) * t]];
          });
          assert.equal(actual.length, expected.length, 'Only active motion tracks are visible');
          actual.forEach((point, i) => point.forEach((coordinate, axis) =>
            assert.ok(Math.abs(coordinate - expected[i][axis]) < .001, 'SVG motion follows the timeline within subpixel precision')));
        }
        time += frame.duration;
      }
      await seekTo(seek, 0);
      await figure.locator('[data-animation-next]').click();
      assert.ok(Math.abs(await value(seek) - asset.frames[0].duration) < .011);
      await figure.locator('[data-animation-previous]').click();
      assert.equal(await value(seek), 0);
      const background = () => surface.locator('defs > *').first().getAttribute('fill');
      await page.evaluate(() => { document.documentElement.dataset.bsTheme = 'light'; });
      const light = await background();
      await page.evaluate(() => { document.documentElement.dataset.bsTheme = 'dark'; });
      assert.notDeepEqual(await background(), light, 'Theme changes the drawing');
      if (process.env.LWS_SCREENSHOT_DIR) {
        const name = (await figure.getAttribute('data-animation-source')).split('/').at(-1).replace('.json', '');
        await surface.screenshot({ path: `${process.env.LWS_SCREENSHOT_DIR}/${name}-svg-dark.png` });
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
  // Firefox restores the earlier unchecked form state on reload.
  await loop.check();
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
    assert.ok(await player.evaluate(node => node.scrollHeight <= innerHeight + 1), 'Fullscreen fits drawing and controls');
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
  console.log(`Verified ${examples} SVG examples: all captions/states, themes, controls, autoplay, offscreen/manual pause, loop, fullscreen, reduced motion, and 320px layout.`);
} finally {
  await browser.close();
}
