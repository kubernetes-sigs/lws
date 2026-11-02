import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {stripTypeScriptTypes} from 'node:module';
import test from 'node:test';
import vm from 'node:vm';

const source = stripTypeScriptTypes(await readFile(new URL('../assets/js/animation-player.ts', import.meta.url), 'utf8'))
  .replace(/^export /gm, '');
const drawing = () => ({version: 1, width: 1920, height: 1080, title: 'Example',
  primitives: [{type: 'rect', x: 0, y: 0, width: 1920, height: 1080, fill: ['#ffffff', '#101720']},
    {type: 'circle', x: 0, y: 0, radius: 4, fill: '#326ce5'}],
  frames: [{duration: 2, caption: 'First state', items: [0]}, {duration: 2, caption: 'Second state', items: [0]}],
});

function fixture(data = drawing(), {intersection = true, near = true, reducedMotion = true} = {}) {
  const observers = [], mutations = [], queries = [], responses = [data];
  const ctx = {clears: 0, translations: [], clearRect() {this.clears++;},
    translate(...point) {this.translations.push(point);}, save() {}, restore() {},
    beginPath() {}, roundRect() {}, arc() {}, fill() {}, stroke() {}};
  const element = () => {
    let content = '';
    return {handlers: {}, attrs: {}, writes: {}, hidden: false, disabled: true,
      style: {setProperty() {}}, value: '1', checked: true,
      get textContent() {return content;},
      set textContent(value) {content = value; this.writes.textContent = (this.writes.textContent ?? 0) + 1;},
      addEventListener(name, callback) {this.handlers[name] = callback;},
      setAttribute(name, value) {this.attrs[name] = value; this.writes[name] = (this.writes[name] ?? 0) + 1;},
      getBoundingClientRect: () => ({left: 0, top: 0, width: 1920, height: 1080}),
      offsetWidth: 100, offsetHeight: 50,
    };
  };
  const names = ['canvas', 'seek', 'play', 'speed', 'loop', 'fullscreen', 'tooltip', 'clock', 'summary',
    'previous', 'next', 'player', 'controls', 'error'];
  const nodes = Object.fromEntries(names.map(name => [name, element()]));
  nodes.canvas.getContext = () => ctx;
  nodes.error.hidden = true;
  const root = {...element(), dataset: {animationSource: '/example.json'},
    querySelector(selector) {queries.push(selector); return nodes[selector.match(/data-animation-([^\]]+)/)[1]];},
    querySelectorAll: () => names.filter(name => !['error', 'summary', 'clock'].includes(name)).map(name => nodes[name]),
  };
  const document = {documentElement: {dataset: {bsTheme: 'light'}}, hidden: false,
    querySelectorAll: () => [], addEventListener() {}, fullscreenEnabled: false};
  let fetches = 0;
  class Observer {
    constructor(callback, options) {this.callback = callback; this.options = options; observers.push(this);}
    observe() {}
    disconnect() {this.disconnected = true;}
    fire(visible) {this.callback([{isIntersecting: visible}]);}
  }
  const sandbox = {document, console, Error, performance: {now: () => 0},
    matchMedia: () => ({matches: reducedMotion, addEventListener() {}}),
    ResizeObserver: class {observe() {}},
    MutationObserver: class {constructor(callback) {mutations.push(callback);} observe() {}},
    requestAnimationFrame: () => 1, cancelAnimationFrame() {},
    fetch: async () => {
      fetches++;
      const response = responses.length > 1 ? responses.shift() : responses[0];
      if (response instanceof Error) throw response;
      return {ok: true, json: async () => structuredClone(response)};
    },
    ...(intersection ? {IntersectionObserver: Observer} : {}),
  };
  vm.runInNewContext(`${source}\nthis.Player = AnimationPlayer;`, sandbox);
  const player = new sandbox.Player(root);
  if (near) observers.find(observer => observer.options?.rootMargin)?.fire(true);
  return {player, nodes, ctx, queries, observers, mutations, responses, document,
    fetches: () => fetches,
    ready: () => new Promise(resolve => setImmediate(resolve)),
    seek(time) {nodes.seek.value = String(time); nodes.seek.handlers.input();},
    theme(theme) {document.documentElement.dataset.bsTheme = theme; mutations.forEach(callback => callback([]));},
    root,
  };
}

test('static playback caches drawing and DOM nodes; clock text changes once per second', async () => {
  const f = fixture(); await f.ready();
  const {clock, seek, summary} = f.nodes;
  const initialQueries = f.queries.length;
  assert.equal(f.ctx.clears, 1);
  f.seek(.2); f.seek(.9);
  assert.equal(f.ctx.clears, 1);
  assert.equal(clock.writes.textContent, 1);
  assert.equal(seek.writes['aria-valuetext'], 1);
  f.seek(1); f.seek(1.9);
  assert.equal(clock.textContent, '0:01 / 0:04');
  assert.equal(clock.writes.textContent, 2);
  assert.equal(seek.writes['aria-valuetext'], 2);
  assert.equal(summary.writes.textContent, 1);
  assert.equal(f.ctx.clears, 1);
  assert.equal(f.queries.length, initialQueries, 'Rendering does not look up clock or summary again');
  f.seek(2);
  assert.equal(f.ctx.clears, 2);
  assert.equal(summary.textContent, 'Second state');
  f.theme('dark');
  assert.equal(f.ctx.clears, 3, 'A paused frame repaints when its theme changes');
  f.theme('dark');
  assert.equal(f.ctx.clears, 3, 'An unchanged theme does not repaint');
  f.seek(.5);
  assert.equal(f.ctx.clears, 4);
  assert.equal(summary.textContent, 'First state');
  assert.equal(clock.textContent, '0:00 / 0:04');
});

