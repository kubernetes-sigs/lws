// Real 1080p browser encoding, decoding, cancellation and lazy-loading checks.
import assert from 'node:assert/strict';
import {chromium, firefox} from '@playwright/test';
const base = process.env.LWS_SITE_URL || 'http://127.0.0.1:18446';
const engine = process.env.PLAYWRIGHT_BROWSER === 'firefox' ? firefox : chromium;
const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: process.env.PLAYWRIGHT_CHANNEL || 'chrome'} : {})});
const page = await browser.newPage({viewport:{width:1280,height:900},reducedMotion:'reduce'});
const failures = []; page.on('pageerror',error=>failures.push(String(error)));
const fixture = {version:1,width:1920,height:1080,title:'Export verification',primitives:[
  {type:'rect',x:0,y:0,width:1920,height:1080,fill:['#ffffff','#101720']},
  {type:'rect',x:100,y:100,width:500,height:500,fill:'#00aa00'},
  {type:'rect',x:100,y:100,width:500,height:500,fill:'#dd0000'},
  {type:'text',x:100,y:800,text:'Pending allowance: 1',size:48,fill:['#101720','#ffffff']},
],frames:[{duration:.4,caption:'Green',items:[0,1,3]},{duration:.4,caption:'Red',items:[0,2,3]}]};
try {
  await page.goto(`${base}/disaggregated-set-trace-visualizer/`);
  await page.waitForFunction(()=>!document.querySelector('[data-trace-export-menu]').disabled);
  assert.equal(await page.evaluate(()=>performance.getEntriesByType('resource').some(entry=>/trace-(export|gif-worker|video)\./.test(entry.name))),false);
  await page.locator('[data-trace-suite="import"]').click();
  await page.locator('[data-trace-file]').setInputFiles({name:'export-check.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify(fixture))});
  await page.waitForFunction(()=>!document.querySelector('[data-trace-export-menu]').disabled);
  const result=await page.evaluate(async drawing=>{
    const assets=JSON.parse(document.querySelector('#trace-export-assets').textContent);
    const {exportScenario,videoSupport}=await import(assets.module);
    const encode = async options => {
      const canvas = HTMLCanvasElement.prototype.getContext, offscreen = globalThis.OffscreenCanvas?.prototype.getContext;
      HTMLCanvasElement.prototype.getContext = () => {throw new Error('Export attempted Canvas rendering.');};
      if (globalThis.OffscreenCanvas) OffscreenCanvas.prototype.getContext = HTMLCanvasElement.prototype.getContext;
      try {return await exportScenario(options);}
      finally {HTMLCanvasElement.prototype.getContext = canvas; if(globalThis.OffscreenCanvas) OffscreenCanvas.prototype.getContext = offscreen;}
    };
    const progress=[];
    const options={drawing,speed:1,dark:true,signal:new AbortController().signal,onProgress:value=>progress.push(value),gifWorkerURL:assets.worker,videoModuleURL:assets.video};
    const gif=await encode({...options,format:'gif'});
    const image=await createImageBitmap(gif.blob),canvas=document.createElement('canvas');canvas.width=1920;canvas.height=1080;
    const ctx=canvas.getContext('2d');ctx.drawImage(image,0,0);
    const background=[...ctx.getImageData(20,20,1,1).data];image.close();
    const dimensions={width:canvas.width,height:canvas.height};
    let frameCount,finalPixel;
    if(typeof ImageDecoder !== 'undefined') {
    const decoder=new ImageDecoder({data:await gif.blob.arrayBuffer(),type:'image/gif'});
    await decoder.tracks.ready;
    frameCount=decoder.tracks.selectedTrack.frameCount;
    const final=await decoder.decode({frameIndex:frameCount-1});ctx.drawImage(final.image,0,0);
    finalPixel=[...ctx.getImageData(200,200,1,1).data];final.image.close();decoder.close();
    }
    const controller=new AbortController();
    let aborted='';
    try {await encode({...options,format:'gif',signal:controller.signal,onProgress:value=>{if(value>0)controller.abort();}});} catch(error){aborted=error.name;}
    let video;
    const support=await videoSupport();
    if(support){
      const encoded=await encode({...options,format:'video'});
      const url=URL.createObjectURL(encoded.blob),element=document.createElement('video');element.src=url;element.muted=true;
      try {
        await new Promise((resolve,reject)=>{element.onloadedmetadata=resolve;element.onerror=()=>reject(new Error('Video failed to decode.'));});
        element.currentTime=Math.max(0,element.duration-.05);
        await new Promise(resolve=>{element.onseeked=resolve;});ctx.drawImage(element,0,0);
        video={extension:encoded.extension,type:encoded.mimeType,bytes:encoded.blob.size,width:element.videoWidth,height:element.videoHeight,duration:element.duration,finalPixel:[...ctx.getImageData(200,200,1,1).data]};
      } finally {element.removeAttribute('src');element.load();URL.revokeObjectURL(url);}
      const abortVideo=new AbortController();let videoAbort='';
      try{await encode({...options,format:'video',signal:abortVideo.signal,onProgress:value=>{if(value>.01)abortVideo.abort();}});}catch(error){videoAbort=error.name;}
      video.cancellation=videoAbort;
    }
    return {gif:{extension:gif.extension,type:gif.mimeType,bytes:gif.blob.size,...dimensions,background,frameCount,finalPixel},progress,aborted,video};
  },fixture);
  assert.equal(result.gif.type,'image/gif');assert.equal(result.gif.extension,'gif');
  assert.deepEqual(result.gif.background,[16,23,32,255]);
  if(result.gif.frameCount !== undefined){assert.equal(result.gif.frameCount,2);assert.deepEqual(result.gif.finalPixel,[221,0,0,255]);}
  assert.equal(result.gif.width,1920);assert.equal(result.gif.height,1080);assert.ok(result.gif.bytes>100);
  assert.equal(result.progress[0],0);assert.equal(result.progress.at(-1),1);assert.equal(result.aborted,'AbortError');
  if(result.video){assert.equal(result.video.width,1920);assert.equal(result.video.height,1080);assert.ok(result.video.bytes>100);assert.ok(Math.abs(result.video.duration-.8)<.02);assert.ok(result.video.finalPixel[0]>180&&result.video.finalPixel[1]<30,JSON.stringify(result.video));assert.equal(result.video.cancellation,'AbortError');}

  // Exercise the real toolbar as well as the encoder API: a format/ring while exporting,
  // with full export details available to assistive technology and on hover.
  const status=page.locator('[data-trace-export-status]'),menu=page.locator('[data-trace-export-menu]');
  assert.equal(await status.isVisible(),false);
  await status.evaluate(node=>{
    node.testChanges=[];
    new MutationObserver(()=>{
      const progress=node.querySelector('[data-trace-export-progress]');
      node.testChanges.push({state:node.dataset.state,label:node.querySelector('[data-trace-export-label]').textContent,
        value:Number(progress.getAttribute('aria-valuenow')),offset:Number(node.querySelector('[data-trace-export-ring]').getAttribute('stroke-dashoffset')),
        visible:getComputedStyle(progress).visibility==='visible'});
    }).observe(node,{attributes:true,childList:true,subtree:true,characterData:true});
  });
  await page.locator('[data-animation-speed]').selectOption('2');
  for(const [format,extension] of [['gif','gif'],...(result.video?[['video',result.video.extension]]:[])]) {
    await status.evaluate(node=>{node.testChanges=[];});
    const downloaded=page.waitForEvent('download');
    await menu.click();
    await page.locator(`[data-trace-media="${format}"]`).click();
    const download=await downloaded,label=extension.toUpperCase();
    assert.ok(download.suggestedFilename().endsWith(`.${extension}`));
    await page.waitForFunction(()=>document.querySelector('[data-trace-export-status]').dataset.state==='done');
    const detail=await status.getAttribute('title');
    assert.match(detail,/1080p.*theme.*2×/);
    assert.equal(await status.getAttribute('aria-label'),detail,'Full export details remain accessible');
    assert.equal(await page.locator('[data-trace-export-message]').textContent(),detail,'Live announcement retains the full export details');
    assert.equal(await page.locator('[data-trace-export-label]').isVisible(),false,'Format label disappears after completion');
    assert.equal(await page.locator('[data-trace-export-progress]').isVisible(),false);
    assert.equal(await page.locator('[data-trace-export-done]').isVisible(),true,'Completion uses a check icon');
    await status.focus();assert.equal(await status.evaluate(node=>node===document.activeElement),true,'Export details are keyboard reachable');
    const changes=await status.evaluate(node=>node.testChanges);
    const progress=changes.filter(change=>change.state==='progress');
    assert.ok(progress.length>0&&progress.some(change=>change.value>0),`${label}: real progress updates are reported`);
    assert.ok(progress.every(change=>change.visible&&(change.label===label||(format==='video'&&change.value===0&&change.label==='MP4'))&&
      change.value>=0&&change.value<=100&&change.offset===100-change.value),
      `${label}: visible ring and accessible percentage stay synchronized without a sentence`);
    assert.equal(await page.locator('[data-trace-export-progress]').getAttribute('role'),'progressbar');
    assert.equal(await page.locator('[data-trace-export-ring]').getAttribute('pathLength'),'100');
    const geometry=await status.evaluate(node=>{
      const rect=target=>target.getBoundingClientRect().toJSON();
      return {status:rect(node),exports:rect(node.closest('.trace-player-exports')),header:rect(node.closest('.trace-player-subheader')),
        toggle:rect(document.querySelector('[data-trace-export-menu]'))};
    });
    for(const container of [geometry.exports,geometry.header]) assert.ok(geometry.status.x>=container.x-1&&geometry.status.right<=container.right+1&&
      geometry.status.y>=container.y-1&&geometry.status.bottom<=container.bottom+1,'Export badge stays within the shared subheader toolbar');
    assert.ok(geometry.status.width<=40&&geometry.status.height<=40,'Export indicator stays compact');
    assert.ok(Math.abs((geometry.status.y+geometry.status.bottom-geometry.toggle.y-geometry.toggle.bottom)/2)<1,'Export indicator stays beside Export to');
    assert.equal(await page.locator('[data-trace-cancel]').isVisible(),false);
    assert.equal(await page.locator(`[data-trace-media="${format}"]`).isEnabled(),true);
  }
  await menu.click();
  const canCancel=await page.evaluate(()=>{
    document.querySelector('[data-trace-media="gif"]').click();
    document.querySelector('[data-trace-export-menu]').click();
    const cancel=document.querySelector('[data-trace-cancel]'),visible=cancel.getClientRects().length>0;
    cancel.click();return visible;
  });
  assert.equal(canCancel,true,'Active export can be cancelled from its reopened menu');
  await page.waitForFunction(()=>document.querySelector('[data-trace-export-status]').dataset.state==='cancelled');
  assert.match(await status.getAttribute('title'),/cancelled/i);
  assert.equal(await status.getAttribute('aria-label'),await status.getAttribute('title'));
  assert.equal(await page.locator('[data-trace-export-cancelled]').isVisible(),true);
  assert.equal(await page.locator('[data-trace-cancel]').isVisible(),false);
  assert.deepEqual(failures,[]);
  console.log(JSON.stringify({gif:result.gif,video:result.video,cancellation:'passed',lazyLoading:'passed',canvasFree:'passed',toolbarFeedback:'passed'},null,2));
} finally {await browser.close();}
