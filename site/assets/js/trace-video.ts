/** Loaded only for video export. WebCodecs encodes; Mediabunny muxes MP4/WebM. */
import {BufferTarget, Mp4OutputFormat, WebMOutputFormat, Output, VideoSample, VideoSampleSource} from 'mediabunny';
import type {ExportFrame, ExportOptions, RenderFrame, videoSupport} from './trace-export.ts';

export async function encodeVideo(render: RenderFrame, frames: ExportFrame[],
  support: NonNullable<Awaited<ReturnType<typeof videoSupport>>>, options: ExportOptions): Promise<Blob> {
  const target = new BufferTarget();
  const output = new Output({target, format: support.extension === 'mp4' ? new Mp4OutputFormat({fastStart: false}) : new WebMOutputFormat()});
  const source = new VideoSampleSource({codec: support.codec, fullCodecString: support.fullCodecString, bitrate: 5_000_000});
  output.addVideoTrack(source);
  let bytes = 0;
  target.on('write', ({end}) => {bytes = Math.max(bytes, end);});
  const abort = () => {void output.cancel().catch(() => {});};
  const check = () => {
    options.signal.throwIfAborted();
    if (bytes > 128 * 1024 * 1024) throw new Error('Video reached 128 MiB. Increase speed or use a shorter scenario.');
  };
  options.signal.addEventListener('abort', abort, {once: true});
  try {
    check(); await output.start();
    for (const [index, frame] of frames.entries()) {
      check();
      const pixels = await render(frame);
      let sample: VideoSample | undefined;
      try {sample = new VideoSample(pixels); await source.add(sample);} finally {sample?.close(); pixels.close();}
      check(); options.onProgress(.98 * (index + 1) / frames.length);
    }
    source.close(); await output.finalize(); check();
    if (!target.buffer?.byteLength) throw new Error('The video encoder produced no output.');
    return new Blob([target.buffer], {type: support.mimeType});
  } catch (error) {
    options.signal.throwIfAborted();
    throw error;
  } finally {
    options.signal.removeEventListener('abort', abort);
    if (output.state !== 'finalized') await output.cancel().catch(() => {});
  }
}
