/** Playback of packaged, precomputed drawings. No controller or routing logic. */
import {SvgDrawing, drawingTimeline, position} from './animation-svg.ts';
type Color = string | string[];
type Point = [number, number];
interface Hit {x: number; y: number; width: number; height: number; label: string; detail: string}
interface Paint {fill?: Color; stroke?: Color; lineWidth?: number; opacity?: number}
export type Primitive = Paint & (
  | {type: 'rect'; x: number; y: number; width: number; height: number; radius?: number}
  | {type: 'circle'; x: number; y: number; radius: number}
  | {type: 'path'; points: Point[]; close?: boolean}
  | {type: 'text'; x: number; y: number; text: string; size: number; font?: 'sans' | 'mono'; bold?: boolean; align?: 'left' | 'center' | 'right'}
);
export interface Drawing {
  version: 1; width: number; height: number; title: string;
  themes?: string[];
  primitives: Primitive[];
  hits?: Hit[];
  frames: {duration: number; caption: string; items: number[]; hits?: number[]}[];
  motions?: {primitive: number; keyframes: [number, number, number][]}[];
}
export const MAX_DRAWING_BYTES = 16 * 1024 * 1024;
type LoadRequest = ({source: string} | {file: File} | {drawing: unknown}) & {requestId?: string | number};

/** Bound both compressed input and decoded JSON before allocating/parsing text. */
export async function readDrawingJSON(stream: ReadableStream<Uint8Array> | null, signal?: AbortSignal): Promise<unknown> {
  const bounded = async (body: ReadableStream<Uint8Array> | null): Promise<Blob> => {
    if (!body) throw new Error('Animation file is empty.');
    const reader = (signal ? body.pipeThrough(new TransformStream<Uint8Array, Uint8Array>(), {signal}) : body).getReader();
    const chunks: Uint8Array[] = [];
    let size = 0;
    try {
      while (true) {
        const {done, value} = await reader.read();
        if (done) return new Blob(chunks as BlobPart[]);
        size += value.byteLength;
        if (size > MAX_DRAWING_BYTES) throw new Error('Animation JSON must fit within 16 MiB.');
        chunks.push(value);
      }
    } catch (error) {await reader.cancel().catch(() => {}); throw error;}
    finally {reader.releaseLock();}
  };
  let blob = await bounded(stream);
  const magic = new Uint8Array(await blob.slice(0, 2).arrayBuffer());
  if (magic[0] === 0x1f && magic[1] === 0x8b) {
    if (typeof DecompressionStream === 'undefined') throw new Error('This browser needs an uncompressed JSON file.');
    blob = await bounded(blob.stream().pipeThrough(new DecompressionStream('gzip')));
  }
  try {return JSON.parse(await blob.text());}
  catch {throw new Error('Animation file is not valid JSON.');}
}

