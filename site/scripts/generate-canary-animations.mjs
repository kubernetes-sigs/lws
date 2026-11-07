// Finite teaching examples for the existing docs player. No cluster timings or
// planner simulation: the states below illustrate safe transitions and holds.
// Run from any directory: node site/scripts/generate-canary-animations.mjs
import assert from 'node:assert/strict';
import {writeFile} from 'node:fs/promises';
import {compactDrawing} from './compact-animation-hits.mjs';

// The same six palettes and revision colors as the established docs scenes.
const themes = ['lws', 'lws-dark', 'mistral-light', 'mistral-dark', 'llm-d-light', 'llm-d-dark'];
const bg = ['#ffffff', '#101720', '#fbfbf8', '#101013', '#ffffff', '#16121a'];
const ink = ['#212121', '#e8edf4', '#171722', '#fafaf4', '#121619', '#e3e3e3'];
const muted = ['#666666', '#aab4c3', '#55555e', '#a1a1aa', '#4d4d4d', '#c1c7cd'];
const surface = ['#f3f6fa', '#202b3a', '#ebe9e0', '#27272b', '#f8edf8', '#24212a'];
const border = ['#bdc1c6', '#45546a', '#c9c9c4', '#474754', '#dde1e6', '#606770'];
const safe = ['#176b35', '#69db8c', '#176b35', '#69db8c', '#176b35', '#69db8c'];
const fills = [
  ['#326ce5', '#326ce5', '#ffaf01', '#ffaf01', '#7f317f', '#ca77ca'],
  ['#34a853', '#34a853', '#ff8204', '#ff8204', '#e783a7', '#e783a7'],
  ['#999999', '#999999', '#c4001d', '#e61300', '#ca77ca', '#e7c4e7'],
];
const labels = [
  ['#2858ba', '#8bb0ff', '#7b5401', '#ffaf01', '#7f317f', '#ca77ca'],
  ['#24783a', '#77d98b', '#933800', '#ff8204', '#7f317f', '#e783a7'],
  ['#666666', '#c0cad8', '#c4001d', '#ff6660', '#a044a0', '#e7c4e7'],
];
const roles = ['Prefill', 'Decode'];

