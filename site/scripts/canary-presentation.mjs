/** Short narration for the recorded canary suite. Every API view is retained.
 * The standalone exporter renders these moments with its existing Canvas code.
 */
export function observedPercent(frame) {
  return frame.deltas?.find(line => line.startsWith('Observed canaryPercent:')) ?? 'Canary percentage not recorded.';
}

export function observationState(frame) {
  // Include Pod identities and all planner fields, including unknown values.
  return JSON.stringify([frame.roles, observedPercent(frame)]);
}

function requestPhase(step, external, previousPercent) {
  const request = step.title.match(/^Request (\d+)% new at (\d+) Prefill \/ (\d+) Decode: (.+)\.$/);
  if (request) {
    const [, percent, prefill, decode, split] = request;
    let instruction = 'Open readiness as groups arrive; preserve the availability minimum.';
    if (percent === '100') instruction = 'Advance to 100%; replace the remaining A groups with B.';
    else if (percent === '0') instruction = previousPercent === null ? 'Hold A; B stays at zero.'
      : 'Restore A before draining B; retain B’s empty LWS objects.';
    else if (previousPercent !== null && Number(percent) < previousPercent) {
      instruction = 'Restore A before reducing B; keep enough groups Ready.';
    } else if (external) instruction = 'Resize through the External scalers; keep the same percentage.';
    return {title: `${percent}% new · ${prefill} Prefill / ${decode} Decode`,
      lines: [`Expected ${split}.`, instruction]};
  }
  if (step.title.startsWith('Give the held canary to External')) return {
    title: 'Static → External scaling', lines: ['Keep the current targets and existing Pods.', 'Delay the first autoscaler write; capacity must stay unchanged.']};
  if (step.title.startsWith('Hold B at 50%')) return {
    title: 'Hold B at 50%', lines: ['Expected A=2 Prefill / 4 Decode and B=2 Prefill / 4 Decode.', 'Both halves must be Ready before the next update.']};
  if (step.title.startsWith('Keep A fixed at 50%')) return {
    title: 'Replace B with C; keep A at 50%', lines: ['C starts with readiness closed; B continues serving.', 'A must keep the same LWS specification, Pod IDs and subrole labels.']};
  if (step.title === 'Allow C to replace B safely.') return {
    title: 'Open C readiness', lines: ['C replaces B in the canary half.', 'Expected A=2/4 and C=2/4; B=0. A remains unchanged.']};
  return null;
}

export function buildCanaryTimeline(scenario) {
  if (!scenario.id.startsWith('canary')) throw new Error('This narration is only for the canary recordings.');
  const chain = scenario.id.startsWith('canary-chain');
  const moments = [], seenEvents = new Set();
  let time = 0, currentIndex = null, currentFrame = null, previousState;
  let phaseIndex = 0, external = false, previousPercent = null;
  let phase = {title: 'Set up 4 Prefill / 8 Decode groups',
    lines: ['Create A and wait for all groups to be Ready.', 'maxSurge=1 and maxUnavailable=0 for each logical role.']};
  const push = (card, duration, held = true, step = phase) => {
    moments.push({start: time, end: time + duration, frameIndex: currentIndex, frame: currentFrame,
      step, stepIndex: phaseIndex, card: {...card, key: `${scenario.id}/${card.key}`}, noNewObservation: held});
    time += duration;
  };
  push({key: 'compact-intro', heading: 'What this scenario tests', kind: 'intro', lines: [
    'Start with 4 Prefill and 8 Decode groups; one Pod per group.',
    'Each logical role uses maxSurge=1 and maxUnavailable=0.',
    chain ? 'Hold A+B at 50%, then replace B with C in the canary half.'
      : 'Promote, reverse, hold at 0%, then resize through External scalers.',
    chain ? 'Expected: A keeps its Pods and half the capacity throughout.'
      : 'Expected: preserve availability and reach every requested split.',
  ]}, chain ? 10 : 11, true, null);

  let visited = 0;
  for (const step of scenario.steps) {
    const nextPhase = requestPhase(step, external, previousPercent);
    if (nextPhase) {
      phase = nextPhase; phaseIndex++;
      if (step.title.startsWith('Give the held canary')) external = true;
      if (step.title.startsWith('Request ')) previousPercent = Number(step.title.match(/^Request (\d+)/)[1]);
      push({key: `${step.id}/compact-trigger`, heading: 'Scenario trigger', kind: 'trigger', lines: phase.lines}, 4);
    }
    for (const index of step.frame_indices) {
      if (index !== visited++) throw new Error('Every API observation must appear once, in order.');
      currentIndex = index; currentFrame = scenario.frames[index];
      const state = observationState(currentFrame), changed = state !== previousState;
      const freshEvents = (currentFrame.events ?? []).filter(reason => !seenEvents.has(reason));
      (currentFrame.events ?? []).forEach(reason => seenEvents.add(reason));
      const newReadError = (scenario.read_errors_at ?? []).some(elapsed => elapsed <= currentFrame.elapsed &&
        elapsed > (scenario.frames[index - 1]?.elapsed ?? -Infinity));
      const lines = [observedPercent(currentFrame), phase.lines[0]];
      if (freshEvents.length) lines.push(`New recorded Event: ${freshEvents.join(', ')}.`);
      if (newReadError) lines.push('An API read failed; this is the next successful observation.');
      push({key: `${step.id}/frame-${index}/compact-observed`, heading: 'Observed state', kind: 'observed', lines},
        newReadError ? 4 : freshEvents.length ? 2 : changed ? 1.1 : .15, false);
      previousState = state;
    }
    // Never hide a failed checkpoint while removing repeated passing prose.
    if (step.checks?.passed === false) {
      const lines = step.checks.pages?.flat() ?? step.checks.lines ?? ['Checkpoint failed.'];
      for (let i = 0; i < lines.length; i += 3) push({key: `${step.id}/failure-${i}`,
        heading: 'Recorded checkpoint failed', kind: 'check', verdict: false, lines: lines.slice(i, i + 3)}, 6);
    }
  }
  if (visited !== scenario.frames.length) throw new Error('Missing API observations.');
  const passed = scenario.outcome.passed;
  const lines = passed === true ? [chain
    ? 'PASS: A kept its specification and original Pods. C replaced B in the other half.'
    : 'PASS: requested splits, 0% hold, scaler handoff and proportional resizing matched expectations.',
    'Ready capacity and requested-group limits passed the recorded checks.']
    : [scenario.outcome.detail, scenario.outcome.error].filter(Boolean);
  const lateReadErrors = (scenario.read_errors_at ?? []).filter(elapsed => elapsed > (currentFrame?.elapsed ?? -Infinity));
  if (scenario.outcome.error && passed === true) lines.push(scenario.outcome.error);
  if (lateReadErrors.length) lines.push(lateReadErrors.length === 1 && passed === true &&
    scenario.outcome.detail?.includes('interrupted as the harness shut down')
    ? 'One final API read was canceled during shutdown after the checks passed.'
    : `${lateReadErrors.length} failed API read(s) were recorded after the last successful view.`);
  push({key: 'compact-outcome', heading: 'Recorded outcome', kind: 'outcome', verdict: passed, lines}, lateReadErrors.length ? 10 : 6);
  return {moments, duration: time, scenario, observationCount: visited};
}