test('motion entry, advancement, exit and backward seeks repaint without leaving a trail', async () => {
  const data = drawing();
  data.motions = [{primitive: 1, keyframes: [[.5, 10, 10], [1.5, 30, 30]]}];
  const f = fixture(data); await f.ready();
  f.seek(.4); assert.equal(f.ctx.clears, 1);
  f.seek(.5); assert.equal(f.ctx.clears, 2);
  f.seek(1); assert.equal(f.ctx.clears, 3);
  assert.deepEqual(f.ctx.translations.at(-1), [20, 20]);
  f.seek(1); assert.equal(f.ctx.clears, 3, 'A paused motion at the same position is cached');
  f.seek(1.6); assert.equal(f.ctx.clears, 4, 'Leaving a motion clears its last position');
  f.seek(1.8); assert.equal(f.ctx.clears, 4);
  f.seek(.6); assert.equal(f.ctx.clears, 5);
  f.theme('dark'); assert.equal(f.ctx.clears, 6);
});

test('loading another drawing resets duration, captions and render caches', async () => {
  const f = fixture(); await f.ready();
  f.seek(2.5);
  const replacement = drawing();
  replacement.frames = [{duration: 1.25, caption: 'Replacement state', items: [0]}];
  f.responses[0] = replacement;
  await f.player.load();
  assert.equal(f.nodes.seek.max, '1.25');
  assert.equal(f.nodes.seek.value, '0');
  assert.equal(f.nodes.clock.textContent, '0:00 / 0:01');
  assert.equal(f.nodes.summary.textContent, 'Replacement state');
  assert.equal(f.ctx.clears, 3);
});

test('offscreen figures load only within the 300px preload margin and do not autoplay while merely near', async () => {
  const f = fixture(drawing(), {near: false, reducedMotion: false}); await f.ready();
  assert.equal(f.fetches(), 0);
  assert.equal(f.nodes.play.disabled, true);
  const preload = f.observers.find(observer => observer.options?.rootMargin);
  assert.equal(preload.options.rootMargin, '300px');
  preload.fire(false); await f.ready();
  assert.equal(f.fetches(), 0);
  preload.fire(true); preload.fire(true);
  assert.equal(f.root.attrs['aria-busy'], 'true');
  await f.ready();
  assert.equal(f.fetches(), 1, 'Overlapping observer callbacks cannot start duplicate fetches');
  assert.equal(preload.disconnected, true);
  assert.equal(f.root.attrs['aria-busy'], 'false');
  assert.equal(f.nodes.play.disabled, false);
  assert.equal(f.nodes.play.textContent, 'Play');
  assert.equal(f.player.visible, false, 'Preload margin is separate from playback visibility');
  f.observers.find(observer => !observer.options).fire(true);
  assert.equal(f.nodes.play.textContent, 'Pause', 'Autoplay begins only when the figure is actually visible');
});

test('failed lazy loads remain accessible and can be retried without reloading the page', async () => {
  const f = fixture(drawing(), {near: false});
  f.responses.splice(0, 1, new Error('Network unavailable'), drawing());
  f.observers.find(observer => observer.options?.rootMargin).fire(true);
  await f.ready();
  assert.equal(f.nodes.error.hidden, false);
  assert.equal(f.nodes.error.textContent, 'Network unavailable');
  assert.equal(f.nodes.play.textContent, 'Retry');
  assert.equal(f.nodes.play.attrs['aria-label'], 'Retry animation');
  assert.equal(f.nodes.play.disabled, false);
  assert.equal(f.root.attrs['aria-busy'], 'false');
  f.nodes.play.handlers.click();
  assert.equal(f.nodes.play.disabled, true);
  await f.ready();
  assert.equal(f.fetches(), 2);
  assert.equal(f.nodes.error.hidden, true);
  assert.equal(f.nodes.play.textContent, 'Play');
  assert.equal(f.nodes.play.disabled, false);
  assert.equal(f.ctx.clears, 1);
});

test('browsers without IntersectionObserver load immediately with usable paused controls', async () => {
  const f = fixture(drawing(), {intersection: false}); await f.ready();
  assert.equal(f.fetches(), 1);
  assert.equal(f.player.visible, true);
  assert.equal(f.nodes.play.disabled, false);
  assert.equal(f.nodes.play.textContent, 'Play');
  f.seek(2);
  assert.equal(f.nodes.summary.textContent, 'Second state');
});