class Scene {
  constructor(title, replacement = false) {
    this.replacement = replacement;
    this.drawing = {version: 1, width: 1920, height: 1080, title, themes,
      provenance: ['Illustrative group changes, not a recorded Kubernetes run.',
        'maxSurge 1 and maxUnavailable 0 per logical role. Starting groups do not provide Ready capacity.',
        'The controller may combine changes or use a different safe deletion order.'],
      source: {generator: 'site/scripts/generate-canary-animations.mjs'},
      primitives: [], frames: []};
    this.indices = new Map();
  }
  add(p) {
    const key = JSON.stringify(p);
    if (!this.indices.has(key)) {this.indices.set(key, this.drawing.primitives.length); this.drawing.primitives.push(p);}
    this.items.push(this.indices.get(key));
  }
  text(x, y, text, size = 28, fill = ink, bold = false, align = 'left', mono = false) {
    this.add({type: 'text', x, y, text, size, fill, bold, align, font: mono ? 'mono' : 'sans'});
  }
  rect(x, y, width, height, fill, radius = 0, stroke) {
    this.add({type: 'rect', x, y, width, height, fill, radius, ...(stroke ? {stroke, lineWidth: 2} : {})});
  }
  line(x1, y1, x2, y2, stroke, lineWidth = 2) {
    this.add({type: 'path', points: [[x1, y1], [x2, y2]], stroke, lineWidth});
  }
  hit(x, y, width, height, label, detail) {this.hits.push({x, y, width, height, label, detail});}
  frame({percent, counts, ready = counts, desired = [4, 8], seconds = .4, cue, action, zero = false}) {
    this.items = []; this.hits = [];
    const target = this.replacement ? 2 : 1;
    const quota = desired.map(d => percent === 100 ? d : percent === 0 ? 0 : Math.min(d - 1, Math.ceil(d * percent / 100)));
    const total = roles.map((_, i) => counts.reduce((sum, r) => sum + r[i], 0));
    const serving = roles.map((_, i) => ready.reduce((sum, r) => sum + (r.every(n => n > 0) ? r[i] : 0), 0));
    this.rect(0, 0, 1920, 1080, bg);
    this.text(960, 46, this.drawing.title, 38, ink, true, 'center');
    this.text(960, 94, 'maxSurge = 1 · maxUnavailable = 0 per role · Each square is one group', 25, muted, false, 'center');
    this.rect(48, 124, 1824, 66, surface, 10);
    this.text(74, 157, `canaryPercent: ${percent}`, 31, ink, true, 'left', true);
    this.text(1845, 157, `Desired: ${desired[0]} Prefill + ${desired[1]} Decode`, 30, ink, false, 'right');
    const starts = this.replacement ? [365, 865, 1365] : [380, 1170];
    const names = this.replacement ? ['A · baseline stays fixed', 'B · previous canary', 'C · new canary'] : ['A · baseline', 'B · canary'];
    starts.forEach((x, revision) => this.text(x, 228, names[revision], 30, labels[revision], true));
    roles.forEach((role, i) => {
      const y = 275 + i * 112;
      this.text(60, y + 19, role, 32, ink, true);
      starts.forEach((x, revision) => {
        for (let n = 0; n < counts[revision][i]; n++) {
          const readyGroup = n < ready[revision][i], left = x + n * 60;
          this.rect(left, y, 48, 48, readyGroup ? fills[revision] : bg, 4, labels[revision]);
          if (!readyGroup) this.add({type: 'circle', x: left + 24, y: y + 24, radius: 2, fill: labels[revision]});
          this.hit(left, y, 48, 48, `${String.fromCharCode(65 + revision)} ${role} group ${n + 1}`,
            this.replacement && revision === 0 ? 'Same baseline group throughout B → C; its workload and Pod identities stay unchanged.' :
              readyGroup ? 'Ready. It contributes serving capacity only while every required role of this revision is Ready.' : 'Starting: requested, but not Ready capacity.');
        }
        const pending = counts[revision][i] - ready[revision][i];
        const count = counts[revision][i];
        const label = zero && revision === target && !count ? 'LWS retained · replicas: 0' : `${count} ${count === 1 ? 'group' : 'groups'} · ${ready[revision][i]} Ready${pending ? ` · ${pending} starting` : ''}`;
        this.text(x, y + 74, label, 24, muted);
      });
    });
    this.text(60, 493, action, 26, ink);
    this.rect(1500, 481, 18, 18, ink, 3);
    this.text(1530, 491, 'Ready', 22, muted);
    this.rect(1660, 481, 18, 18, bg, 3, muted);
    this.text(1690, 491, 'Starting', 22, muted);

    this.rect(48, 526, 1824, 180, surface, 10, border);
    this.text(72, 607, 'Serving Ready / floor', 28, muted);
    this.text(72, 668, 'Requested / ceiling', 28, muted);
    roles.forEach((role, i) => {
      const x = 960 + i * 575;
      this.text(x, 558, role, 28, ink, true, 'center');
      this.text(x, 607, `${serving[i]} / ${desired[i]}`, 38, serving[i] >= desired[i] ? safe : muted, true, 'center');
      this.text(x, 668, `${total[i]} / ${desired[i] + 1}`, 34, total[i] <= desired[i] + 1 ? safe : muted, false, 'center');
      this.hit(x - 160, 579, 320, 55, `${role}: availability`,
        `${serving[i]} serving Ready groups; current desired floor ${desired[i]} because maxUnavailable is zero. A scale-up can raise this floor before new groups start; that gap does not authorize deleting Ready capacity.`);
      this.hit(x - 160, 640, 320, 55, `${role}: surge`,
        `${total[i]} groups requested; current ceiling ${desired[i] + 1}. Existing excess after a demand reduction drains safely; the new ceiling does not require instant deletion.`);
    });
    this.text(60, 760, `${String.fromCharCode(65 + target)} · Ready groups / desired`, 29, ink, true);
    this.text(1845, 760, percent === 100 ? 'Complete at 100%' : `Hold at ${percent}%`, 29, ink, true, 'right');
    roles.forEach((role, i) => {
      const y = 818 + i * 86, width = 1180;
      this.text(60, y, role, 28, ink, true);
      this.rect(370, y - 9, width, 18, surface, 3);
      this.rect(370, y - 9, width * Math.min(1, ready[target][i] / desired[i]), 18, fills[target], 3);
      const marker = 370 + width * quota[i] / desired[i];
      this.line(marker, y - 20, marker, y + 20, labels[target], 3);
      this.text(1845, y, `${ready[target][i]} / ${desired[i]} Ready`, 29, ink, false, 'right');
      this.hit(370, y - 25, width, 50, `${role}: canary hold`,
        `Bar: ${ready[target][i]} Ready ${String.fromCharCode(65 + target)} groups out of ${desired[i]} desired. Marker: ${quota[i]} groups at the requested hold. Partial percentages round up while keeping both revisions.`);
    });
    this.text(370, 958, 'Marker = requested hold · Bars show Ready groups, not traffic weight', 23, muted);
    this.rect(48, 996, 1824, 66, surface, 10);
    this.text(960, 1029, cue, 28, ink, false, 'center');
    const caption = `${cue} ${action} canaryPercent ${percent}; desired ${desired.join('/')} Prefill/Decode. ` +
      counts.map((r, revision) => `${String.fromCharCode(65 + revision)}: ${r.join('/')} groups, ${ready[revision].join('/')} Ready.`).join(' ') +
      ` Serving Ready ${serving.join('/')}; floors ${desired.join('/')}; requested ${total.join('/')}; ceilings ${desired.map(d => d + 1).join('/')}. Illustrative timing.`;
    this.drawing.frames.push({duration: seconds, caption, items: this.items, hits: this.hits});
    // Catch teaching states that invent Ready groups.
    counts.forEach((r, revision) => r.forEach((n, i) => assert.ok(n >= ready[revision][i] && ready[revision][i] >= 0)));
  }
  async save(name) {
    const drawing = compactDrawing(this.drawing);
    await writeFile(new URL(`../static/animations/${name}.json`, import.meta.url), `${JSON.stringify(drawing)}\n`);
    console.log(`${name}: ${drawing.frames.length} frames, ${drawing.frames.reduce((sum, f) => sum + f.duration, 0).toFixed(1)}s`);
  }
}

