// LWS_SITE_URL=http://127.0.0.1:18446 PLAYWRIGHT_CHANNEL=chrome node site/scripts/trace-player.browser.mjs
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import {chromium, firefox} from '@playwright/test';

const origin = process.env.LWS_SITE_URL || 'http://127.0.0.1:18446';
const route = '/disaggregated-set-trace-visualizer/';
const themes = ['lws','lws-dark','mistral-light','mistral-dark','llm-d-light','llm-d-dark'];
const engine = process.env.PLAYWRIGHT_BROWSER === 'firefox' ? firefox : chromium;
const browser = await engine.launch({headless: true,
  ...(process.env.PLAYWRIGHT_CHANNEL ? {channel: process.env.PLAYWRIGHT_CHANNEL} : {})});
const page = await browser.newPage({reducedMotion: 'reduce', viewport: {width: 1600, height: 1200}});
const requests = [], errors = [];
page.on('request', request => requests.push(request.url()));
page.on('pageerror', error => errors.push(error.message));
const status = page.locator('[data-trace-status]'), surface = page.locator('[data-animation-svg]');
const ready = () => page.waitForFunction(() => !document.querySelector('[data-trace-export]').disabled);
const settle = () => page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
async function visit(suffix = '') {
  const response = await page.goto(origin + route + suffix);
  assert.ok([200,304].includes((response ?? await page.reload()).status()));
  await ready();await page.evaluate(() => document.fonts.ready);await settle();
}
async function select(group, entry) {
  await page.locator(`[data-trace-suite="${group.id}"]`).click();
  await page.locator(`[data-trace-scenario="${entry.id}"]`).click();
  await page.waitForFunction(title => document.querySelector('[data-trace-status]').textContent === title, entry.title);
  assert.equal(await page.locator(`[data-trace-scenario="${entry.id}"]`).getAttribute('aria-current'), 'true');
  assert.equal(await page.locator('[data-animation-error]').isVisible(), false);
}
async function imageHash() {
  return createHash('sha256').update(await surface.screenshot()).digest('hex');
}
const state = () => page.evaluate(() => ({
  time: document.querySelector('[data-animation-seek]').value,
  speed: document.querySelector('[data-animation-speed]').value,
  play: document.querySelector('[data-animation-play]').textContent,
  caption: document.querySelector('[data-animation-summary]').textContent,
}));
async function assertResponsiveSizing() {
  const viewport = page.viewportSize();
  for (const [width, height] of [[2048, 900], [1600, 1200], [1280, 900], [1024, 768], [390, 900]]) {
    await page.setViewportSize({width, height});await settle();
    const geometry = await page.evaluate(() => {
      const rect = selector => document.querySelector(selector).getBoundingClientRect().toJSON();
      const surface = document.querySelector('[data-animation-svg]');
      return {layout: rect('.trace-player-layout'), main: rect('main'),
        figure: rect('.lws-animation'), surface: rect('[data-animation-svg]'), native: [surface.viewBox.baseVal.width, surface.viewBox.baseVal.height],
        scrollWidth: document.documentElement.scrollWidth};
    });
    assert.ok(geometry.main.x >= geometry.layout.x - 1 && geometry.main.right <= geometry.layout.right + 1,
      `${width}px: main must stay inside its content column`);
    assert.ok(geometry.figure.x >= geometry.main.x - 1 && geometry.figure.right <= geometry.main.right + 1,
      `${width}px: figure must stay inside the main column`);
    assert.ok(geometry.surface.width > 0 && geometry.surface.width <= geometry.figure.width + 1,
      `${width}px: drawing must fit its player`);
    assert.ok(Math.abs(geometry.surface.width / geometry.surface.height - 16 / 9) < .01, `${width}px: preserve SVG aspect ratio`);
    assert.deepEqual(geometry.native, [1920, 1080], `${width}px: CSS sizing must not reduce drawing resolution`);
    assert.ok(geometry.scrollWidth <= width + 1, `${width}px: no horizontal overflow`);
    if (width >= 1024) await assertViewportFit(`${width}×${height}`);
  }
  await page.setViewportSize(viewport);await settle();
}
async function assertViewportFit(label) {
  await page.evaluate(() => window.scrollTo({top: 0, behavior: 'instant'}));await settle();
  const sizes = await page.evaluate(() => new Promise(resolve => {
    const samples = [];
    const measure = () => {
      const stage = document.querySelector('.lws-animation__stage').getBoundingClientRect();
      samples.push([stage.width, stage.height]);
      if (samples.length === 8) resolve(samples.slice(-3));else requestAnimationFrame(measure);
    };
    requestAnimationFrame(measure);
  }));
  assert.ok(sizes.every(size => size.every((value, index) => Math.abs(value - sizes[0][index]) < 1)),
    `${label}: responsive control wrapping must settle without a repeated shrinking loop`);
  const geometry = await page.evaluate(() => {
    const rect = selector => document.querySelector(selector).getBoundingClientRect().toJSON();
    const main = document.querySelector('main');
    const sidebar = document.querySelector('#td-section-nav');
    const player = document.querySelector('[data-animation-player]'), controls = document.querySelector('[data-animation-controls]');
    const playerStyle = getComputedStyle(player);
    return {width: innerWidth, height: innerHeight, scrollY, scrollWidth: document.documentElement.scrollWidth,
      scrollHeight: document.documentElement.scrollHeight, mainScrollHeight: main.scrollHeight, mainHeight: main.clientHeight,
      sidebarOverflows: sidebar.scrollHeight > sidebar.clientHeight + 1,
      playerBorder: Number.parseFloat(playerStyle.borderLeftWidth) + Number.parseFloat(playerStyle.borderRightWidth),
      controlsScrollWidth: controls.scrollWidth, controlsWidth: controls.clientWidth,
      gutter: Number.parseFloat(getComputedStyle(document.documentElement).fontSize),
      header: rect('.td-navbar'), logo: rect('.navbar-logo'), subheader: rect('.trace-player-subheader'),
      title: rect('h1'), tabs: rect('.trace-player-tabs'), sidebar: rect('#td-section-nav'), aside: rect('.trace-player-sidebar'),
      main: rect('main'), toolbar: rect('.trace-player-toolbar'), figure: rect('.lws-animation'),
      exportStatus: rect('[data-trace-export-status]'),
      player: rect('[data-animation-player]'), surface: rect('[data-animation-svg]'), stage: rect('.lws-animation__stage'), controls: rect('[data-animation-controls]'),
      footer: rect('footer'), buttons: [...document.querySelectorAll('.trace-player-tabs button, .trace-player-toolbar button, .trace-player-toolbar select, [data-animation-controls] button, [data-animation-controls] select, [data-animation-seek]')]
        .filter(node => node.getClientRects().length).map(node => ({name: node.getAttribute('aria-label') || node.textContent.trim(), playback: controls.contains(node), ...node.getBoundingClientRect().toJSON()}))};
  });
  assert.ok(geometry.scrollWidth <= geometry.width + 1, `${label}: no horizontal document overflow`);
  assert.ok(geometry.scrollHeight <= geometry.height + 1, `${label}: entire player page must fit without document scrolling`);
  assert.ok(geometry.mainScrollHeight <= geometry.mainHeight + 1, `${label}: main content must fit without an inner scrollbar`);
  assert.equal(geometry.scrollY, 0, `${label}: document stays at its initial position`);
  assert.ok(geometry.subheader.x <= geometry.aside.x + 1 && geometry.subheader.right >= geometry.main.right - 1,
    `${label}: subheader spans both columns`);
  assert.ok(geometry.subheader.y >= geometry.header.bottom - 1 && geometry.title.y >= geometry.logo.bottom - 1,
    `${label}: compact subheader clears the global header and logo`);
  assert.ok(geometry.tabs.bottom - geometry.title.y <= 120, `${label}: title, description and tabs form a compact subheader`);
  assert.ok(geometry.aside.y - geometry.subheader.bottom <= 12 && geometry.main.y - geometry.subheader.bottom <= 12,
    `${label}: both columns start directly beneath the subheader`);
  for (const name of ['main', 'aside', 'figure', 'surface', 'controls']) {
    assert.ok(geometry[name].y >= geometry.subheader.bottom - 1, `${label}: ${name} is not covered by the subheader`);
    assert.ok(geometry[name].bottom <= geometry.footer.y + 1, `${label}: ${name} clears the footer`);
  }
  for (const name of ['toolbar', 'exportStatus']) {
    if (geometry[name].height > 0) assert.ok(geometry[name].y >= geometry.subheader.y - 1 &&
      geometry[name].bottom <= geometry.subheader.bottom + 1 && geometry[name].x >= geometry.subheader.x - 1 &&
      geometry[name].right <= geometry.subheader.right + 1, `${label}: ${name} stays inside the compact subheader`);
  }
  assert.ok(geometry.surface.x >= geometry.stage.x - 1 && geometry.surface.right <= geometry.stage.right + 1 &&
    geometry.surface.y >= geometry.stage.y - 1 && geometry.surface.bottom <= geometry.stage.bottom + 1,
    `${label}: complete SVG fits inside the stage`);
  assert.ok(geometry.stage.width > 0 && Math.abs(geometry.stage.width / geometry.stage.height - 16 / 9) < .01,
    `${label}: fitting must preserve a nonzero 16:9 drawing`);
  assert.ok(Math.abs(geometry.player.width - geometry.stage.width - geometry.playerBorder) <= 1,
    `${label}: player frame hugs the drawing without empty side panels`);
  assert.ok(Math.abs(geometry.controls.x - geometry.stage.x) <= 1 && Math.abs(geometry.controls.width - geometry.stage.width) <= 1,
    `${label}: playback controls align with the drawing width`);
  assert.ok(geometry.controlsScrollWidth <= geometry.controlsWidth + 1,
    `${label}: playback controls fit the narrower player without horizontal clipping`);
  assert.ok(geometry.surface.bottom <= geometry.controls.y + 1, `${label}: playback controls do not cover the drawing`);
  assert.ok(geometry.sidebar.y >= geometry.subheader.bottom - 1 && geometry.sidebar.bottom <= geometry.footer.y + 1,
    `${label}: scenario list stays between subheader and footer`);
  if (geometry.sidebarOverflows) assert.ok(geometry.footer.y - geometry.sidebar.bottom <= geometry.gutter + 1,
    `${label}: long scenario lists use the remaining screen height`);
  assert.ok(Math.abs(geometry.footer.x) < 1 && Math.abs(geometry.footer.right - geometry.width) < 1 && geometry.footer.bottom <= geometry.height + 1,
    `${label}: complete shared footer stays visible`);
  for (const button of geometry.buttons) {
    assert.ok(button.x >= -1 && button.right <= geometry.width + 1 && button.y >= geometry.header.bottom - 1 && button.bottom <= geometry.footer.y + 1,
      `${label}: ${button.name || 'timeline'} is fully visible and reachable`);
    if (button.playback) assert.ok(button.x >= geometry.controls.x - 1 && button.right <= geometry.controls.right + 1 &&
      button.y >= geometry.controls.y - 1 && button.bottom <= geometry.controls.bottom + 1,
      `${label}: ${button.name || 'timeline'} remains inside the playback controls`);
  }
}
async function assertMobileLayout(label) {
  await page.waitForFunction(() => ['--lws-header-height', '--lws-logo-bottom', '--lws-footer-height', '--lws-subheader-height']
    .every(name => Number.parseFloat(document.documentElement.style.getPropertyValue(name)) > 0));
  let subheaderTop;
  for (const fraction of [0, .5, 1]) {
    await page.evaluate(fraction => window.scrollTo({top: (document.documentElement.scrollHeight - innerHeight) * fraction, behavior: 'instant'}), fraction);
    await settle();
    const geometry = await page.evaluate(() => {
      const rect = selector => document.querySelector(selector).getBoundingClientRect().toJSON();
      return {width: innerWidth, height: innerHeight, scrollY, scroll: document.documentElement.scrollWidth, footer: rect('footer'),
        gutter: Number.parseFloat(getComputedStyle(document.documentElement).fontSize),
        header: rect('.td-navbar'), logo: rect('.navbar-logo'), sidebar: rect('#td-section-nav'), title: rect('h1'),
        subheader: rect('.trace-player-subheader'), tabs: rect('.trace-player-tabs'), aside: rect('.trace-player-sidebar'), main: rect('main')};
    });
    assert.ok(geometry.scroll <= geometry.width + 1, `${label}: horizontal overflow`);
    assert.ok(Math.abs(geometry.footer.x) < 1 && Math.abs(geometry.footer.right - geometry.width) < 1, `${label}: full-width footer`);
    if (fraction === 0) {
      subheaderTop = geometry.subheader.y;
      assert.ok(geometry.subheader.x <= geometry.aside.x + 1 && geometry.subheader.right >= geometry.main.right - 1,
        `${label}: subheader must span both content columns`);
      assert.ok(geometry.aside.y >= geometry.subheader.bottom && geometry.main.y >= geometry.subheader.bottom,
        `${label}: both columns must start below the subheader`);
    }
    assert.ok(Math.abs(geometry.subheader.y - subheaderTop) < 1,
      `${label}: title and tabs must stay pinned while the document scrolls`);
    assert.ok(geometry.subheader.y >= geometry.header.bottom - 1,
      `${label}: pinned subheader must remain below the global header`);
    assert.ok(geometry.title.y >= Math.max(geometry.header.bottom, geometry.logo.bottom) - 1,
      `${label}: pinned title must clear the global header and hanging logo`);
    assert.ok(geometry.title.y >= geometry.subheader.y && geometry.tabs.bottom <= geometry.subheader.bottom + 1,
      `${label}: title and tabs must remain inside the full-width subheader`);
    assert.ok(geometry.tabs.bottom <= geometry.height, `${label}: pinned tabs must remain visible`);
    if (fraction === 0) {
      assert.ok(geometry.title.y >= geometry.header.bottom - 1, `${label}: title under header`);
      assert.ok(geometry.title.y - geometry.header.bottom < 70, `${label}: excessive gap below mobile header`);
    }
  }
}
const styles = (target, selector, properties) => target.locator(selector).evaluate((node, properties) => {
  const computed = getComputedStyle(node);
  return Object.fromEntries(properties.map(name => [name, computed.getPropertyValue(name)]));
}, properties);
async function settledScroll() {
  await page.evaluate(() => new Promise(resolve => {
    let previous = '', stable = 0;
    const check = () => {
      const current = `${scrollY}/${document.querySelector('#td-section-nav').scrollTop}`;
      stable = current === previous ? stable + 1 : 0;previous = current;
      if (stable >= 4) resolve();else requestAnimationFrame(check);
    };
    requestAnimationFrame(check);
  }));
}
async function assertContainedScrolling() {
  await page.evaluate(() => {window.scrollTo({top: 0, behavior: 'instant'});document.querySelector('#td-section-nav').scrollTop = 0;});
  await settledScroll();
  const navigation = page.locator('#td-section-nav'), box = await navigation.boundingBox();
  await page.mouse.move(box.x + box.width / 2, box.y + Math.min(box.height / 2, 100));
  await page.mouse.wheel(0, 240);await settledScroll();
  assert.ok(await navigation.evaluate(node => node.scrollTop > 0), 'Wheel over sidebar scrolls its list');
  assert.equal(await page.evaluate(() => scrollY), 0, 'Sidebar wheel must not move the document');
  await navigation.evaluate(node => {node.scrollTop = node.scrollHeight;});await settledScroll();
  const last = await page.locator('[data-trace-scenario]').last().boundingBox();
  assert.ok(last.y >= box.y - 1 && last.y + last.height <= Math.min(box.y + box.height, page.viewportSize().height) + 1,
    'Last sidebar item must be visible after scrolling the list to its end');
  await page.mouse.wheel(0, 240);await settledScroll();
  assert.equal(await page.evaluate(() => scrollY), 0, 'Sidebar end must not chain wheel events into the document');
  const before = await page.evaluate(() => ({y: scrollY, heading: document.querySelector('.trace-player-subheader').getBoundingClientRect().y,
    main: document.querySelector('main').getBoundingClientRect().y}));
  const main = await page.locator('main').boundingBox();
  await page.mouse.move(main.x + main.width / 2, Math.min(main.y + 160, page.viewportSize().height - 80));
  await page.mouse.wheel(0, 100);await settledScroll();
  const after = await page.evaluate(() => ({y: scrollY, heading: document.querySelector('.trace-player-subheader').getBoundingClientRect().y,
    main: document.querySelector('main').getBoundingClientRect().y}));
  assert.equal(after.y, before.y, 'Wheel over the fitted player must not scroll the document');
  assert.ok(Math.abs(before.heading - after.heading) < 1 && Math.abs(before.main - after.main) < 1,
    'Title, tabs and player must stay in place while the scenario list scrolls');
}
async function assertStableScenarioSwitch(group) {
  await page.locator(`[data-trace-suite="${group.id}"]`).click();await ready();await settle();
  const entry=group.scenarios.at(-1),button=page.locator(`[data-trace-scenario="${entry.id}"]`);
  await button.scrollIntoViewIfNeeded();await settledScroll();
  const geometry=()=>page.evaluate(()=>({
    sidebarScroll:document.querySelector('#td-section-nav').scrollTop,documentScroll:scrollY,
    boxes:Object.fromEntries(['.trace-player-subheader','#td-section-nav','[data-animation-player]',
      '.lws-animation__stage','[data-animation-controls]'].map(selector=>{
      const {x,y,width,height}=document.querySelector(selector).getBoundingClientRect();return [selector,{x,y,width,height}];
    })),
  }));
  const before=await geometry(),drawing=await surface.innerHTML();
  assert.ok(before.sidebarScroll>0,`${group.id}: switching regression exercises a scrolled scenario list`);
  const unchanged=async phase=>{
    const current=await geometry();
    assert.equal(current.sidebarScroll,before.sidebarScroll,`${group.id}: ${phase} preserves the list position`);
    assert.equal(current.documentScroll,before.documentScroll,`${group.id}: ${phase} preserves the page position`);
    for(const [selector,box] of Object.entries(before.boxes)) for(const key of ['x','y','width','height'])
      assert.ok(Math.abs(current.boxes[selector][key]-box[key])<1,`${group.id}: ${phase} must not move or resize ${selector} (${key})`);
  };
  const url=new URL(entry.source,origin).href;
  let pending,intercepted;
  const captured=new Promise(resolve=>{intercepted=resolve;});
  const hold=route=>{pending=route;intercepted();};
  await page.route(url,hold);
  try {
    const requested=page.waitForRequest(url);
    await button.click();await requested;await captured;await settle();
    assert.equal(await status.textContent(),'Loading…');
    assert.equal(await page.locator('[data-animation-source]').getAttribute('aria-busy'),'true');
    assert.equal(await surface.innerHTML(),drawing,`${group.id}: old drawing stays visible while the response is held`);
    await unchanged('loading');
    await pending.continue();pending=undefined;
    await ready();await settle();
    assert.equal(await status.textContent(),entry.title);
    assert.equal(await surface.getAttribute('aria-label'),entry.title,'The requested source replaces the previous drawing');
    assert.equal(await page.locator('[data-animation-source]').getAttribute('aria-busy'),'false');
    await unchanged('completed switch');
  } finally {if(pending) await pending.continue();await page.unroute(url,hold);}
}

