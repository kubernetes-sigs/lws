import test from 'node:test';
import assert from 'node:assert/strict';
import {gzipSync} from 'node:zlib';
import {AnimationPlayer, MAX_DRAWING_BYTES, parseDrawing, readDrawingJSON} from '../assets/js/animation-player.ts';

import {SvgDrawing} from '../assets/js/animation-svg.ts';

class Element extends EventTarget {
  dataset={};style={setProperty(){}};attributes=new Map();children=[];hidden=false;disabled=true;value='1';checked=true;
  setAttribute(name,value){this.attributes.set(name,value);}
  removeAttribute(name){this.attributes.delete(name);}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=nodes;}
  getBoundingClientRect(){return {height:40};}
}
function svgFixture(t,data) {
  const previous=globalThis.document;
  globalThis.document={createElementNS:()=>new Element()};
  t.after(()=>{if(previous)globalThis.document=previous;else delete globalThis.document;});
  const root=new Element();return {root,svg:new SvgDrawing(root,data)};
}
const background = node => node.children[0].children[0].attributes.get('fill');

const example = () => ({version:1,width:1920,height:1080,title:'Example',
  primitives:[
    {type:'rect',x:0,y:0,width:1920,height:1080,fill:['#ffffff','#101720']},
    {type:'text',x:50,y:50,size:24,text:'Pending allowance: 1',fill:['#212121','#e8edf4']},
    {type:'path',points:[[0,0],[20,0],[20,20]],close:true,stroke:'#326ce5',lineWidth:2},
    {type:'circle',x:0,y:0,radius:5,fill:'#326ce5'},
  ],
  hits:[{x:0,y:0,width:100,height:100,label:'Recorded value',detail:'Plain text only.'}],
  frames:[{duration:2,caption:'An explanation.',items:[0,1,2],
    hits:[0]}],
  motions:[{primitive:3,keyframes:[[0,10,20],[2,30,40]]}],
});
const palettes=['#ffffff','#111111','#eeeeee','#222222','#dddddd','#333333'];
const themedExample = () => {
  const data=example();data.themes=['lws','lws-dark','mistral-light','mistral-dark','llm-d-light','llm-d-dark'];
  for(const primitive of data.primitives) if(Array.isArray(primitive.fill)) primitive.fill=[...palettes];
  return data;
};

test('accepts the finite drawing vocabulary and preserves resolved data', () => {
  const data=example();
  assert.equal(parseDrawing(data),data);
  data.primitives[1].text='<script>alert(1)</script>';
  assert.equal(parseDrawing(data).primitives[1].text,data.primitives[1].text);
});

for (const [name,edit] of [
  ['unknown primitive',d=>{d.primitives[0].type='execute';}],
  ['external paint URL',d=>{d.primitives[0].fill='url(https://example.test/image)';}],
  ['nonfinite position',d=>{d.primitives[0].x=Infinity;}],
  ['negative dimensions',d=>{d.primitives[0].width=-1;}],
  ['oversized canvas',d=>{d.width=100000;}],
  ['empty frames',d=>{d.frames=[];}],
  ['bad primitive reference',d=>{d.frames[0].items=[50];}],
  ['fractional primitive reference',d=>{d.frames[0].items=[1.5];}],
  ['invalid frame duration',d=>{d.frames[0].duration=0;}],
  ['invalid text font',d=>{d.primitives[1].font='url(example)';}],
  ['invalid text size',d=>{d.primitives[1].size=NaN;}],
  ['malformed path',d=>{d.primitives[2].points=[[1,2,3]];}],
  ['invalid opacity',d=>{d.primitives[0].opacity=2;}],
  ['backward motion time',d=>{d.motions[0].keyframes=[[1,0,0],[0,10,10]];}],
  ['duplicate motion time',d=>{d.motions[0].keyframes=[[0,0,0],[0,10,10]];}],
  ['motion outside presentation',d=>{d.motions[0].keyframes[1][0]=3;}],
  ['invalid hit bounds',d=>{d.hits[0].width=-1;}],
]) test(`rejects ${name}`,()=>{
  const data=example();edit(data);
  assert.throws(()=>parseDrawing(data),/Unsupported animation drawing/);
});

