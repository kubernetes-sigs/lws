import assert from 'node:assert/strict';
import {readFile, readdir} from 'node:fs/promises';
import test from 'node:test';
import {gunzipSync} from 'node:zlib';
import {parseDrawing} from '../assets/js/animation-player.ts';

test('prepared animations retain valid drawings without release, PR or capture-tool labels', async () => {
  for (const path of ['animations', 'trace-player/scenarios/small', 'trace-player/scenarios/large']) {
    const directory = new URL(`../static/${path}/`, import.meta.url);
    const files = (await readdir(directory)).filter(name => /\.json(\.gz)?$/.test(name));
    assert.ok(files.length > 0);
    for (const name of files) {
      const bytes = await readFile(new URL(name, directory));
      const drawing = parseDrawing(JSON.parse(name.endsWith('.gz') ? gunzipSync(bytes) : bytes));
      const notes = Array.isArray(drawing.provenance) ? drawing.provenance : drawing.provenance?.notes ?? [];
      const labels = [drawing.title, ...drawing.primitives.map(item => item.text),
        ...drawing.frames.map(frame => frame.caption),
        ...(drawing.hits ?? []).flatMap(hit => [hit.label, hit.detail]),
        ...notes].filter(Boolean);
      for (const label of labels) assert.doesNotMatch(label,
        /\bLWS\s+(?:v(?:ersion)?\s*)?\d|(?:^|\s)v\d+(?:\.\d+)*\b|\bversion\s+\d|\bPR\b|\bpull request\b|\bk9s\b/i, name);
    }
  }
});
