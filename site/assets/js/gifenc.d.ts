declare module 'gifenc' {
  type Pixels = Uint8Array | Uint8ClampedArray;
  type Palette = number[][];
  export function quantize(rgba: Pixels, colors: number, options?: {format?: 'rgb565'}): Palette;
  export function applyPalette(rgba: Pixels, palette: Palette, format?: 'rgb565'): Uint8Array;
  export function GIFEncoder(): {
    writeFrame(indexed: Uint8Array, width: number, height: number, options: {palette: Palette; delay: number; repeat: number}): void;
    finish(): void;
    bytes(): Uint8Array<ArrayBuffer>;
    bytesView(): Uint8Array<ArrayBuffer>;
  };
}
