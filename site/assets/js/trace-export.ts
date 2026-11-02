/** Lazy per-scenario export from the same SVG renderer used for playback. */
import type {Drawing} from './animation-player.ts';
import {SvgDrawing, svgElement} from './animation-svg.ts';

export interface ExportOptions {
  drawing: Drawing; format: 'gif' | 'video'; speed: number;
  theme?: string; dark?: boolean; signal: AbortSignal; onProgress: (fraction: number) => void;
  gifWorkerURL?: string; videoModuleURL?: string;
}
export const EXPORT_LIMITS = {gif: {seconds: 600, frames: 1200, bytes: 64 * 1024 * 1024},
  video: {seconds: 600, bytes: 128 * 1024 * 1024}} as const;
const WIDTH = 1920, HEIGHT = 1080;
const FPS = 24;
const canceled = () => new DOMException('Export canceled.', 'AbortError');
const check = (signal: AbortSignal) => {if (signal.aborted) throw canceled();};

/** Availability is browser/OS-dependent. A WebM fallback is never mislabeled MP4. */
export async function videoSupport() {
  if (typeof VideoEncoder === 'undefined' || typeof VideoFrame === 'undefined') return null;
  for (const candidate of [
    {codec: 'avc', fullCodecString: 'avc1.420028', extension: 'mp4', mimeType: 'video/mp4'},
    {codec: 'vp9', fullCodecString: 'vp09.00.40.08', extension: 'webm', mimeType: 'video/webm'},
    {codec: 'vp8', fullCodecString: 'vp8', extension: 'webm', mimeType: 'video/webm'},
  ] as const) {
    try {
      if ((await VideoEncoder.isConfigSupported({codec: candidate.fullCodecString, width: WIDTH,
        height: HEIGHT, bitrate: 5_000_000, framerate: FPS})).supported) return candidate;
    } catch { /* A rejected codec does not rule out the next one. */ }
  }
  return null;
}

export function exportDuration(drawing: Drawing, speed: number): number {
  if (!Number.isFinite(speed) || speed < .1 || speed > 16) throw new Error('Choose a speed between 0.1× and 16×.');
  const duration = drawing.frames.reduce((sum, frame) => sum + frame.duration, 0) / speed;
  if (!Number.isFinite(duration) || duration <= 0) throw new Error('This scenario has no duration to export.');
  return duration;
}

/** Static holds need only one GIF frame. Motion is sampled without skipping state boundaries. */
export function gifSchedule(drawing: Drawing, speed: number): {time: number; delay: number}[] {
  if (exportDuration(drawing, speed) > EXPORT_LIMITS.gif.seconds) throw new Error('GIF is limited to 10 minutes. Increase playback speed.');
  const segments: {time: number; duration: number; items: number[]}[] = [];
  let time = 0;
  for (const frame of drawing.frames) {
    const previous = segments.at(-1);
    if (!drawing.motions?.length && previous && frame.items.length === previous.items.length && frame.items.every((id, i) => id === previous.items[i])) previous.duration += frame.duration;
    else segments.push({time, duration: frame.duration, items: frame.items});
    time += frame.duration;
  }
  const frames: {time: number; delay: number}[] = [];
  for (const segment of segments) {
    const count = drawing.motions?.length ? Math.ceil(segment.duration / speed * 10) : 1;
    if (frames.length + count > EXPORT_LIMITS.gif.frames) throw new Error('GIF is limited to 1,200 encoded frames. Increase playback speed or choose video.');
    for (let i = 0; i < count; i++) {
      const start = segment.time + segment.duration * i / count, end = segment.time + segment.duration * (i + 1) / count;
      const delay = (Math.round(end / speed * 100) - Math.round(start / speed * 100)) * 10;
      if (delay < 20) throw new Error('A state is shorter than GIF’s portable 0.02-second minimum. Lower playback speed to retain it.');
      frames.push({time: start, delay});
    }
  }
  return frames;
}

export interface ExportFrame {time: number; timestamp: number; duration: number}
/** Encode static holds once; sample motion while retaining every state boundary. */
export function videoSchedule(drawing: Drawing, speed: number): ExportFrame[] {
  if (exportDuration(drawing, speed) > EXPORT_LIMITS.video.seconds) throw new Error('Video is limited to 10 minutes. Increase playback speed.');
  const frames: ExportFrame[] = [];
  let time = 0;
  for (const frame of drawing.frames) {
    const count = drawing.motions?.length ? Math.ceil(frame.duration / speed * FPS) : 1;
    if (frames.length + count > 15_000) throw new Error('Video is limited to 15,000 encoded frames. Increase playback speed.');
    for (let i = 0; i < count; i++) {
      const start = time + frame.duration * i / count, end = time + frame.duration * (i + 1) / count;
      const timestamp = Math.round(start / speed * 1_000_000) / 1_000_000;
      const duration = Math.round(end / speed * 1_000_000) / 1_000_000 - timestamp;
      if (duration < .001 - 1e-9) throw new Error('A state is shorter than one millisecond. Lower playback speed to retain it.');
      frames.push({time: start, timestamp, duration});
    }
    time += frame.duration;
  }
  return frames;
}

async function abortable<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  check(signal);
  return new Promise((resolve, reject) => {
    const abort = () => reject(canceled());
    signal.addEventListener('abort', abort, {once: true});
    promise.then(value => {signal.removeEventListener('abort', abort); resolve(value);},
      error => {signal.removeEventListener('abort', abort); reject(error);});
  });
}