test('rejects missing or unsupported envelopes',()=>{
  for(const value of [null,[],{}, {...example(),version:2}]) assert.throws(()=>parseDrawing(value));
});

test('accepts named palettes while retaining constant semantic colors',()=>{
  const data=themedExample();assert.equal(parseDrawing(data),data);
  assert.equal(data.primitives[2].stroke,'#326ce5');
});

for(const [name,edit] of [
  ['empty themes',d=>{d.themes=[];}],
  ['too many themes',d=>{d.themes=Array.from({length:9},(_,i)=>`theme-${i}`);}],
  ['duplicate themes',d=>{d.themes[1]='lws';}],
  ['non-string theme',d=>{d.themes[0]=1;}],
  ['unsafe theme name',d=>{d.themes[0]='url(example)';}],
  ['long theme name',d=>{d.themes[0]='a'.repeat(65);}],
  ['palette shorter than theme list',d=>{d.primitives[0].fill.pop();}],
  ['palette longer than theme list',d=>{d.primitives[0].fill.push('#abcdef');}],
  ['invalid palette color',d=>{d.primitives[0].fill[4]='red';}],
  ['multi-palette without names',d=>{delete d.themes;}],
]) test(`rejects ${name}`,()=>{const data=themedExample();edit(data);assert.throws(()=>parseDrawing(data));});

test('shared SVG resolves named palettes and website light/dark fallbacks',t=>{
  const data=parseDrawing(themedExample()), {root,svg}=svgFixture(t,data);
  for(const [theme,dark,expected] of [
    ...data.themes.map((theme,i)=>[theme,false,palettes[i]]),
    [undefined,false,palettes[0]],[undefined,true,palettes[1]],['unknown',true,palettes[1]],
  ]) {
    svg.draw(0,theme,dark);assert.equal(background(root),expected);
  }
  const legacy=new SvgDrawing(root,parseDrawing(example()));
  for(const [theme,dark,expected] of [['mistral-light',true,'#101720'],['lws-dark',false,'#101720'],['lws',true,'#ffffff']]) {
    legacy.draw(0,theme,dark);assert.equal(background(root),expected);
  }
});

test('shared SVG retains frame boundaries, motion interpolation and backward seeking',t=>{
  const data=example();data.frames.push({duration:2,caption:'Last frame',items:[0]});
  const {root,svg}=svgFixture(t,parseDrawing(data));
  svg.draw(1);
  assert.equal(root.children[2].children[0].attributes.get('transform'),'translate(20 30)');
  assert.equal(root.children[1].children.length,3);
  svg.draw(3);
  assert.equal(root.children[1].children.length,1);
  assert.equal(root.children[2].children[0].attributes.get('display'),'none');
  svg.draw(1);
  assert.equal(root.children[1].children.length,3);
  assert.equal(root.children[2].children[0].attributes.has('display'),false);
});

test('reads plain or gzip prepared JSON, independent of filenames and headers', async () => {
  const json=JSON.stringify(example()), gzip=gzipSync(json);
  assert.deepEqual(await readDrawingJSON(new Blob([json]).stream()),example());
  // The gzip signature can cross stream chunks; hosts may also decode gzip already.
  const stream=new ReadableStream({start(controller){
    controller.enqueue(gzip.subarray(0,1));controller.enqueue(gzip.subarray(1));controller.close();
  }});
  assert.deepEqual(await readDrawingJSON(stream),example());
});

test('rejects oversized transport and cancels its stream', async () => {
  let cancelled=false;
  const stream=new ReadableStream({
    start(controller){controller.enqueue(new Uint8Array(MAX_DRAWING_BYTES+1));},
    cancel(){cancelled=true;},
  });
  await assert.rejects(readDrawingJSON(stream),/16 MiB/);
  assert.equal(cancelled,true);
});