const promotion = new Scene('Promote a canary · 0 → 25 → 50 → 100%');
const promote = (percent, a, b, action, pending = [0, 0], seconds = .75, cue = 'Wait for Ready replacements, then remove old groups.') =>
  promotion.frame({percent, counts: [a, b], ready: [a, b.map((n, i) => n - pending[i])], seconds, action, cue});
promote(0, [4, 8], [0, 0], 'Commit templates B and the initial hold together.', [0, 0], 3, 'Argo CD applies Git; LWS keeps the requested share.');
promote(25, [4, 8], [1, 1], 'Set 25%: start B in both roles.', [1, 1], 1.5);
promote(25, [4, 8], [1, 1], 'B becomes Ready in both roles.');
promote(25, [3, 7], [1, 1], 'Remove the replaced A groups.');
promote(25, [3, 7], [1, 2], 'Start the second B Decode.', [0, 1]);
promote(25, [3, 7], [1, 2], 'The new Decode becomes Ready.');
promote(25, [3, 6], [1, 2], 'Hold: 1 of 4 Prefill and 2 of 8 Decode are new.', [0, 0], 3, 'Hold at 25%: check the engine before promoting.');
promote(50, [3, 6], [2, 3], 'Set 50%: add the next replacements.', [1, 1], 1.1);
promote(50, [3, 6], [2, 3], 'The new groups become Ready.');
promote(50, [2, 5], [2, 3], 'Remove the replaced A groups.');
promote(50, [2, 5], [2, 4], 'Start the fourth B Decode.', [0, 1]);
promote(50, [2, 5], [2, 4], 'The new Decode becomes Ready.');
promote(50, [2, 4], [2, 4], 'Hold: half of each role is new.', [0, 0], 3, 'Hold at 50%: the same percentage applies to both roles.');
promote(100, [2, 4], [3, 5], 'Set 100%: continue the rollout.', [1, 1], 1.1);
promote(100, [2, 4], [3, 5], 'The new groups become Ready.');
promote(100, [1, 3], [3, 5], 'Remove the replaced A groups.');
promote(100, [1, 3], [4, 6], 'Start the next replacements.', [1, 1]);
promote(100, [1, 3], [4, 6], 'The new groups become Ready.');
promote(100, [1, 2], [4, 6], 'A’s last Prefill stays with its Decode.', [0, 0], 1.5);
promote(100, [1, 2], [4, 7], 'Start the next B Decode.', [0, 1]);
promote(100, [1, 2], [4, 7], 'The new Decode becomes Ready.');
promote(100, [1, 1], [4, 7], 'One old group remains in each role.');
promote(100, [1, 1], [4, 8], 'Start the final B Decode.', [0, 1]);
promote(100, [1, 1], [4, 8], 'B is fully Ready; A can now retire.');
promote(100, [0, 0], [4, 8], 'Complete: B serves the full desired workload.', [0, 0], 3, '100% completes the rollout; readiness and budgets still apply.');
await promotion.save('canary-promotion');

const replacement = new Scene('Replace canary B with C · keep baseline A', true);
const replace = (b, c, action, pending = [0, 0], seconds = .8, cue = 'A keeps its own half; B rolls to C within the other half.') =>
  replacement.frame({percent: 50, counts: [[2, 4], b, c], ready: [[2, 4], b, c.map((n, i) => n - pending[i])], seconds, action, cue});