async function gif(render: RenderFrame, options: ExportOptions): Promise<Blob> {
  const frames = gifSchedule(options.drawing, options.speed);
  if (!options.gifWorkerURL || typeof Worker === 'undefined') throw new Error('GIF export is unavailable in this browser.');
  const url = new URL(options.gifWorkerURL, location.href);
  if (url.origin !== location.origin) throw new Error('The GIF worker must be hosted on this site.');
  const worker = new Worker(url, {type: 'module'});
  let rejectPending: ((reason: unknown) => void) | undefined, timeout: ReturnType<typeof setTimeout> | undefined;
  const abort = () => {worker.terminate(); rejectPending?.(canceled());};
  options.signal.addEventListener('abort', abort, {once: true});
  const send = (message: object, transfer: Transferable[] = []): Promise<ArrayBuffer | undefined> => new Promise((resolve, reject) => {
    rejectPending = reject;
    timeout = setTimeout(() => reject(new Error('GIF encoding timed out. Try a shorter scenario or video.')), 60_000);
    worker.onmessage = event => {clearTimeout(timeout); rejectPending = undefined; event.data.error ? reject(new Error(event.data.error)) : resolve(event.data.buffer);};
    worker.onerror = event => {clearTimeout(timeout); rejectPending = undefined; reject(new Error(event.message || 'GIF encoding failed.'));};
    worker.postMessage(message, transfer);
  });
  try {
    check(options.signal);
    await send({type: 'init', width: WIDTH, height: HEIGHT, maxBytes: EXPORT_LIMITS.gif.bytes});
    for (const [index, frame] of frames.entries()) {
      check(options.signal);
      const image = await render({time: frame.time, timestamp: 0, duration: frame.delay / 1000});
      const pixels = new Uint8Array(WIDTH * HEIGHT * 4);
      try {await image.copyTo(pixels, {format: 'RGBA'});} finally {image.close();}
      check(options.signal);
      // Exactly one RGBA frame in flight; palette work never blocks the UI thread.
      await send({type: 'frame', buffer: pixels.buffer, delay: frame.delay}, [pixels.buffer]);
      options.onProgress(.98 * (index + 1) / frames.length);
    }
    const buffer = await send({type: 'finish'});
    if (!buffer) throw new Error('GIF encoder produced no output.');
    return new Blob([buffer], {type: 'image/gif'});
  } finally {clearTimeout(timeout); options.signal.removeEventListener('abort', abort); worker.terminate();}
}

export type RenderFrame = (frame: ExportFrame) => Promise<VideoFrame>;

/** Browser SVG rasterization directly into WebCodecs; no Canvas or screen capture. */
function svgFrames(options: ExportOptions): RenderFrame {
  const root = svgElement('svg', {width: WIDTH, height: HEIGHT, viewBox: `0 0 ${WIDTH} ${HEIGHT}`});
  const drawing = svgElement('svg', {width: WIDTH, height: HEIGHT}) as SVGSVGElement;
  root.append(svgElement('rect', {width: WIDTH, height: HEIGHT, fill: '#000000'}), drawing);
  const renderer = new SvgDrawing(drawing, options.drawing), serializer = new XMLSerializer();
  return async frame => {
    check(options.signal);
    renderer.draw(frame.time, options.theme, options.dark);
    const url = URL.createObjectURL(new Blob([serializer.serializeToString(root)], {type: 'image/svg+xml'}));
    const image = new Image(WIDTH, HEIGHT);
    try {
      image.src = url;
      await abortable(image.decode(), options.signal);
      check(options.signal);
      return new VideoFrame(image, {timestamp: Math.round(frame.timestamp * 1_000_000),
        duration: Math.round(frame.duration * 1_000_000), alpha: 'discard'});
    } finally {image.removeAttribute('src'); URL.revokeObjectURL(url);}
  };
}

let active = false;
export async function exportScenario(options: ExportOptions): Promise<{blob: Blob; extension: string; mimeType: string}> {
  if (active) throw new Error('An export is already running. Finish or cancel it first.');
  if (options.format !== 'gif' && options.format !== 'video') throw new Error('Choose GIF or video.');
  if (typeof VideoFrame === 'undefined') throw new Error('Export requires WebCodecs VideoFrame. Use a current Chrome or Firefox, or download the scenario JSON.');
  exportDuration(options.drawing, options.speed); check(options.signal); active = true;
  // Bound stalled decoders/encoders as well as user cancellation.
  const controller = new AbortController(), abort = () => controller.abort();
  options.signal.addEventListener('abort', abort, {once: true});
  const timeout = setTimeout(abort, 600_000);
  const config = {...options, signal: controller.signal};
  try {
    config.onProgress(0);
    await abortable(document.fonts.ready, config.signal);
    const render = svgFrames(config);
    let blob: Blob;
    if (config.format === 'gif') blob = await gif(render, config);
    else {
      if (!config.videoModuleURL) throw new Error('Video export is unavailable.');
      const url = new URL(config.videoModuleURL, location.href);
      if (url.origin !== location.origin) throw new Error('The video encoder must be hosted on this site.');
      const support = await abortable(videoSupport(), config.signal);
      if (!support) throw new Error('No supported 1080p video encoder. Try GIF or download the scenario JSON.');
      const frames = videoSchedule(config.drawing, config.speed);
      const {encodeVideo} = await abortable(import(url.href), config.signal);
      blob = await encodeVideo(render, frames, support, config);
    }
    check(config.signal); options.onProgress(1);
    return {blob, extension: config.format === 'gif' ? 'gif' : blob.type.startsWith('video/mp4') ? 'mp4' : 'webm', mimeType: blob.type};
  } catch (error) {
    if (controller.signal.aborted && !options.signal.aborted) throw new Error('Export timed out after 10 minutes. Try a shorter scenario or increase speed.');
    throw error;
  } finally {clearTimeout(timeout); options.signal.removeEventListener('abort', abort); active = false;}
}
