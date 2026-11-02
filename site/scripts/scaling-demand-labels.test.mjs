import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import test from 'node:test';

for (const scene of ['scaling','scaling-up']) {
  test(`${scene} shows the since-demand delta only after the demand update`,async()=>{
    const drawing=JSON.parse(await readFile(new URL(`../static/animations/${scene}.json`,import.meta.url),'utf8'));
    let before=0,afterWithDelta=0;
    for (const frame of drawing.frames) {
      const labels=frame.items.map(index=>drawing.primitives[index]).filter(item=>item.type==='text').map(item=>item.text);
      if (labels.some(label=>label.startsWith('Initial desired:'))) {
        before++;
        assert.ok(labels.some(label=>/^\d+ requested groups$/.test(label)));
        assert.ok(!labels.some(label=>label.includes('since demand')),'A pre-demand frame must not claim a since-demand change');
      } else if (labels.some(label=>label.startsWith('Replica request:'))&&labels.some(label=>label.endsWith('since demand'))) {
        afterWithDelta++;
      }
    }
    assert.ok(before>0,'Exercise the recorded pre-demand phase');
    assert.ok(afterWithDelta>0,'Keep the useful delta after the demand update');
  });
}
