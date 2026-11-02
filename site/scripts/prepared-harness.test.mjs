import assert from 'node:assert/strict';
import {readFile, readdir} from 'node:fs/promises';
import test from 'node:test';
import {gunzipSync} from 'node:zlib';
import {parseDrawing} from '../assets/js/animation-player.ts';

test('prepared harness scenarios retain valid drawings without release or PR labels', async () => {
  for (const suite of ['small', 'large']) {
    const directory = new URL(`../static/trace-player/scenarios/${suite}/`, import.meta.url);
    const files = (await readdir(directory)).filter(name => name.endsWith('.json.gz'));
    assert.ok(files.length > 0);
    for (const name of files) {
      const drawing = parseDrawing(JSON.parse(gunzipSync(await readFile(new URL(name, directory)))));
      const labels = [drawing.title, ...drawing.primitives.map(item => item.text),
        ...drawing.frames.flatMap(frame => [frame.caption,
          ...(frame.hits ?? []).flatMap(index => [drawing.hits[index].label, drawing.hits[index].detail])])].filter(Boolean);
      for (const label of labels) assert.doesNotMatch(label,
        /\bLWS\s+(?:v(?:ersion)?\s*)?\d|\bPR\b|\bpull request\b/i, name);
    }
  }
});
