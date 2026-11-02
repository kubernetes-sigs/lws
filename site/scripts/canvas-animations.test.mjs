import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const read = path => readFile(new URL(path, import.meta.url), 'utf8');
const script = await read('../assets/js/dark-mode.js');

function themeFixture({ stored = null, blocked = false, dark = false } = {}) {
  const handlers = new Map(), attrs = {};
  const root = { dataset: {}, removeAttribute() {} };
  const toggles = ['light', 'dark', 'auto'].map(value => ({
    dataset: { bsThemeValue: value },
    addEventListener: (name, fn) => handlers.set(`${value}:${name}`, fn),
    setAttribute: (key, next) => { attrs[`${value}:${key}`] = next; },
    classList: { toggle() {} },
    querySelector: () => ({ getAttribute: () => `#${value}-icon` }),
  }));
  const control = { setAttribute: (key, value) => { attrs[key] = value; }, focus() {} };
  const media = { matches: dark, addEventListener: (name, fn) => handlers.set(`media:${name}`, fn) };
  vm.runInNewContext(script, {
    document: { documentElement: root, readyState: 'complete',
      querySelectorAll: () => toggles, querySelector: () => control },
    localStorage: {
      getItem: () => { if (blocked) throw new Error('blocked'); return stored; },
      setItem: (_key, value) => { if (blocked) throw new Error('blocked'); stored = value; },
    },
    matchMedia: () => media,
    window: { addEventListener: (name, fn) => handlers.set(name, fn) },
  });
  return { root, attrs, stored: () => stored, choose: value => handlers.get(`${value}:click`)(),
    system: value => { media.matches = value; handlers.get('media:change')(); } };
}

test('Light/Dark persists; Auto follows the system; blocked storage is harmless', () => {
  for (const blocked of [false, true]) {
    const f = themeFixture({ dark: true, blocked });
    assert.equal(f.root.dataset.bsTheme, 'dark');
    assert.equal(f.attrs['auto:aria-pressed'], 'true');
    f.system(false);
    assert.equal(f.root.dataset.bsTheme, 'light');
    f.choose('dark');
    f.system(false);
    assert.equal(f.root.dataset.bsTheme, 'dark');
    assert.equal(f.attrs['aria-label'], 'Color theme: Dark');
    if (!blocked) assert.equal(themeFixture({ stored: f.stored() }).root.dataset.bsTheme, 'dark');
    f.choose('auto');
    assert.equal(f.root.dataset.bsTheme, 'light');
  }
});

test('documentation embeds local prepared data without a separate application', async () => {
  const scenes = {
    'rollout-strategy': ['fractional'],
    'scaling-during-rollout': ['scaling-up', 'scaling'],
    'llm-d-integration': ['gating'],
  };
  for (const [page, assets] of Object.entries(scenes)) {
    const source = await read(`../content/en/docs/concepts/disaggregatedset/${page}/_index.md`);
    for (const scene of assets) assert.ok(source.includes(`data="/animations/${scene}.json"`), `${page}: ${scene}`);
    assert.doesNotMatch(source, /<video|\.mp4|\.gif/);
  }
  const shortcode = await read('../layouts/shortcodes/canvas-animation.html');
  assert.match(shortcode, /js.Build/);
  assert.match(shortcode, /relURL/);
  assert.doesNotMatch(shortcode, /iframe|scene=/);
});

test('replaced videos and posters are not shipped in current static assets', async () => {
  for (const name of ['fractional-rollout', 'scale-down-comparison', 'llm-d-routing']) {
    for (const path of [`videos/disaggregatedset-${name}.mp4`, `images/disaggregatedset-${name}-poster.png`]) {
      await assert.rejects(readFile(new URL(`../static/${path}`, import.meta.url)), { code: 'ENOENT' });
    }
  }
});

test('gating subtitle introduces the role counts without a release label', async () => {
  const drawing = JSON.parse(await read('../static/animations/gating.json'));
  assert.equal(drawing.primitives.find(item => item.type === 'text' && item.y === 94).text, '5 Prefill + 13 Decode');
});

test('animation labels are release-independent; source provenance remains separate', async () => {
  const directory = new URL('../static/animations/', import.meta.url);
  for (const name of (await readdir(directory)).filter(name => name.endsWith('.json'))) {
    const drawing = JSON.parse(await readFile(new URL(name, directory), 'utf8'));
    const labels = [drawing.title, ...drawing.primitives.map(item => item.text),
      ...(drawing.hits ?? []).flatMap(hit => [hit.label, hit.detail]),
      ...drawing.frames.map(frame => frame.caption)].filter(Boolean);
    for (const label of labels) assert.doesNotMatch(label, /\bLWS\s+(?:v(?:ersion)?\s*)?\d|\bPR\b|\bpull request\b/i, name);
  }
});

test('scaling drawings leave room below status lines and above the annotation band', async () => {
  for (const scene of ['scaling', 'scaling-up']) {
    const drawing = JSON.parse(await read(`../static/animations/${scene}.json`));
    for (const [index, frame] of drawing.frames.entries()) {
      const items = frame.items.map(id => drawing.primitives[id]);
      const hits = (frame.hits ?? []).map(id => drawing.hits[id]);
      const capacities = hits.filter(hit => hit.width === 900 && hit.height === 42);
      const annotation = hits.find(hit => hit.width === 1840 && hit.height === 56);
      assert.equal(capacities.length, 2, `${scene}/${index}: two capacity panels`);
      assert.ok(annotation, `${scene}/${index}: annotation band`);
      for (const capacity of capacities) {
        const below = items.filter(item => item.type === 'text' && item.x >= capacity.x &&
          item.x < capacity.x + capacity.width && item.y > capacity.y + capacity.height);
        const status = below.find(item => item.align === 'center');
        const heading = below.find(item => item.align === 'left');
        assert.ok(status && heading, `${scene}/${index}: status and role heading`);
        assert.ok(heading.y - heading.size / 2 - status.y - status.size / 2 >= 16,
          `${scene}/${index}: status needs space above the role headings`);
      }
      for (const group of hits.filter(hit => hit.width === hit.height && hit.width <= 27)) {
        assert.ok(group.y + group.height <= annotation.y - 16, `${scene}/${index}: group grid touches annotation`);
      }
    }
  }
});

test('all deployment contexts gate snapshots and the Hugo build on integration tests', async () => {
  const netlify = await read('../../netlify.toml');
  const pkg = JSON.parse(await read('../package.json'));
  assert.match(pkg.scripts['build:html'], /hugo --source _generated/);
  assert.doesNotMatch(netlify, /pnpm|build:animations|build:player/);
  const commands = [...netlify.matchAll(/command = "([^"]+)"/g)].map(match => match[1]);
  assert.equal(commands.length, 3);
  for (const command of commands) {
    assert.match(command, /^npm ci && npm run test:integration && python3 .*snapshot-docs\.py.*&& npm run build:html$/);
  }
  assert.doesNotMatch(commands[0], /--main-ref/);
  for (const command of commands.slice(1)) assert.match(command, /--main-ref HEAD/);
});

test('shared navigation retains upstream versions, combined branding, and theme menu', async () => {
  const navbar = await read('../layouts/partials/navbar.html');
  for (const part of ['images/lws-ds-logos.svg', 'navbar-version-selector.html', 'docs-section-toggle', 'theme-toggler.html']) {
    assert.ok(navbar.includes(part));
  }
});