test('bounds decoded gzip independently of compressed size', async () => {
  const compressed=gzipSync(Buffer.alloc(MAX_DRAWING_BYTES+1,32));
  assert.ok(compressed.byteLength<MAX_DRAWING_BYTES);
  await assert.rejects(readDrawingJSON(new Blob([compressed]).stream()),/16 MiB/);
});

test('rejects empty, malformed JSON, and broken gzip', async () => {
  await assert.rejects(readDrawingJSON(null),/empty/);
  await assert.rejects(readDrawingJSON(new Blob(['not json']).stream()),/valid JSON/);
  await assert.rejects(readDrawingJSON(new Blob([new Uint8Array([0x1f,0x8b,0])]).stream()));
});

test('cancels an in-flight local file read when a newer load starts', async () => {
  let cancelled=false;
  const stream=new ReadableStream({cancel(){cancelled=true;}}), abort=new AbortController();
  const pending=readDrawingJSON(stream,abort.signal);
  abort.abort();
  await assert.rejects(pending,{name:'AbortError'});assert.equal(cancelled,true);
});

const event = (root,name) => new Promise(resolve=>root.addEventListener(name,e=>resolve(e.detail),{once:true}));
const load = async (root,detail,type='animation-ready') => {
  const result=event(root,type);root.dispatchEvent(new CustomEvent('animation-load',{detail}));return result;
};

// Minimal native DOM stand-ins keep loader tests independent of a browser dependency.
async function playerFixture(t) {
  const names=['svg','seek','play','speed','loop','fullscreen','tooltip','previous','next','player','controls','error','clock','summary'];
  const nodes=Object.fromEntries(names.map(name=>[name,new Element()]));
  const root=new Element();root.dataset.animationSource='/initial.json';
  root.querySelector=selector=>nodes[selector.match(/data-animation-([^\]]+)/)[1]];
  root.querySelectorAll=()=>Object.values(nodes);
  const document=new EventTarget();document.createElementNS=()=>new Element();document.documentElement={dataset:{}};document.hidden=false;
  let observerCount=0,raf=0,redrawTheme;
  const globals={
    document,fetch:async()=>new Response(JSON.stringify(example())),
    matchMedia:()=>({matches:true,addEventListener(){}}),
    requestAnimationFrame:()=>++raf,cancelAnimationFrame:()=>{},
    ResizeObserver:class{constructor(){observerCount++;}observe(){}},
    MutationObserver:class{constructor(callback){observerCount++;this.callback=callback;}observe(target){if(target===root)redrawTheme=this.callback;}},
    IntersectionObserver:class{constructor(callback){observerCount++;this.callback=callback;}observe(){this.callback([{isIntersecting:true}]);}},
  };
  const previous=Object.fromEntries(Object.keys(globals).map(key=>[key,Object.getOwnPropertyDescriptor(globalThis,key)]));
  for(const [key,value] of Object.entries(globals)) Object.defineProperty(globalThis,key,{value,writable:true,configurable:true});
  t.after(()=>{for(const [key,descriptor] of Object.entries(previous)){
    if(descriptor) Object.defineProperty(globalThis,key,descriptor);else delete globalThis[key];
  }});
  const ready=event(root,'animation-ready');new AnimationPlayer(root);await ready;
  return {root,nodes,redrawTheme,observers:()=>observerCount};
}

