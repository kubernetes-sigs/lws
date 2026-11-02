// One-time, deterministic migration of prepared drawings. No rendering is needed.
import {readFile, writeFile} from 'node:fs/promises';
import {pathToFileURL} from 'node:url';
import {parseDrawing} from '../assets/js/animation-player.ts';

export function compactDrawing(value) {
  const drawing = parseDrawing(value), hits = [], byValue = new Map();
  const frames = drawing.frames.map(frame => ({...frame, ...(frame.hits ? {hits: frame.hits.map(index => {
    const hit = drawing.hits[index];
    const key = JSON.stringify([hit.x, hit.y, hit.width, hit.height, hit.label, hit.detail]);
    if (!byValue.has(key)) {byValue.set(key, hits.length); hits.push(hit);}
    return byValue.get(key);
  })} : {})}));
  return {...drawing, hits, frames};
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const paths = process.argv.slice(2);
  if (!paths.length) throw new Error('Usage: node scripts/compact-animation-hits.mjs <drawing.json> [...]');
  for (const path of paths) {
    const before = await readFile(path, 'utf8');
    const after = `${JSON.stringify(compactDrawing(JSON.parse(before)))}\n`;
    await writeFile(path, after);
    console.log(`${path}: ${Buffer.byteLength(before)} → ${Buffer.byteLength(after)} bytes`);
  }
}