replace([2, 4], [0, 0], 'Change templates B → C; leave canaryPercent at 50.', [0, 0], 3);
replace([2, 4], [1, 1], 'Start C in both roles.', [1, 1], 2);
replace([2, 4], [1, 1], 'C becomes Ready in both roles.', [0, 0], 1);
replace([1, 3], [1, 1], 'Remove the replaced B groups.');
replace([1, 3], [2, 2], 'Start the next C replacements.', [1, 1]);
replace([1, 3], [2, 2], 'The new C groups become Ready.');
replace([1, 2], [2, 2], 'Keep B’s last Prefill while its Decode remains.', [0, 0], 2.5);
replace([1, 2], [2, 3], 'Start another C Decode.', [0, 1]);
replace([1, 2], [2, 3], 'The new C Decode becomes Ready.');
replace([1, 1], [2, 3], 'One B group remains in each role.');
replace([1, 1], [2, 4], 'Start the final C Decode.', [0, 1]);
replace([1, 1], [2, 4], 'C can now serve the entire canary half.', [0, 0], 1);
replace([0, 0], [2, 4], 'Hold again: A + C, each at 50%.', [0, 0], 4, 'A kept the same groups, LWS objects and Pod UIDs throughout.');
await replacement.save('canary-replacement');

const reversal = new Scene('Reduce a canary · 50 → 25 → 0%');
const reverse = (percent, a, b, action, pending = [0, 0], seconds = .8, cue = 'Restore Ready baseline capacity before removing canary groups.') =>
  reversal.frame({percent, counts: [a, b], ready: [a.map((n, i) => n - pending[i]), b], seconds, action, cue, zero: percent === 0 && b.every(n => n === 0)});
reverse(50, [2, 4], [2, 4], 'Start from a Ready 50% hold.', [0, 0], 2.5);
reverse(25, [3, 5], [2, 4], 'Set 25%: grow baseline A.', [1, 1], 2);
reverse(25, [3, 5], [2, 4], 'Restored A groups become Ready.');
reverse(25, [3, 5], [1, 3], 'Remove the replaced B groups.');
reverse(25, [3, 6], [1, 3], 'Restore one more A Decode.', [0, 1]);
reverse(25, [3, 6], [1, 3], 'The A Decode becomes Ready.');
reverse(25, [3, 6], [1, 2], 'Hold: B is back to 25%.', [0, 0], 2.5);
reverse(0, [4, 7], [1, 2], 'Set 0%: restore the remaining baseline groups.', [1, 1], 1.5);
reverse(0, [4, 7], [1, 2], 'The restored groups become Ready.');
reverse(0, [4, 7], [1, 1], 'Keep B complete until A can take over.');
reverse(0, [4, 8], [1, 1], 'Start the final A Decode.', [0, 1]);
reverse(0, [4, 8], [1, 1], 'A now supplies all desired Ready capacity.');
reverse(0, [4, 8], [0, 0], 'B’s LWS objects remain, with zero replicas.', [0, 0], 5, '0% still desires B’s templates. Restore A in Git for a durable rollback.');
await reversal.save('canary-reversal');

const scaling = new Scene('Autoscale a canary · keep the 25% split');
const scale = (desired, a, b, action, seconds, ready = [a, b], cue = 'An active canary resizes both revisions, including with Deferred.') =>
  scaling.frame({percent: 25, desired, counts: [a, b], ready, seconds, action, cue});
scale([4, 8], [3, 6], [1, 2], 'Start from a Ready 25% hold.', 3);
scale([8, 16], [3, 6], [1, 2], 'Autoscaler doubles desired: 4/8 → 8/16.', 3, undefined, 'More demand raises the floor; keep existing groups while new capacity starts.');
scale([8, 16], [6, 12], [1, 2], 'Grow A to its new 75% share.', 2, [[3, 6], [1, 2]], 'Both shares grow. Starting groups do not yet supply Ready capacity.');
scale([8, 16], [6, 12], [1, 2], 'The restored A groups become Ready.', 1);
scale([8, 16], [6, 12], [2, 4], 'Grow B to its new 25% share.', 2, [[6, 12], [1, 2]]);
scale([8, 16], [6, 12], [2, 4], 'Hold: 6 + 2 Prefill, 12 + 4 Decode.', 3);
scale([4, 8], [6, 12], [2, 4], 'Autoscaler halves desired: 8/16 → 4/8.', 2.5, undefined, 'Existing excess drains safely; the new ceiling is not an instant deletion order.');
scale([4, 8], [6, 12], [1, 2], 'Reduce B to the smaller canary share.', 1.5);
scale([4, 8], [3, 6], [1, 2], 'Reduce A to the smaller baseline share.', 3.5, undefined, 'Back to 3 + 1 Prefill and 6 + 2 Decode: still a 25% canary.');
await scaling.save('canary-scaling');
