import assert from 'node:assert/strict';
import {readFile, readdir} from 'node:fs/promises';
import test from 'node:test';
import {parseDrawing} from '../assets/js/animation-player.ts';
import {compactDrawing} from './compact-animation-hits.mjs';

const hit = () => ({x: 10, y: 20, width: 30, height: 40, label: 'Group', detail: 'Observed Ready'});
const drawing = () => ({version: 1, width: 1920, height: 1080, title: 'Example', primitives: [],
  hits: [hit()], frames: [{duration: 1, caption: 'First', items: [], hits: [0, 0]},
    {duration: 1, caption: 'Second', items: [], hits: [0]}]});

test('legacy inline hits are normalized without mutating the imported drawing', () => {
  const data = drawing(); delete data.hits;
  for (const frame of data.frames) frame.hits = frame.hits.map(() => hit());
  const before = structuredClone(data), parsed = parseDrawing(data);
  assert.deepEqual(data, before);
  assert.deepEqual(parsed.hits, [hit()]);
  assert.deepEqual(parsed.frames.map(frame => frame.hits), [[0, 0], [0]]);
});

for (const [name, change] of [
  ['negative index', d => {d.frames[0].hits = [-1];}],
  ['fractional index', d => {d.frames[0].hits = [.5];}],
  ['out-of-range index', d => {d.frames[0].hits = [1];}],
  ['nonfinite index', d => {d.frames[0].hits = [Infinity];}],
  ['string index', d => {d.frames[0].hits = ['0'];}],
  ['sparse references', d => {d.frames[0].hits = Array(1);}],
  ['missing dictionary', d => {delete d.hits;}],
  ['empty dictionary', d => {d.hits = [];}],
  ['sparse dictionary', d => {d.hits = Array(1);}],
  ['mixed inline and indexed hits', d => {d.frames[0].hits = [hit()];}],
  ['invalid table bounds', d => {d.hits[0].height = -1;}],
  ['nonfinite table bounds', d => {d.hits[0].x = NaN;}],
  ['invalid table label', d => {d.hits[0].label = null;}],
  ['oversized table', d => {d.hits = Array(100_001).fill(hit());}],
  ['oversized reference list', d => {d.frames[0].hits = Array(10_001).fill(0);}],
  ['excessive total references', d => {d.frames = Array.from({length: 201}, () => ({duration: 1, caption: '', items: [], hits: Array(10_000).fill(0)}));}],
]) test(`rejects ${name}`, () => {
  const data = drawing(); change(data);
  assert.throws(() => parseDrawing(data), /Unsupported animation drawing/);
});

test('compaction is deterministic, idempotent, and changes only hit storage', () => {
  const data = drawing();
  data.hits.push({...hit()}, {...hit(), label: 'Later hit'});
  data.frames[1].hits = [1, 2, 0];
  const compact = compactDrawing(data);
  assert.deepEqual(compact.hits, [hit(), {...hit(), label: 'Later hit'}]);
  assert.deepEqual(compact.frames[1].hits, [0, 1, 0]);
  assert.deepEqual(compactDrawing(compact), compact);
  const resolved = ({hits, frames, ...rest}) => ({...rest,
    frames: frames.map(frame => ({...frame, hits: frame.hits.map(index => hits[index])}))});
  assert.deepEqual(resolved(compact), resolved(data));
});

test('every packaged animation uses unique hit definitions and bounded integer references', async () => {
  const directory = new URL('../static/animations/', import.meta.url);
  for (const name of (await readdir(directory)).filter(name => name.endsWith('.json'))) {
    const drawing = JSON.parse(await readFile(new URL(name, directory), 'utf8'));
    assert.deepEqual(parseDrawing(structuredClone(drawing)), drawing, `${name}: packaged data is preserved`);
    assert.ok(Array.isArray(drawing.hits), `${name}: top-level hit table`);
    const keys = drawing.hits.map(h => JSON.stringify([h.x, h.y, h.width, h.height, h.label, h.detail]));
    assert.equal(new Set(keys).size, keys.length, `${name}: no repeated hit definitions`);
  }
});