/** Structural bounds for prepared drawings, not an arbitrary trace-import format. */
export function parseDrawing(value: unknown): Drawing {
  const fail = (): never => {throw new Error('Unsupported animation drawing.');};
  const record = (v: unknown): Record<string, unknown> => v && typeof v === 'object' && !Array.isArray(v) ? v as Record<string, unknown> : fail();
  const number = (v: unknown, min = -100_000, max = 100_000): v is number => typeof v === 'number' && Number.isFinite(v) && v >= min && v <= max;
  const list = (v: unknown, limit: number): unknown[] => Array.isArray(v) && v.length <= limit ? v : fail();
  const data = record(value), themes = data.themes === undefined ? undefined : list(data.themes, 8);
  if (themes && (!themes.length || new Set(themes).size !== themes.length
    || !themes.every(theme => typeof theme === 'string' && /^[a-z][a-z0-9-]{0,63}$/.test(theme)))) fail();
  const color = (v: unknown): boolean => typeof v === 'string' ? /^#[\da-f]{6}(?:[\da-f]{2})?$/i.test(v)
    : Array.isArray(v) && v.length === (themes?.length ?? 2) && v.every(c => typeof c === 'string' && /^#[\da-f]{6}(?:[\da-f]{2})?$/i.test(c));
  const text = (v: unknown, max: number): boolean => typeof v === 'string' && v.length <= max;
  if (data.version !== 1 || !number(data.width, 1, 4096) || !number(data.height, 1, 4096) || !text(data.title, 500)) fail();
  const primitives = list(data.primitives, 100_000);
  for (const raw of primitives) {
    const p = record(raw);
    if (p.fill !== undefined && !color(p.fill) || p.stroke !== undefined && !color(p.stroke)) fail();
    if (p.lineWidth !== undefined && !number(p.lineWidth, 0, 100) || p.opacity !== undefined && !number(p.opacity, 0, 1)) fail();
    if (p.type === 'path') {
      const points = list(p.points, 1000);
      if (!points.length || points.some(point => !Array.isArray(point) || point.length !== 2 || !point.every(v => number(v)))) fail();
      if (p.close !== undefined && typeof p.close !== 'boolean') fail();
    } else {
      if (!number(p.x) || !number(p.y)) fail();
      if (p.type === 'rect') {
        if (!number(p.width, 0) || !number(p.height, 0) || p.radius !== undefined && !number(p.radius, 0)) fail();
      } else if (p.type === 'circle') {
        if (!number(p.radius, 0)) fail();
      } else if (p.type === 'text') {
        if (!text(p.text, 20_000) || !number(p.size, 1, 500) || !color(p.fill)) fail();
        if (p.font !== undefined && !['sans', 'mono'].includes(String(p.font)) || p.align !== undefined && !['left', 'center', 'right'].includes(String(p.align))) fail();
        if (p.bold !== undefined && typeof p.bold !== 'boolean') fail();
      } else fail();
    }
  }
  const index = (v: unknown): boolean => number(v, 0, primitives.length - 1) && Number.isInteger(v);
  const validateHit = (raw: unknown): Hit => {
    const hit = record(raw);
    if (!number(hit.x) || !number(hit.y) || !number(hit.width, 0) || !number(hit.height, 0)
      || !text(hit.label, 500) || !text(hit.detail, 20_000)) fail();
    return raw as Hit;
  };
  const indexedHits = data.hits !== undefined;
  const hits = Array.from(list(data.hits ?? [], 100_000), validateHit);
  const legacyFrames: Drawing['frames'] = [], legacyIndices = new Map<string, number>();
  let legacyHits = false, hitReferences = 0;
  const frames = list(data.frames, 10_000);
  if (!frames.length) fail();
  let items = 0, duration = 0;
  for (const raw of frames) {
    const frame = record(raw), indices = list(frame.items, 100_000);
    if (!number(frame.duration, .001, 600) || !text(frame.caption, 20_000) || !indices.every(index)) fail();
    items += indices.length; duration += frame.duration as number;
    const references = list(frame.hits ?? [], 10_000);
    hitReferences += references.length;
    if (hitReferences > 2_000_000) fail();
    if (indexedHits) {
      for (const ref of references) if (!number(ref, 0, hits.length - 1) || !Number.isInteger(ref)) fail();
    } else {
      const indices: number[] = [];
      for (const rawHit of references) {
        const hit = validateHit(rawHit);
        legacyHits = true;
        const key = JSON.stringify([hit.x, hit.y, hit.width, hit.height, hit.label, hit.detail]);
        if (!legacyIndices.has(key)) {
          if (hits.length >= 100_000) fail();
          legacyIndices.set(key, hits.length); hits.push(hit);
        }
        indices.push(legacyIndices.get(key)!);
      }
      legacyFrames.push({...frame, ...(frame.hits !== undefined ? {hits: indices} : {})} as Drawing['frames'][number]);
    }
  }
  if (items > 2_000_000 || duration > 86_400) fail();
  for (const raw of list(data.motions ?? [], 1000)) {
    const motion = record(raw), keys = list(motion.keyframes, 10_000);
    if (!index(motion.primitive) || keys.length < 2) fail();
    let previous = -1;
    for (const key of keys) {
      if (!Array.isArray(key) || key.length !== 3 || !number(key[0], 0, duration) || !number(key[1]) || !number(key[2]) || key[0] <= previous) fail();
      previous = (key as number[])[0];
    }
  }
  // Keep historical inline-hit exports usable without retaining repeated objects.
  // Validation finishes before returning a normalized copy; the input is untouched.
  return legacyHits ? {...data, hits, frames: legacyFrames} as Drawing : value as Drawing;
}

const clock = (time: number) => `${Math.floor(time / 60)}:${String(Math.floor(time) % 60).padStart(2, '0')}`;

export class AnimationPlayer {
  private root: HTMLElement;
  private data?: Drawing;
  private starts: number[] = [];
  private duration = 0;
  private time = 0;
  private lastFrame = -1;
  private lastSecond = -1;
  private lastPalette?: string;
  private lastRenderTime = -1;
  private wasMoving = false;
  private loading = false;
  private loadFailed = false;
  private lastInput?: LoadRequest;
  private visible = false;
  private intent = !matchMedia('(prefers-reduced-motion: reduce)').matches;
  private request = 0;
  private lastTick = 0;
  private loadVersion = 0;
  private loadAbort?: AbortController;
  private vector: SVGSVGElement;
  private svg?: SvgDrawing;
  private seek: HTMLInputElement;
  private play: HTMLButtonElement;
  private speed: HTMLButtonElement;
  private loop: HTMLInputElement;
  private fullscreen: HTMLButtonElement;
  private tooltip: HTMLElement;
  private clockEl: HTMLElement;
  private summaryEl: HTMLElement;

  constructor(root: HTMLElement) {
    this.root = root;
    const get = <T extends HTMLElement | SVGElement>(name: string) => root.querySelector<T>(`[data-animation-${name}]`)!;
    this.vector = get('svg');
    this.seek = get('seek'); this.play = get('play'); this.speed = get('speed'); this.loop = get('loop');
    this.fullscreen = get('fullscreen'); this.fullscreen.hidden = !document.fullscreenEnabled;
    this.tooltip = get('tooltip');
    this.clockEl = get('clock'); this.summaryEl = get('summary');
    this.vector.addEventListener('pointermove', event => this.inspect(event));
    this.vector.addEventListener('pointerdown', event => this.inspect(event));
    this.vector.addEventListener('pointerleave', () => {this.tooltip.hidden = true;});
    this.play.addEventListener('click', () => {
      if (!this.data) {void this.load(this.lastInput!); return;}
      this.intent = !this.playing;
      if (this.time >= this.duration) this.time = 0;
      this.render(); this.sync();
    });
    this.seek.addEventListener('input', () => this.seekTo(Number(this.seek.value)));
    this.seek.addEventListener('keydown', event => {
      const step = event.key === 'ArrowRight' || event.key === 'ArrowUp' ? 1
        : event.key === 'ArrowLeft' || event.key === 'ArrowDown' ? -1 : 0;
      if (!step) return;
      event.preventDefault(); this.seekTo(this.time + step);
    });
    const speeds = [...root.querySelectorAll<HTMLButtonElement>('[data-animation-speed-option]')];
    speeds.forEach(option => option.addEventListener('click', () => {
      this.speed.value = option.value;
      this.speed.textContent = option.textContent!.trim();
      this.speed.setAttribute('aria-label', `Playback speed: ${this.speed.textContent}`);
      speeds.forEach(item => item.setAttribute('aria-checked', String(item === option)));
      this.lastTick = performance.now(); this.speed.focus();
    }));
    for (const [name, step] of [['previous', -1], ['next', 1]] as const) get(name).addEventListener('click', () => {
      const index = Math.max(0, Math.min(this.starts.length - 1, position(this.starts, this.time) + step));
      this.seekTo(this.starts[index]);
    });
    this.fullscreen.addEventListener('click', async () => {
      try {if (document.fullscreenElement) await document.exitFullscreen(); else await get('player').requestFullscreen();}
      catch {this.error('Fullscreen is unavailable. The animation still works inline.');}
    });
    document.addEventListener('fullscreenchange', () => {
      const label = document.fullscreenElement === get('player') ? 'Exit fullscreen' : 'Enter fullscreen';
      this.fullscreen.setAttribute('aria-label', label); this.fullscreen.title = label;
    });
    new ResizeObserver(entries => get('player').style.setProperty('--animation-controls-height',
      `${Math.ceil(entries[0].target.getBoundingClientRect().height)}px`)).observe(get('controls'));
    if (typeof IntersectionObserver !== 'undefined') {
      new IntersectionObserver(entries => {this.visible = entries[0].isIntersecting; this.sync();}).observe(root);
    } else {this.visible = true;}
    document.addEventListener('visibilitychange', () => this.sync());
    matchMedia('(prefers-reduced-motion: reduce)').addEventListener('change', event => {
      if (event.matches) {this.intent = false; this.sync();}
    });
    const themes = new MutationObserver(() => this.render());
    themes.observe(document.documentElement, {attributes: true, attributeFilter: ['data-bs-theme']});
    themes.observe(root, {attributes: true, attributeFilter: ['data-animation-theme']});
    root.addEventListener('animation-load', event => {void this.load((event as CustomEvent<LoadRequest>).detail);});
    void this.load({source: root.dataset.animationSource!});
  }

  private get playing() {return !!this.data && this.intent && this.visible && !document.hidden;}
  private inspect(event: PointerEvent) {
    if (!this.data) return;
    const bounds = this.vector.getBoundingClientRect();
    const x = (event.clientX - bounds.left) * this.data.width / bounds.width;
    const y = (event.clientY - bounds.top) * this.data.height / bounds.height;
    const references = this.data.frames[position(this.starts, this.time)].hits ?? [];
    let hit: Hit | undefined;
    for (let i = references.length - 1; i >= 0; i--) {
      const candidate = this.data.hits![references[i]];
      if (x >= candidate.x && y >= candidate.y && x <= candidate.x + candidate.width && y <= candidate.y + candidate.height) {
        hit = candidate; break;
      }
    }
    this.tooltip.hidden = !hit;
    if (!hit) return;
    this.tooltip.textContent = `${hit.label}\n${hit.detail}`;
    this.tooltip.style.left = `${Math.max(8, Math.min(event.clientX - bounds.left + 12, bounds.width - this.tooltip.offsetWidth - 8))}px`;
    this.tooltip.style.top = `${Math.max(8, Math.min(event.clientY - bounds.top + 12, bounds.height - this.tooltip.offsetHeight - 8))}px`;
  }
  private error(message: string) {
    const error = this.root.querySelector<HTMLElement>('[data-animation-error]')!;
    error.textContent = message; error.hidden = false;
  }
  private async load(input: LoadRequest) {
    const version = ++this.loadVersion;
    this.loadAbort?.abort();
    const abort = this.loadAbort = new AbortController();
    this.lastInput = input; this.loading = true; this.loadFailed = false;
    this.root.setAttribute('aria-busy', 'true'); this.sync();
    try {
      if (!input || typeof input !== 'object' || ['source', 'file', 'drawing'].filter(key => key in input).length !== 1)
        throw new Error('Choose a prepared animation JSON file.');
      let raw: unknown;
      if ('drawing' in input) raw = input.drawing;
      else if ('file' in input) {
        if (!(input.file instanceof Blob)) throw new Error('Choose a prepared animation JSON file.');
        raw = await readDrawingJSON(input.file.stream(), abort.signal);
      } else {
        if (typeof input.source !== 'string' || !input.source) throw new Error('Choose an animation source.');
        const response = await fetch(input.source, {signal: abort.signal});
        if (!response.ok) throw new Error(`Animation could not load (${response.status}).`);
        if (Number(response.headers.get('content-length')) > MAX_DRAWING_BYTES) {
          await response.body?.cancel();
          throw new Error('Animation JSON must fit within 16 MiB.');
        }
        raw = await readDrawingJSON(response.body, abort.signal);
      }
      if (version !== this.loadVersion) return;
      const data = parseDrawing(raw);
      const {duration, starts} = drawingTimeline(data, true);
      // Commit only validated data; a failed import leaves the last-good drawing intact.
      if (this.request) cancelAnimationFrame(this.request);
      this.request = 0; this.time = 0; this.lastFrame = -1; this.lastTick = 0;
      this.lastSecond = -1; this.lastPalette = undefined; this.lastRenderTime = -1; this.wasMoving = false;
      this.data = data; this.duration = duration; this.starts = starts;
      this.tooltip.hidden = true;
      this.root.querySelector<HTMLElement>('[data-animation-error]')!.hidden = true;
      this.svg = new SvgDrawing(this.vector, data);
      this.vector.setAttribute('aria-label', data.title);
      this.root.style.setProperty('--animation-ratio', `${data.width} / ${data.height}`);
      this.root.style.setProperty('--animation-width-ratio', String(data.width / data.height));
      this.seek.max = String(duration);
      this.root.querySelectorAll<HTMLInputElement | HTMLButtonElement>('[data-animation-control]').forEach(control => {control.disabled = false;});
      this.render(); this.sync();
      this.root.dispatchEvent(new CustomEvent('animation-ready', {bubbles: true, detail: {
        drawing: data, source: 'source' in input ? input.source : undefined, requestId: input.requestId,
      }}));
    } catch (error) {
      if (version !== this.loadVersion) return;
      this.loadFailed = true;
      const message = error instanceof Error ? error.message : 'Animation could not load.';
      this.error(message);
      this.root.dispatchEvent(new CustomEvent('animation-error', {bubbles: true, detail: {message, requestId: input?.requestId}}));
    } finally {
      if (version === this.loadVersion) {
        this.loadAbort = undefined; this.loading = false;
        this.root.setAttribute('aria-busy', 'false'); this.sync();
      }
    }
  }
  private seekTo(time: number) {
    this.intent = false; this.time = Math.max(0, Math.min(this.duration, time));
    this.render(); this.sync();
  }
  private sync() {
    const active = this.playing;
    const label = !this.data && this.loadFailed ? 'Retry' : active ? 'Pause' : 'Play';
    this.play.textContent = label;
    this.play.setAttribute('aria-label', `${label} animation`);
    this.play.disabled = !this.data && (this.loading || !this.loadFailed);
    if (active && !this.request) {this.lastTick = performance.now(); this.request = requestAnimationFrame(this.tick);}
    if (!active && this.request) {cancelAnimationFrame(this.request); this.request = 0;}
  }
  private tick = (now: number) => {
    this.request = 0;
    if (!this.playing) return;
    this.time += Math.max(0, now - this.lastTick) / 1000 * Number(this.speed.value); this.lastTick = now;
    if (this.time >= this.duration) {
      if (this.loop.checked) this.time %= this.duration;
      else {this.time = this.duration; this.intent = false;}
    }
    this.render();
    if (this.playing) this.request = requestAnimationFrame(this.tick); else this.sync();
  };
  private render() {
    if (!this.data || !this.svg) return;
    const index = position(this.starts, this.time), frame = this.data.frames[index];
    const theme = this.root.dataset.animationTheme, dark = document.documentElement.dataset.bsTheme === 'dark';
    const palette = `${theme ?? ''}:${dark}`;
    const moving = drawingTimeline(this.data).motionTimes.some(times => this.time >= times[0] && this.time <= times[times.length - 1]);
    if (index !== this.lastFrame || palette !== this.lastPalette || moving !== this.wasMoving
      || moving && this.time !== this.lastRenderTime) {
      this.svg.draw(this.time, theme, dark);
      this.lastPalette = palette; this.wasMoving = moving; this.lastRenderTime = this.time;
    }
    this.seek.value = String(this.time);
    const second = Math.floor(this.time);
    if (second !== this.lastSecond) {
      this.seek.setAttribute('aria-valuetext', `${clock(second)} of ${clock(this.duration)}`);
      this.clockEl.textContent = `${clock(second)} / ${clock(this.duration)}`;
      this.lastSecond = second;
    }
    if (index !== this.lastFrame) {
      this.tooltip.hidden = true;
      this.summaryEl.textContent = frame.caption;
      this.lastFrame = index;
    }
  }
}

if (typeof document !== 'undefined') document.querySelectorAll<HTMLElement>('[data-animation-source]').forEach(root => new AnimationPlayer(root));
