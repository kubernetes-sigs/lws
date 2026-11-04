import type {Drawing, Primitive} from './animation-player.ts';

export const svgElement = (tag: string, attributes: Record<string, string | number> = {}) => {
  const node = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, String(value));
  return node;
};

export function position(starts: readonly number[], time: number): number {
  let low = 0, high = starts.length;
  while (low < high) {const mid = (low + high) >>> 1; if (starts[mid] <= time) low = mid + 1; else high = mid;}
  return Math.max(0, low - 1);
}

const timelines = new WeakMap<Drawing, {duration: number; starts: number[]; motionTimes: number[][]}>();
export function drawingTimeline(data: Drawing, refresh = false) {
  const cached = timelines.get(data);
  if (cached && !refresh) return cached;
  let duration = 0;
  const starts = data.frames.map(frame => {const start = duration; duration += frame.duration; return start;});
  const timeline = {duration, starts, motionTimes: (data.motions ?? []).map(track => track.keyframes.map(key => key[0]))};
  timelines.set(data, timeline);
  return timeline;
}

function primitive(p: Primitive) {
  let node: SVGElement;
  if (p.type === 'rect') node = svgElement('rect', {x: p.x, y: p.y, width: p.width, height: p.height, rx: p.radius ?? 0});
  else if (p.type === 'circle') node = svgElement('circle', {cx: p.x, cy: p.y, r: p.radius});
  else if (p.type === 'path') node = svgElement('path', {d: p.points.map(([x, y], i) => `${i ? 'L' : 'M'}${x} ${y}`).join(' ') + (p.close ? ' Z' : '')});
  else {
    node = svgElement('text', {x: p.x, y: p.y, 'font-size': p.size,
      'font-family': p.font === 'mono' ? 'Menlo, monospace' : 'Arial, sans-serif',
      'font-weight': p.bold ? 700 : 400, 'dominant-baseline': 'central',
      'text-anchor': p.align === 'center' ? 'middle' : p.align === 'right' ? 'end' : 'start'});
    node.textContent = p.text;
    node.style.whiteSpace = 'pre';
  }
  node.setAttribute('opacity', String(p.opacity ?? 1));
  node.setAttribute('stroke-width', String(p.lineWidth ?? 1));
  return node;
}

let nextId = 0;
/** One renderer for the interactive view and exported frames. Input must be validated. */
export class SvgDrawing {
  private data: Drawing;
  private nodes = new Map<number, SVGElement>();
  private definitions = svgElement('defs');
  private frame = svgElement('g');
  private moving: SVGElement[];
  private lastFrame = -1;
  private lastPalette = -1;
  private prefix = `lws-svg-${++nextId}-`;

  constructor(root: SVGSVGElement, data: Drawing) {
    this.data = data;
    const motions = svgElement('g');
    this.moving = (data.motions ?? []).map(track => svgElement('use', {href: `#${this.prefix}${track.primitive}`, display: 'none'}));
    motions.append(...this.moving);
    root.setAttribute('viewBox', `0 0 ${data.width} ${data.height}`);
    root.replaceChildren(this.definitions, this.frame, motions);
  }

  private paint(node: SVGElement, id: number, palette: number) {
    for (const paint of ['fill', 'stroke'] as const) {
      const color = this.data.primitives[id][paint];
      node.setAttribute(paint, color === undefined ? 'none' : typeof color === 'string' ? color : color[palette]);
    }
  }

  draw(time: number, theme?: string, dark = false) {
    const data = this.data, timeline = drawingTimeline(data), named = data.themes?.indexOf(theme ?? '') ?? -1;
    const legacyDark = theme === 'lws-dark' || theme !== 'lws' && dark;
    const palette = named >= 0 ? named : data.themes ? Math.max(0, data.themes.indexOf(dark ? 'lws-dark' : 'lws')) : legacyDark ? 1 : 0;
    time = Math.max(0, Math.min(timeline.duration, Number.isFinite(time) ? time : 0));
    const index = position(timeline.starts, time);
    if (palette !== this.lastPalette) {
      this.nodes.forEach((node, id) => this.paint(node, id, palette));
      this.lastPalette = palette;
    }
    if (index !== this.lastFrame) {
      // Keep only this frame's definitions in the DOM/serialized SVG. Cache
      // previously seen geometry off-DOM rather than constructing an entire trace.
      const ids = new Set([...data.frames[index].items, ...(data.motions ?? []).map(track => track.primitive)]);
      this.definitions.replaceChildren(...[...ids].map(id => {
        let node = this.nodes.get(id);
        if (!node) {
          node = primitive(data.primitives[id]); node.id = this.prefix + id;
          this.paint(node, id, palette); this.nodes.set(id, node);
        }
        return node;
      }));
      this.frame.replaceChildren(...data.frames[index].items.map(id => svgElement('use', {href: `#${this.prefix}${id}`})));
      this.lastFrame = index;
    }
    (data.motions ?? []).forEach((motion, i) => {
      const times = timeline.motionTimes[i], node = this.moving[i];
      if (time < times[0] || time > times[times.length - 1]) {node.setAttribute('display', 'none'); return;}
      const key = Math.min(position(times, time), times.length - 2);
      const a = motion.keyframes[key], b = motion.keyframes[key + 1], t = (time - a[0]) / (b[0] - a[0]);
      node.removeAttribute('display');
      node.setAttribute('transform', `translate(${a[1] + (b[1] - a[1]) * t} ${a[2] + (b[2] - a[2]) * t})`);
    });
  }
}
