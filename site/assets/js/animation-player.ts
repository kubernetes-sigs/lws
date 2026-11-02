/** Playback of packaged, precomputed drawings. No controller or routing logic. */
type Color = string | [string, string];
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
  primitives: Primitive[];
  hits?: Hit[];
  frames: {duration: number; caption: string; items: number[]; hits?: number[]}[];
  motions?: {primitive: number; keyframes: [number, number, number][]}[];
}

/** Structural bounds for packaged assets, not an arbitrary trace-import format. */
export function parseDrawing(value: unknown): Drawing {
  const fail = (): never => {throw new Error('Unsupported animation drawing.');};
  const record = (v: unknown): Record<string, unknown> => v && typeof v === 'object' && !Array.isArray(v) ? v as Record<string, unknown> : fail();
  const number = (v: unknown, min = -100_000, max = 100_000): v is number => typeof v === 'number' && Number.isFinite(v) && v >= min && v <= max;
  const list = (v: unknown, limit: number): unknown[] => Array.isArray(v) && v.length <= limit ? v : fail();
  const color = (v: unknown): boolean => typeof v === 'string' ? /^#[\da-f]{6}(?:[\da-f]{2})?$/i.test(v)
    : Array.isArray(v) && v.length === 2 && v.every(c => typeof c === 'string' && /^#[\da-f]{6}(?:[\da-f]{2})?$/i.test(c));
  const text = (v: unknown, max: number): boolean => typeof v === 'string' && v.length <= max;
  const data = record(value);
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

function drawPrimitive(ctx: CanvasRenderingContext2D, p: Primitive, dark: boolean) {
  const ink = (value: Color) => typeof value === 'string' ? value : value[dark ? 1 : 0];
  ctx.save(); ctx.globalAlpha = p.opacity ?? 1;
  if (p.fill) ctx.fillStyle = ink(p.fill);
  if (p.stroke) ctx.strokeStyle = ink(p.stroke);
  ctx.lineWidth = p.lineWidth ?? 1;
  if (p.type === 'text') {
    ctx.font = `${p.bold ? 700 : 400} ${p.size}px ${p.font === 'mono' ? 'Menlo, monospace' : 'Arial, sans-serif'}`;
    ctx.textAlign = p.align ?? 'left'; ctx.textBaseline = 'middle';
    ctx.fillText(p.text, p.x, p.y);
  } else {
    ctx.beginPath();
    if (p.type === 'rect') ctx.roundRect(p.x, p.y, p.width, p.height, p.radius ?? 0);
    else if (p.type === 'circle') ctx.arc(p.x, p.y, p.radius, 0, Math.PI * 2);
    else {
      ctx.moveTo(...p.points[0]);
      for (const point of p.points.slice(1)) ctx.lineTo(...point);
      if (p.close) ctx.closePath();
    }
    if (p.fill) ctx.fill();
    if (p.stroke) ctx.stroke();
  }
  ctx.restore();
}

/** Binary search for the last entry at or before time. */
function position(starts: readonly number[], time: number): number {
  let low = 0, high = starts.length;
  while (low < high) {const mid = (low + high) >>> 1; if (starts[mid] <= time) low = mid + 1; else high = mid;}
  return Math.max(0, low - 1);
}
const clock = (time: number) => `${Math.floor(time / 60)}:${String(Math.floor(time) % 60).padStart(2, '0')}`;

class AnimationPlayer {
  private root: HTMLElement;
  private data?: Drawing;
  private starts: number[] = [];
  private motionTimes: number[][] = [];
  private duration = 0;
  private time = 0;
  private lastFrame = -1;
  private lastSecond = -1;
  private lastDark?: boolean;
  private lastRenderTime = -1;
  private wasMoving = false;
  private loading = false;
  private loadFailed = false;
  private visible = false;
  private intent = !matchMedia('(prefers-reduced-motion: reduce)').matches;
  private request = 0;
  private lastTick = 0;
  private canvas: HTMLCanvasElement;
  private context: CanvasRenderingContext2D | null;
  private seek: HTMLInputElement;
  private play: HTMLButtonElement;
  private speed: HTMLSelectElement;
  private loop: HTMLInputElement;
  private fullscreen: HTMLButtonElement;
  private tooltip: HTMLElement;
  private clockEl: HTMLElement;
  private summaryEl: HTMLElement;

  constructor(root: HTMLElement) {
    this.root = root;
    const get = <T extends HTMLElement>(name: string) => root.querySelector<T>(`[data-animation-${name}]`)!;
    this.canvas = get('canvas'); this.context = this.canvas.getContext('2d');
    this.seek = get('seek'); this.play = get('play'); this.speed = get('speed'); this.loop = get('loop');
    this.fullscreen = get('fullscreen'); this.fullscreen.hidden = !document.fullscreenEnabled;
    this.tooltip = get('tooltip');
    this.clockEl = get('clock'); this.summaryEl = get('summary');
    this.canvas.addEventListener('pointermove', event => this.inspect(event));
    this.canvas.addEventListener('pointerdown', event => this.inspect(event));
    this.canvas.addEventListener('pointerleave', () => {this.tooltip.hidden = true;});
    this.play.addEventListener('click', () => {
      if (!this.data) {void this.load(); return;}
      this.intent = !this.playing;
      if (this.time >= this.duration) this.time = 0;
      this.render(); this.sync();
    });
    this.seek.addEventListener('input', () => this.seekTo(Number(this.seek.value)));
    this.seek.addEventListener('keydown', event => {
      const step = event.key === 'ArrowRight' || event.key === 'ArrowUp' ? 1
        : event.key === 'ArrowLeft' || event.key === 'ArrowDown' ? -1 : 0;
      if (!step) return;
      // Keep exact fractional positions with step="any", but make keyboard
      // scrubbing one second per press instead of a browser-dependent fraction.
      event.preventDefault(); this.seekTo(this.time + step);
    });
    this.speed.addEventListener('change', () => {this.lastTick = performance.now();});
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
    } else {
      this.visible = true;
    }
    document.addEventListener('visibilitychange', () => this.sync());
    matchMedia('(prefers-reduced-motion: reduce)').addEventListener('change', event => {
      if (event.matches) {this.intent = false; this.sync();}
    });
    new MutationObserver(() => this.render()).observe(document.documentElement, {attributes: true, attributeFilter: ['data-bs-theme']});
    // Fetch on page load; visibility only controls playback.
    void this.load();
  }

  private get playing() {return !!this.data && this.intent && this.visible && !document.hidden;}
  private inspect(event: PointerEvent) {
    if (!this.data) return;
    const bounds = this.canvas.getBoundingClientRect();
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
  private async load() {
    if (this.loading) return;
    this.loading = true; this.loadFailed = false;
    this.root.setAttribute('aria-busy', 'true');
    this.sync();
    try {
      if (!this.context) throw new Error('Canvas is unavailable in this browser.');
      const response = await fetch(this.root.dataset.animationSource!);
      if (!response.ok) throw new Error(`Animation could not load (${response.status}).`);
      this.data = parseDrawing(await response.json());
      this.duration = 0; this.time = 0;
      this.lastFrame = -1; this.lastSecond = -1; this.lastDark = undefined;
      this.lastRenderTime = -1; this.wasMoving = false;
      this.starts = this.data.frames.map(frame => {const start = this.duration; this.duration += frame.duration; return start;});
      this.motionTimes = (this.data.motions ?? []).map(track => track.keyframes.map(key => key[0]));
      this.canvas.width = this.data.width; this.canvas.height = this.data.height;
      this.canvas.setAttribute('aria-label', this.data.title);
      this.root.style.setProperty('--animation-ratio', `${this.data.width} / ${this.data.height}`);
      this.root.style.setProperty('--animation-width-ratio', String(this.data.width / this.data.height));
      this.seek.max = String(this.duration);
      this.root.querySelectorAll<HTMLInputElement | HTMLButtonElement | HTMLSelectElement>('[data-animation-control]').forEach(control => {control.disabled = false;});
      this.root.querySelector<HTMLElement>('[data-animation-error]')!.hidden = true;
      this.render();
    } catch (error) {
      this.loadFailed = true;
      this.error(error instanceof Error ? error.message : 'Animation could not load.');
    } finally {
      this.loading = false;
      this.root.setAttribute('aria-busy', 'false');
      this.sync();
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
    this.play.disabled = this.loading || !this.data && !this.loadFailed;
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
    if (!this.data || !this.context) return;
    const ctx = this.context, dark = document.documentElement.dataset.bsTheme === 'dark';
    const index = position(this.starts, this.time), frame = this.data.frames[index];
    const activeMotions = this.motionTimes.flatMap((times, i) =>
      this.time >= times[0] && this.time <= times[times.length - 1] ? [i] : []);
    const moving = activeMotions.length > 0;
    if (index !== this.lastFrame || dark !== this.lastDark || moving !== this.wasMoving
      || moving && this.time !== this.lastRenderTime) {
      ctx.clearRect(0, 0, this.data.width, this.data.height);
      for (const id of frame.items) drawPrimitive(ctx, this.data.primitives[id], dark);
      for (const i of activeMotions) {
        const motion = this.data.motions![i], times = this.motionTimes[i];
        const key = Math.min(position(times, this.time), times.length - 2);
        const a = motion.keyframes[key], b = motion.keyframes[key + 1], t = (this.time - a[0]) / (b[0] - a[0]);
        ctx.save(); ctx.translate(a[1] + (b[1] - a[1]) * t, a[2] + (b[2] - a[2]) * t);
        drawPrimitive(ctx, this.data.primitives[motion.primitive], dark); ctx.restore();
      }
      this.lastDark = dark; this.wasMoving = moving; this.lastRenderTime = this.time;
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
