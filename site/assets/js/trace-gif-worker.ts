/// <reference lib="webworker" />
import {GIFEncoder, quantize, applyPalette} from 'gifenc';
const scope = self as unknown as DedicatedWorkerGlobalScope;
let encoder: ReturnType<typeof GIFEncoder>, width = 0, height = 0, maxBytes = 0;
scope.onmessage = event => {
  const message = event.data;
  try {
    if (message.type === 'init') {
      ({width, height, maxBytes} = message); encoder = GIFEncoder();
    } else if (message.type === 'frame') {
      const rgba = new Uint8ClampedArray(message.buffer);
      if (!encoder || rgba.length !== width * height * 4) throw new Error('Invalid GIF frame.');
      const palette = quantize(rgba, 256, {format: 'rgb565'});
      encoder.writeFrame(applyPalette(rgba, palette, 'rgb565'), width, height, {palette, delay: message.delay, repeat: 0});
      if (encoder.bytesView().byteLength > maxBytes) throw new Error('GIF reached 64 MiB. Increase speed or choose video.');
    } else if (message.type === 'finish') {
      encoder.finish(); const bytes = encoder.bytes();
      scope.postMessage({buffer: bytes.buffer}, [bytes.buffer]); return;
    } else throw new Error('Unknown GIF operation.');
    scope.postMessage({});
  } catch (error) {scope.postMessage({error: error instanceof Error ? error.message : String(error)});}
};
