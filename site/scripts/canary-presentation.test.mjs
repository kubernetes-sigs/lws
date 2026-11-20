import assert from 'node:assert/strict';
import test from 'node:test';
import {buildCanaryTimeline, observationState} from './canary-presentation.mjs';

test('compact narration retains every view, transient state, event and failed checkpoint', () => {
  const role = {name: 'prefill', desired: 1, floor: null, revisions: [{name: 'B', requested: 1,
    pods: [{id: 'one', group: 'group', status: 'pending'}]}]};
  const frames = Array.from({length: 9}, (_, i) => ({elapsed: i, roles: [structuredClone(role)],
    source: {sample_id: 100 + i, capture_seconds: .08}, observer_age_seconds: .25,
    deltas: ['Observed canaryPercent: 25%.'], events: []}));
  frames[2].roles[0].floor = 1; // A metric becomes known even though no Pod changes.
  frames[3].roles[0].revisions[0].pods[0].status = 'ready';
  frames[4].roles[0].revisions[0].pods[0].status = 'deleting';
  frames[5].roles[0].revisions[0].pods[0].id = 'replacement';
  frames[6].deltas = ['Observed canaryPercent: 50%.'];
  frames[7].events = ['RevisionDrainBlocked'];
  frames[8].events = ['RevisionDrainBlocked'];
  const scenario = {id: 'canary-test', frames, read_errors_at: [2.5, 9],
    steps: [{id: 'setup', title: 'Setup', frame_indices: [0, 1, 2, 3]},
      {id: 'promote', title: 'Request 50% new at 4 Prefill / 8 Decode: A=2/4, B=2/4.',
        frame_indices: [4, 5, 6, 7, 8], checks: {passed: false, lines: ['Availability check failed.']}}],
    outcome: {passed: false, detail: 'Run failed.', error: 'Recorded failure.'}};
  const original = structuredClone(scenario);
  const timeline = buildCanaryTimeline(scenario);
  const observed = timeline.moments.filter(moment => !moment.noNewObservation);
  assert.deepEqual(observed.map(moment => moment.frameIndex), frames.map((_, i) => i));
  observed.forEach((moment, i) => {
    assert.equal(moment.frame, frames[i]);
    if (!i || observationState(frames[i - 1]) !== observationState(frames[i])) {
      assert.ok(moment.end - moment.start >= 1, `changed view ${i} needs readable dwell`);
    }
  });
  assert.ok(observed[1].end - observed[1].start < .2, 'unchanged view remains without a long hold');
  assert.match(observed[3].card.lines.join('\n'), /API read failed/);
  assert.ok(observed[3].end - observed[3].start >= 4);
  assert.match(observed[7].card.lines.join('\n'), /RevisionDrainBlocked/);
  assert.ok(timeline.moments.some(moment => moment.card.verdict === false &&
    moment.card.lines.includes('Availability check failed.')));
  assert.match(timeline.moments.at(-1).card.lines.join('\n'), /1 failed API read\(s\).*last successful view/);
  assert.doesNotMatch(timeline.moments.at(-1).card.lines.join('\n'), /canceled during shutdown/);
  assert.deepEqual(scenario, original, 'source observations and evidence must not be changed');
});

test('reversal restores A, while final promotion is not described as autoscaling', () => {
  const steps = [
    'Request 50% new at 4 Prefill / 8 Decode: A=2/4, B=2/4.',
    'Request 25% new at 4 Prefill / 8 Decode: A=3/6, B=1/2.',
    'Give the held canary to External scalers without losing capacity.',
    'Request 25% new at 8 Prefill / 16 Decode: A=6/12, B=2/4.',
    'Request 100% new at 4 Prefill / 8 Decode: A=0/0, B=4/8.',
  ].map((title, i) => ({id: `step-${i}`, title, frame_indices: []}));
  const timeline = buildCanaryTimeline({id: 'canary-test', frames: [], steps, outcome: {passed: true}});
  const triggers = timeline.moments.filter(moment => moment.card.kind === 'trigger');
  assert.match(triggers[1].card.lines.join('\n'), /Restore A before reducing B/);
  assert.match(triggers[3].card.lines.join('\n'), /Resize through the External scalers/);
  assert.match(triggers[4].card.lines.join('\n'), /Advance to 100%/);
  assert.doesNotMatch(triggers[4].card.lines.join('\n'), /Resize|same percentage/);
});