test('replacement resets timeline and motion state without adding observers or losing preferences', async t => {
  const {root,nodes,observers}=await playerFixture(t);
  nodes.speed.value='2';nodes.loop.checked=false;
  nodes.seek.value='1';nodes.seek.dispatchEvent(new Event('input'));
  nodes.tooltip.hidden=false;
  const data=example();data.title='Replacement';delete data.motions;
  data.frames=[{duration:3,caption:'First replacement frame.',items:[0,1]},{duration:4,caption:'Second.',items:[0]}];
  const result=await load(root,{drawing:data,requestId:'replacement'});
  assert.equal(result.requestId,'replacement');assert.equal(result.drawing,data);
  assert.equal(nodes.seek.max,'7');assert.equal(nodes.seek.value,'0');
  assert.equal(nodes.summary.textContent,'First replacement frame.');
  assert.equal(nodes.svg.attributes.get('aria-label'),'Replacement');
  assert.equal(nodes.tooltip.hidden,true);assert.equal(nodes.play.textContent,'Play');
  assert.equal(nodes.speed.value,'2');assert.equal(nodes.loop.checked,false);assert.equal(observers(),3);
  nodes.next.dispatchEvent(new Event('click'));assert.equal(nodes.seek.value,'3');
  // Explicit play intent, unlike the initial reduced-motion preference, survives source changes.
  nodes.play.dispatchEvent(new Event('click'));
  await load(root,{drawing:example()});assert.equal(nodes.play.textContent,'Pause');
});

test('changing the figure theme redraws without resetting time, playback, or speed',async t=>{
  const {root,nodes,calls,redrawTheme,observers}=await playerFixture(t);
  await load(root,{drawing:themedExample()});
  nodes.seek.value='1';nodes.seek.dispatchEvent(new Event('input'));nodes.speed.value='2';
  root.dataset.animationTheme='mistral-dark';redrawTheme();
  assert.equal(background(nodes.svg),palettes[3]);
  assert.equal(nodes.seek.value,'1');assert.equal(nodes.play.textContent,'Play');assert.equal(nodes.speed.value,'2');
  nodes.play.dispatchEvent(new Event('click'));delete root.dataset.animationTheme;redrawTheme();
  assert.equal(background(nodes.svg),palettes[0]);
  assert.equal(nodes.seek.value,'1');assert.equal(nodes.play.textContent,'Pause');assert.equal(observers(),3);
});

test('bad imports preserve the last-good frame, and a later valid import clears the error', async t => {
  const {root,nodes}=await playerFixture(t);
  nodes.seek.value='1';nodes.seek.dispatchEvent(new Event('input'));
  const failed=await load(root,{drawing:{version:99},requestId:'invalid'},'animation-error');
  assert.equal(failed.requestId,'invalid');assert.match(failed.message,/Unsupported/);
  assert.equal(nodes.seek.value,'1');assert.equal(nodes.seek.max,'2');
  assert.equal(nodes.summary.textContent,'An explanation.');assert.equal(nodes.error.hidden,false);
  const result=await load(root,{file:new File([gzipSync(JSON.stringify(example()))],'local.json.gz')});
  assert.equal(result.drawing.title,'Example');assert.equal(nodes.error.hidden,true);assert.equal(nodes.seek.value,'0');
});

test('a stale remote result cannot replace a newer drawing and its request is aborted', async t => {
  const {root,nodes}=await playerFixture(t);
  let resolve,signal;
  globalThis.fetch=(_url,options)=>{signal=options.signal;return new Promise(done=>{resolve=done;});};
  root.dispatchEvent(new CustomEvent('animation-load',{detail:{source:'/slow.json',requestId:'slow'}}));
  let readyCount=0;root.addEventListener('animation-ready',()=>readyCount++);
  const latest=example();latest.title='Newest';await load(root,{drawing:latest});
  assert.equal(signal.aborted,true);
  resolve(new Response(JSON.stringify(example())));
  await new Promise(setImmediate);
  assert.equal(readyCount,1);assert.equal(nodes.svg.attributes.get('aria-label'),'Newest');
});

test('failed remote loads keep the old drawing, including oversized declared responses', async t => {
  const {root,nodes}=await playerFixture(t);
  globalThis.fetch=async()=>new Response('{}',{headers:{'content-length':String(MAX_DRAWING_BYTES+1)}});
  const error=await load(root,{source:'/too-large.json'},'animation-error');
  assert.match(error.message,/16 MiB/);assert.equal(nodes.svg.attributes.get('aria-label'),'Example');
});
