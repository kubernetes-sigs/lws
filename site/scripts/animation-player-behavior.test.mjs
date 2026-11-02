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

function fixture(data = drawing(), {intersection = true, reducedMotion = true} = {}) {
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
    constructor(callback) {this.callback = callback; observers.push(this);}
    observe() {}
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
  new sandbox.Player(root);
  return {nodes, ctx, queries, observers, mutations, responses, document,
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

test('offscreen figures load immediately but autoplay only while visible', async () => {
  const f = fixture(drawing(), {reducedMotion: false});
  assert.equal(f.fetches(), 1, 'Loading starts without a visibility callback');
  assert.equal(f.root.attrs['aria-busy'], 'true');
  assert.equal(f.nodes.play.disabled, true);
  await f.ready();
  assert.equal(f.root.attrs['aria-busy'], 'false');
  assert.equal(f.nodes.play.disabled, false);
  assert.equal(f.nodes.play.textContent, 'Play');
  f.observers[0].fire(true);
  assert.equal(f.nodes.play.textContent, 'Pause', 'Autoplay begins only when the figure is actually visible');
  f.observers[0].fire(false);
  assert.equal(f.nodes.play.textContent, 'Play', 'Playback pauses when the figure leaves the viewport');
  assert.equal(f.fetches(), 1, 'Scrolling never downloads the same drawing again');
});

test('failed loads remain accessible and can be retried without reloading the page', async () => {
  const f = fixture(new Error('Network unavailable'));
  await f.ready();
  assert.equal(f.nodes.error.hidden, false);
  assert.equal(f.nodes.error.textContent, 'Network unavailable');
  assert.equal(f.nodes.play.textContent, 'Retry');
  assert.equal(f.nodes.play.attrs['aria-label'], 'Retry animation');
  assert.equal(f.nodes.play.disabled, false);
  assert.equal(f.root.attrs['aria-busy'], 'false');
  f.responses.splice(0, 1, drawing());
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
  assert.equal(f.nodes.play.disabled, false);
  assert.equal(f.nodes.play.textContent, 'Play');
  f.seek(2);
  assert.equal(f.nodes.summary.textContent, 'Second state');
});

test('pointer inspection resolves shared hit indices in reverse paint order', async () => {
  const data = drawing();
  data.hits = [
    {x: 10, y: 10, width: 50, height: 50, label: 'Underlying group', detail: 'First evidence'},
    {x: 10, y: 10, width: 50, height: 50, label: 'Top group', detail: 'Later evidence'},
  ];
  data.frames[0].hits = [0, 1]; data.frames[1].hits = [0];
  const f = fixture(data); await f.ready();
  const inspect = (x, y) => f.nodes.canvas.handlers.pointermove({clientX: x, clientY: y});
  inspect(20, 20);
  assert.equal(f.nodes.tooltip.hidden, false);
  assert.equal(f.nodes.tooltip.textContent, 'Top group\nLater evidence');
  f.seek(2);
  assert.equal(f.nodes.tooltip.hidden, true, 'Changing frames hides stale evidence');
  inspect(20, 20);
  assert.equal(f.nodes.tooltip.textContent, 'Underlying group\nFirst evidence');
  inspect(200, 200);
  assert.equal(f.nodes.tooltip.hidden, true);
});
