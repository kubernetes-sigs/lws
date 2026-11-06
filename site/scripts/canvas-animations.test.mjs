import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
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

test('documentation pages embed all four prepared animations', async () => {
  const scenes = {
    'rollout-strategy': ['fractional'],
    'role-scaler': ['scaling-up', 'scaling-down'],
    'llm-d-integration': ['gating'],
  };
  for (const [page, assets] of Object.entries(scenes)) {
    const source = await read(`../content/en/docs/concepts/disaggregatedset/${page}/_index.md`);
    for (const scene of assets) assert.ok(source.includes(`data="/animations/${scene}.json"`), `${page}: ${scene}`);
    assert.doesNotMatch(source, /<video|\.mp4|\.gif/);
  }
});

test('all deployment contexts gate snapshots and the Hugo build on integration tests', async () => {
  const netlify = await read('../../netlify.toml');
  for (const context of ['build', 'context.deploy-preview', 'context.branch-deploy']) {
    const section = netlify.split(`[${context}]`)[1]?.split('\n[')[0] ?? '';
    const command = section.match(/^\s*command\s*=\s*"([^"]+)"/m)?.[1];
    assert.ok(command, `${context}: missing build command`);
    const steps = command.split('&&');
    const positions = ['npm ci', 'npm run test:integration', 'snapshot-docs.py', 'npm run build:html']
      .map(name => steps.findIndex(step => step.includes(name)));
    assert.ok(positions.every((position, index) => position >= 0 && (!index || position > positions[index - 1])),
      `${context}: install, tests, snapshots, and build must succeed in order`);
    if (context === 'build') assert.doesNotMatch(command, /--main-ref/);
    else assert.match(command, /--main-ref HEAD/);
  }
});