try {
  await visit();
  assert.equal(await page.locator('h1').textContent(), 'DisaggregatedSet Trace Player');
  assert.equal(await page.locator('.docs-version-nav').count(), 0, 'The hidden player omits the documentation switcher');
  assert.match(await page.locator('meta[name=robots]').getAttribute('content'), /noindex/);
  assert.doesNotMatch(await (await page.request.get(origin + '/sitemap.xml')).text(), /disaggregated-set-trace-visualizer/);
  assert.ok(requests.filter(url => url.includes('/scenarios/')).every(url => url.includes('ordinary--Ordinal--Deferred')),
    'Only the selected scenario is fetched on entry');
  assert.ok(!requests.some(url => /trace-export\.|trace-gif-worker\./.test(url)), 'Media encoders stay unloaded until export');
  assert.equal(await page.locator('[data-trace-theme-control]').isVisible(), false, 'Extra themes are hidden on the ordinary URL');
  assert.equal(await page.locator('[data-trace-export-status]').isVisible(), false, 'No export indicator before an export starts');
  await assertResponsiveSizing();
  const catalog = JSON.parse(await page.locator('#trace-player-catalog').textContent());
  assert.deepEqual(catalog.map(group => group.scenarios.length), [28, 20, 4]);
  for(const group of catalog.slice(0,2)) await assertStableScenarioSwitch(group);
  for (const group of catalog) {
    await page.locator(`[data-trace-suite="${group.id}"]`).click();
    assert.equal(await page.locator('[data-trace-scenario]').count(), group.scenarios.length);
    assert.equal(await page.locator('[data-trace-scenario]').first().evaluate(node => node.tagName), 'BUTTON');
    for (const entry of group.scenarios) {
      await page.locator(`[data-trace-scenario="${entry.id}"]`).click();
      await page.waitForFunction(title => document.querySelector('[data-trace-status]').textContent === title, entry.title);
      assert.equal(await page.locator('[data-animation-error]').isVisible(), false);
      assert.ok(Number(await page.locator('[data-animation-seek]').getAttribute('max')) > 0);
      assert.equal(await page.locator(`[data-trace-scenario="${entry.id}"]`).getAttribute('aria-current'), 'true');
    }
  }

  // Download/import is an exact local prepared-JSON round-trip, not an upload.
  const exportMenu = page.locator('[data-trace-export-menu]');
  await exportMenu.focus();await exportMenu.press('ArrowDown');
  await page.locator('[data-trace-export]').waitFor({state: 'visible'});
  assert.equal(await page.locator('[data-trace-export]').evaluate(node => node === document.activeElement), true,
    'Export menu keyboard navigation focuses its first format');
  const downloadPromise = page.waitForEvent('download');await page.keyboard.press('Enter');
  const download = await downloadPromise, exported = JSON.parse(await readFile(await download.path(), 'utf8'));
  assert.equal(await page.locator('[data-trace-export-status]').getAttribute('data-state'), 'done');
  assert.equal(await page.locator('[data-trace-export-done]').isVisible(), true, 'JSON export reports completion');
  assert.equal(exported.version, 1);assert.deepEqual(exported.themes, themes);
  assert.deepEqual([exported.width, exported.height], [1920, 1080], 'Prepared JSON retains native 1080p dimensions');
  await page.locator('[data-trace-suite="import"]').click();
  assert.equal(await surface.isVisible(), false);assert.equal(await page.locator('[data-trace-scenario]').count(), 0);
  const beforeImport = requests.length;
  await page.locator('[data-trace-file]').setInputFiles({name: 'my-scenario.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify(exported))});
  await ready();assert.equal(await status.textContent(), 'Imported: my-scenario');
  assert.equal(requests.length, beforeImport, 'Local JSON import must not upload or fetch anything');
  assert.equal(await surface.getAttribute('aria-label'), exported.title);
  await page.setViewportSize({width: 1024, height: 768});await assertViewportFit('Imported drawing with file picker');
  await page.setViewportSize({width: 1600, height: 1200});await settle();
  const previousDrawing = await surface.innerHTML(), previousState = await state();
  for (const invalid of ['{broken', JSON.stringify({version: 1, scenarios: []})]) {
    await page.locator('[data-trace-file]').setInputFiles({name: 'bad.json', mimeType: 'application/json', buffer: Buffer.from(invalid)});
    await page.waitForFunction(() => /not valid JSON|Unsupported animation/.test(document.querySelector('[data-trace-status]').textContent));
    assert.equal(await page.locator('[data-animation-error]').isVisible(), true, 'Failed imports show a visible error');
    assert.equal(await surface.innerHTML(), previousDrawing, 'Bad import preserves last-good drawing even when its error message changes available height');
    assert.deepEqual(await state(), previousState, 'Bad import preserves playback state');
  }
  await page.locator('[data-trace-suite="docs"]').click();await ready();
  await page.locator('[data-trace-suite="import"]').click();await ready();
  assert.equal(await status.textContent(), 'Imported: my-scenario', 'Import has its own retained workspace');
  assert.equal(await page.locator('[data-animation-error]').isVisible(), false);
  const gzip = await readFile(new URL('../static/trace-player/scenarios/small/ordinary--Ordinal--Deferred.json.gz', import.meta.url));
  await page.locator('[data-trace-file]').setInputFiles({name: 'second-scenario.json.gz', mimeType: 'application/gzip', buffer: gzip});
  await page.waitForFunction(() => document.querySelector('[data-trace-status]').textContent === 'Imported: second-scenario');
  assert.notEqual(await surface.getAttribute('aria-label'), exported.title);

  // The unlisted page uses the same real header/footer partials and fonts as docs.
  const docs = await browser.newPage({viewport: {width: 1600, height: 1200}});
  await docs.goto(origin + (process.env.LWS_DOCS_PREFIX ?? '/main') + '/docs/concepts/disaggregatedset/rollout-strategy/');
  await docs.evaluate(() => document.fonts.ready);
  await docs.waitForFunction(() => !document.querySelector('[data-animation-play]').disabled);
  const shell = target => target.evaluate(() => ({
    logo: document.querySelector('.navbar-logo').getAttribute('src'),
    links: [...document.querySelectorAll('.navbar-nav > .nav-item:not(.docs-version-nav) a')].map(node => [node.textContent.trim(), node.getAttribute('href')]),
    headerFont: getComputedStyle(document.querySelector('.td-navbar')).fontFamily,
    footerFont: getComputedStyle(document.querySelector('footer')).fontFamily,
    footer: document.querySelector('footer').textContent.replace(/\s+/g, ' ').trim(),
  }));
  assert.deepEqual(await shell(page), await shell(docs));
  assert.equal(await docs.locator('.docs-version-nav').count(), 1, 'Ordinary documentation retains its switcher');
  const scrollProperties = ['scrollbar-width','scrollbar-color','scrollbar-gutter','overscroll-behavior-y','padding-right'];
  const scrollStyle = await styles(page, '#td-section-nav', scrollProperties);
  assert.deepEqual(scrollStyle, await styles(docs, '#td-section-nav', scrollProperties), 'Scrollbar styling must match shared docs');
  // The OS may suppress overlay scrollbars; compare with the real docs above.
  assert.equal(scrollStyle['overscroll-behavior-y'], 'contain');
  const controlProperties = ['font-family','font-size','padding-top','padding-right','padding-bottom','padding-left','border-radius','min-height'];
  for (const control of ['play','previous','next','speed','fullscreen']) {
    assert.deepEqual(await styles(page, `[data-animation-${control}]`, controlProperties),
      await styles(docs, `[data-animation-${control}]`, controlProperties), `${control}: preserve shared player controls`);
  }
  await docs.evaluate(() => {document.documentElement.dataset.bsTheme = 'dark';});
  const darkScrollStyle = await styles(docs, '#td-section-nav', scrollProperties);
  assert.notEqual(darkScrollStyle['scrollbar-color'], scrollStyle['scrollbar-color'], 'Shared scrollbar must follow the site color theme');
  await docs.close();
  await select(catalog[0], catalog[0].scenarios[0]);await assertViewportFit('Desktop 1200px');
  await page.setViewportSize({width: 1600, height: 900});await assertViewportFit('Desktop 900px');await assertContainedScrolling();
  const documentScroll = await page.evaluate(() => scrollY);
  await page.locator('[data-trace-suite="large"]').click();await ready();
  assert.equal(await page.locator('#td-section-nav').evaluate(node => node.scrollTop), 0, 'Switching suites resets only the sidebar list');
  assert.equal(await page.evaluate(() => scrollY), documentScroll, 'Switching suites must not jump the document');
  await page.setViewportSize({width: 1600, height: 1200});
  const siteLight = await imageHash();
  await page.locator('#bd-theme').click();
  await page.locator('[data-bs-theme-value="dark"]').click();await settle();
  assert.equal(await page.locator('html').getAttribute('data-bs-theme'), 'dark');
  assert.deepEqual(await styles(page, '#td-section-nav', scrollProperties), darkScrollStyle, 'Dark scrollbar must match shared docs');
  assert.notEqual(await imageHash(), siteLight, 'Ordinary URL follows the site dark theme');
  await page.locator('#bd-theme').click();
  await page.locator('[data-bs-theme-value="light"]').click();await settle();
  assert.equal(await imageHash(), siteLight, 'Ordinary URL restores the site light theme');

  // Extra themes require an explicit query flag, retain state, and use distinct real palettes.
  await visit('?themes=all#suite=docs&theme=mistral-dark');
  assert.equal(await page.locator('[data-trace-theme-control]').isVisible(), true);
  assert.equal(await page.locator('[data-trace-theme]').inputValue(), 'mistral-dark');
  await page.setViewportSize({width: 1024, height: 768});await assertViewportFit('Extra theme controls');
  await page.setViewportSize({width: 1600, height: 1200});await settle();
  for (const group of [catalog[0], catalog[1], catalog[2]]) {
    await select(group, group.scenarios[0]);
    await page.locator('[data-animation-next]').click();await page.locator('[data-animation-speed]').selectOption('2');
    const original = await state(), hashes = new Set();
    for (const theme of themes) {
      await page.locator('[data-trace-theme]').selectOption(theme);await settle();
      assert.equal(await page.locator('[data-animation-source]').getAttribute('data-animation-theme'), theme);
      assert.deepEqual(await state(), original, `${group.id}: theme changes preserve playback state`);
      hashes.add(await imageHash());
    }
    assert.equal(hashes.size, themes.length, `${group.id}: all six palettes must be visually distinct`);
    const url = new URL(page.url());assert.equal(url.searchParams.get('themes'), 'all');
    assert.equal(new URLSearchParams(url.hash.slice(1)).get('theme'), 'llm-d-dark');
  }
  await page.locator('[data-trace-suite="small"]').click();await ready();
  assert.equal(await page.locator('[data-trace-theme]').inputValue(), 'llm-d-dark', 'Theme survives suite changes');
  await page.reload();await ready();
  assert.equal(await page.locator('[data-animation-source]').getAttribute('data-animation-theme'), 'llm-d-dark', 'Theme survives URL restore');
  await visit('?themes=all#suite=docs&theme=not-a-theme');
  assert.equal(await page.locator('[data-trace-theme]').inputValue(), '');
  assert.equal(await page.locator('[data-animation-source]').getAttribute('data-animation-theme'), null);
  assert.equal(new URLSearchParams(new URL(page.url()).hash.slice(1)).has('theme'), false, 'Unknown URL theme is removed');
  const fallback = await imageHash();await page.locator('[data-trace-theme]').selectOption('lws');await settle();
  assert.equal(await imageHash(), fallback, 'Invalid theme falls back to normal site light palette');
  await page.locator('[data-trace-theme]').selectOption('mistral-dark');
  await page.locator('[data-trace-suite="import"]').click();
  const legacy = {version: 1, width: 1920, height: 1080, title: 'Legacy pair',
    primitives: [{type: 'rect', x: 0, y: 0, width: 1920, height: 1080, fill: ['#ff0000', '#00ff00']}],
    frames: [{duration: 4, caption: 'Legacy palette compatibility.', items: [0]}]};
  await page.locator('[data-trace-file]').setInputFiles({name: 'legacy.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify(legacy))});
  await ready();
  await page.setViewportSize({width: 1024, height: 768});
  await assertViewportFit('Imported drawing with theme controls');
  // Reserve room for the real exporter progress/cancellation UI without encoding a video in this layout test.
  await page.locator('[data-trace-export-status]').evaluate(node => {
    node.hidden = false;node.dataset.state = 'progress';node.title = 'GIF export: 67%';node.setAttribute('aria-label', node.title);
    node.querySelector('[data-trace-export-label]').textContent = 'GIF';
    node.querySelector('[data-trace-export-progress]').setAttribute('aria-valuenow', '67');
    node.querySelector('[data-trace-export-ring]').setAttribute('stroke-dashoffset', '33');
  });
  await page.locator('[data-trace-cancel]').evaluate(node => {node.hidden = false;});
  await assertViewportFit('Imported drawing with export status and controls');
  await page.locator('[data-trace-export-status]').evaluate(node => {node.hidden = true;delete node.dataset.state;node.removeAttribute('title');node.removeAttribute('aria-label');});
  await page.locator('[data-trace-cancel]').evaluate(node => {node.hidden = true;});
  await page.setViewportSize({width: 1600, height: 1200});await settle();
  assert.equal(await page.locator('[data-trace-theme]').inputValue(), '', 'Unsupported current theme clears on legacy import');
  for (const name of themes.slice(2)) assert.equal(await page.locator(`[data-trace-theme] option[value="${name}"]`).evaluate(option => option.disabled), true);
  const pixel = () => surface.locator('defs > rect').first().getAttribute('fill');
  await page.locator('[data-trace-theme]').selectOption('lws-dark');await settle();
  assert.deepEqual(await pixel(), '#00ff00', 'Explicit legacy LWS dark overrides global light');
  await page.locator('[data-trace-theme]').selectOption('lws');await settle();
  assert.deepEqual(await pixel(), '#ff0000');
  await visit('#suite=docs&theme=mistral-dark');
  assert.equal(await page.locator('[data-trace-theme-control]').isVisible(), false);
  assert.equal(await page.locator('[data-animation-source]').getAttribute('data-animation-theme'), null, 'Hash alone cannot enable extra themes');

  // Mobile uses the existing header toggle; sidebar scrolling does not widen or cover the page.
  await page.setViewportSize({width: 390, height: 844});await visit();
  const navigation = page.locator('#td-section-nav'), toggle = page.locator('.docs-section-toggle');
  assert.equal(await navigation.isVisible(), false);await assertMobileLayout('Mobile');
  await page.evaluate(() => window.scrollTo({top: 0, behavior: 'instant'}));
  await exportMenu.click();await page.locator('[data-trace-export]').waitFor({state: 'visible'});
  const menuBox = await page.locator('[data-trace-export]').evaluate(node => node.closest('.dropdown-menu').getBoundingClientRect().toJSON());
  assert.ok(menuBox.x >= 0 && menuBox.right <= 390 && menuBox.bottom <= 844, 'Mobile export menu fits on screen');
  await page.keyboard.press('Escape');
  assert.equal(await exportMenu.getAttribute('aria-expanded'), 'false', 'Escape dismisses the export menu');
  assert.equal(await exportMenu.evaluate(node => node === document.activeElement), true, 'Escape restores focus to Export to');
  await toggle.click();await navigation.waitFor({state: 'visible'});
  await page.waitForFunction(() => document.querySelector('.docs-section-toggle').getAttribute('aria-expanded') === 'true');
  assert.ok((await navigation.boundingBox()).height <= 844 * .45 + 1);
  const last = catalog[0].scenarios.at(-1);
  await page.locator(`[data-trace-scenario="${last.id}"]`).click();
  await page.waitForFunction(title => document.querySelector('[data-trace-status]').textContent === title, last.title);
  await toggle.click();await navigation.waitFor({state: 'hidden'});await assertMobileLayout('Mobile selected');
  assert.deepEqual(errors, []);
  if (process.env.LWS_SCREENSHOT_DIR) await page.screenshot({path: `${process.env.LWS_SCREENSHOT_DIR}/hidden-trace-player-mobile.png`, fullPage: true});
  console.log('Hidden player passed: 52 choices, stable asynchronous scenario switches, local imports, theme/state preservation, viewport-fitted native 1080p drawing, compact full-width subheader, shared controls and thin scrollbars, independent full-height scenario scrolling, visible footer, and mobile navigation.');
} finally {await browser.close();}
