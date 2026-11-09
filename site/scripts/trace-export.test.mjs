import test from 'node:test';
import assert from 'node:assert/strict';
import {exportDuration, gifSchedule, videoSupport, videoSchedule} from '../assets/js/trace-export.ts';
const drawing = durations => ({width:1920,height:1080,frames:durations.map((duration,i)=>({duration,items:[i]}))});

test('static GIF preserves long holds without repeated frames, including normal-speed harness', () => {
  const data=drawing([10,200,17]);
  assert.deepEqual(gifSchedule(data,1),[{time:0,delay:10000},{time:10,delay:200000},{time:210,delay:17000}]);
  assert.equal(exportDuration(data,2),113.5);
});
test('GIF coalesces only identical adjacent visuals and preserves changed states', () => {
  const data=drawing([1,2,3]); data.frames[1].items=[0];
  assert.deepEqual(gifSchedule(data,1),[{time:0,delay:3000},{time:3,delay:3000}]);
});
test('motion sampling includes every state boundary and exact centisecond total', () => {
  const data=drawing([.13,.31,1]);data.motions=[{}];
  const frames=gifSchedule(data,2);
  assert.ok(frames.some(frame=>frame.time===.13));assert.ok(frames.some(frame=>frame.time===.44));
  assert.equal(frames.reduce((sum,frame)=>sum+frame.delay,0),720);
});
test('rejects excessive duration, frame count, invalid speed, or unrepresentable holds', () => {
  assert.throws(()=>gifSchedule(drawing([601]),1),/10 minutes/);
  assert.throws(()=>gifSchedule(drawing(Array(1201).fill(.1)),1),/1,200/);
  assert.throws(()=>gifSchedule({...drawing([121]),motions:[{}]},1),/1,200/);
  assert.throws(()=>gifSchedule(drawing([.001]),1),/minimum/);
  assert.throws(()=>gifSchedule(drawing([.01]),1),/minimum/);
  for(const speed of [NaN,0,-1,17]) assert.throws(()=>exportDuration(drawing([1]),speed),/speed/);
});
test('video preserves sub-frame states and holds without real-time sampling', () => {
  const frames=videoSchedule(drawing([.11,.01,.09]),1);
  assert.deepEqual(frames.map(f=>f.timestamp),[0,.11,.12]);
  assert.ok(Math.abs(frames.at(-1).timestamp+frames.at(-1).duration-.21)<1e-12);
  assert.equal(videoSchedule(drawing([200]),1).length,1);
  assert.throws(()=>videoSchedule(drawing([.001]),2),/millisecond/);
  assert.throws(()=>videoSchedule(drawing([601]),1),/10 minutes/);
  const moving=videoSchedule({...drawing([.13,.31]),motions:[{}]},1);
  assert.ok(moving.some(f=>f.time===.13));
  assert.ok(Math.abs(moving.at(-1).timestamp+moving.at(-1).duration-.44)<1e-12);
});
test('WebCodecs reports MP4, accurately labeled WebM fallback, or unavailable', async () => {
  const previous={VideoEncoder:globalThis.VideoEncoder,VideoFrame:globalThis.VideoFrame};
  try {
    globalThis.VideoFrame=class{};
    globalThis.VideoEncoder={isConfigSupported:async({codec})=>({supported:codec.startsWith('avc')})};
    assert.equal((await videoSupport()).extension,'mp4');
    globalThis.VideoEncoder={isConfigSupported:async({codec})=>({supported:codec.startsWith('vp09')})};
    assert.equal((await videoSupport()).extension,'webm');
    globalThis.VideoEncoder={isConfigSupported:async()=>({supported:false})};assert.equal(await videoSupport(),null);
    delete globalThis.VideoFrame;assert.equal(await videoSupport(),null);
  } finally {
    for(const [key,value] of Object.entries(previous)) if(value===undefined) delete globalThis[key];else globalThis[key]=value;
  }
});
