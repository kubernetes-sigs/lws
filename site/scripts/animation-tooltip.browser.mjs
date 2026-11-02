// Run against a Hugo preview: LWS_SITE_URL=... node scripts/animation-tooltip.browser.mjs
import assert from 'node:assert/strict';
import { chromium, firefox } from '@playwright/test';

const origin = process.env.LWS_SITE_URL || 'http://127.0.0.1:18446';
const prefix = process.env.LWS_DOCS_PREFIX ?? '/main';
const renderer = process.env.LWS_RENDERER || 'svg';
const drawing = {
  version: 1, width: 1920, height: 1080, title: 'Tooltip sizing regression',
  primitives: [{type: 'rect', x: 0, y: 0, width: 1920, height: 1080, fill: '#ffffff'}],
  hits: [
    {x: 1850, y: 200, width: 70, height: 100, label: 'A', detail: 'B'},
    {x: 1380, y: 200, width: 120, height: 100, label: 'Recorded capacity',
      detail: 'Requested groups can still be loading. Ready groups and the availability minimum come from an independent planner observation.'},
  ],
  frames: [{duration: 2, caption: 'Inspect a short label, then a longer explanation.', items: [0], hits: [0, 1]}],
};
const engine = process.env.PLAYWRIGHT_BROWSER === 'firefox' ? firefox : chromium;
const browser = await engine.launch({headless: true,
  ...(process.env.PLAYWRIGHT_CHANNEL ? {channel: process.env.PLAYWRIGHT_CHANNEL} : {})});
const page = await browser.newPage({reducedMotion: 'reduce', viewport: {width: 1600, height: 1100}});
const errors = [];
page.on('pageerror', error => errors.push(error.message));
try {
  await page.route('**/animations/fractional.json', route => route.fulfill({json: drawing}));
  const response = await page.goto(`${origin}${prefix}/docs/concepts/disaggregatedset/rollout-strategy/?renderer=${renderer}`);
  assert.equal(response.status(), 200);
  const figure = page.locator('[data-animation-source]').first();
  await figure.scrollIntoViewIfNeeded();
  const play = figure.locator('[data-animation-play]');
  await page.waitForFunction(button => !button.disabled, await play.elementHandle());
  const canvas = figure.locator(renderer === 'svg' ? '[data-animation-svg]' : 'canvas'), tooltip = figure.locator('[data-animation-tooltip]');
  for (const width of [1600, 1100]) {
    await page.setViewportSize({width, height: 1100});
    await figure.scrollIntoViewIfNeeded();
    const bounds = await canvas.boundingBox();
    const hover = async x => {
      await page.mouse.move(bounds.x + x / drawing.width * bounds.width,
        bounds.y + 250 / drawing.height * bounds.height);
      await tooltip.waitFor({state: 'visible'});
      return tooltip.boundingBox();
    };
    // Repeat without resetting style.left: every long tooltip must be measured
    // independently of the preceding narrow tooltip at the right edge.
    for (let repeat = 0; repeat < 3; repeat++) {
      const short = await hover(1890);
      assert.ok(short.x > bounds.x + bounds.width * .8, 'Short tooltip starts near the right edge');
      const long = await hover(1440);
      assert.equal(await tooltip.textContent(), `${drawing.hits[1].label}\n${drawing.hits[1].detail}`);
      assert.ok(Math.abs(long.width - Math.min(420, bounds.width - 16)) < 1,
        `Long tooltip retains its intrinsic capped width after left changes (${long.width}px)`);
      assert.ok(long.x >= bounds.x + 7, 'Tooltip stays inside the left margin');
      assert.ok(long.x + long.width <= bounds.x + bounds.width - 7, 'Tooltip stays inside the right margin');
    }
  }
  assert.deepEqual(errors, []);
  console.log('Verified tooltip sizing after repeated right-edge hovers at two responsive widths.');
} finally {
  await browser.close();
}
