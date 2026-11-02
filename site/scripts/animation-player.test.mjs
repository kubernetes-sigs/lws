import test from 'node:test';
import assert from 'node:assert/strict';
import {parseDrawing} from '../assets/js/animation-player.ts';

const example = () => ({version:1,width:1920,height:1080,title:'Example',
  primitives:[
    {type:'rect',x:0,y:0,width:1920,height:1080,fill:['#ffffff','#101720']},
    {type:'text',x:50,y:50,size:24,text:'Pending allowance: 1',fill:['#212121','#e8edf4']},
    {type:'path',points:[[0,0],[20,0],[20,20]],close:true,stroke:'#326ce5',lineWidth:2},
    {type:'circle',x:0,y:0,radius:5,fill:'#326ce5'},
  ],
  hits:[{x:0,y:0,width:100,height:100,label:'Recorded value',detail:'Plain text only.'}],
  frames:[{duration:2,caption:'An explanation.',items:[0,1,2],hits:[0]}],
  motions:[{primitive:3,keyframes:[[0,10,20],[2,30,40]]}],
});

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
