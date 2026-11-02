import type {Drawing, Primitive} from './animation-player.ts';

export interface MotionPosition {track: number; x: number; y: number}
let nextId = 0;
const element = (tag: string, attributes: Record<string, string | number> = {}) => {
  const node = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, String(value));
  return node;
};

function primitive(p: Primitive) {
  let node: SVGElement;
  if (p.type === 'rect') node = element('rect', {x: p.x, y: p.y, width: p.width, height: p.height, rx: p.radius ?? 0});
  else if (p.type === 'circle') node = element('circle', {cx: p.x, cy: p.y, r: p.radius});
  else if (p.type === 'path') node = element('path', {d: p.points.map(([x, y], i) => `${i ? 'L' : 'M'}${x} ${y}`).join(' ') + (p.close ? ' Z' : '')});
  else {
    node = element('text', {x: p.x, y: p.y, 'font-size': p.size,
      'font-family': p.font === 'mono' ? 'Menlo, monospace' : 'Arial, sans-serif',
      'font-weight': p.bold ? 700 : 400, 'dominant-baseline': 'central',
      'text-anchor': p.align === 'center' ? 'middle' : p.align === 'right' ? 'end' : 'start'});
    // Drawing strings are always text, never SVG markup.
    node.textContent = p.text;
    node.style.whiteSpace = 'pre';
  }
  node.setAttribute('opacity', String(p.opacity ?? 1));
  node.setAttribute('stroke-width', String(p.lineWidth ?? 1));
  return node;
}

/** Same validated drawing and logical coordinates, with retained vector nodes. */
export class SvgDrawing {
  private data: Drawing;
  private nodes: SVGElement[];
  private frame = element('g');
  private moving: SVGElement[];
  private visible = new Set<number>();
  private lastFrame = -1;
  private lastDark?: boolean;
  private prefix = `lws-svg-${++nextId}-`;

  constructor(root: SVGSVGElement, data: Drawing) {
    this.data = data;
    this.nodes = data.primitives.map((p, i) => {
      const node = primitive(p); node.id = this.prefix + i; return node;
    });
    const definitions = element('defs'); definitions.append(...this.nodes);
    const motions = element('g');
    this.moving = (data.motions ?? []).map(track => element('use', {href: `#${this.prefix}${track.primitive}`, display: 'none'}));
    motions.append(...this.moving);
    root.setAttribute('viewBox', `0 0 ${data.width} ${data.height}`);
    root.replaceChildren(definitions, this.frame, motions);
  }

  draw(index: number, dark: boolean, positions: MotionPosition[]) {
    if (dark !== this.lastDark) {
      this.nodes.forEach((node, i) => {
        for (const paint of ['fill', 'stroke'] as const) {
          const color = this.data.primitives[i][paint];
          node.setAttribute(paint, color === undefined ? 'none' : typeof color === 'string' ? color : color[dark ? 1 : 0]);
        }
      });
      this.lastDark = dark;
    }
    if (index !== this.lastFrame) {
      // Uses preserve paint order and repeated primitive references without
      // copying text/geometry. Only frame changes replace these lightweight nodes.
      this.frame.replaceChildren(...this.data.frames[index].items.map(i => element('use', {href: `#${this.prefix}${i}`})));
      this.lastFrame = index;
    }
    const active = new Set(positions.map(p => p.track));
    for (const i of this.visible) if (!active.has(i)) this.moving[i].setAttribute('display', 'none');
    for (const {track, x, y} of positions) {
      if (!this.visible.has(track)) this.moving[track].removeAttribute('display');
      this.moving[track].setAttribute('transform', `translate(${x} ${y})`);
    }
    this.visible = active;
  }
}
