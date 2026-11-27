# Prepared harness scenarios

`scenarios/small/`, `scenarios/large/`, and `scenarios/canary/` contain one gzip-compressed drawing per recorded scenario. The catalog is `site/data/trace_player.json`; documentation examples reuse `/animations/*.json`.

These assets replay historical harness recordings, not a live cluster connection. They preserve the original Canvas renderer's observations, event explanations, readiness/limit metrics, outcomes, and inspection details. API snapshots and planner previews were captured independently; the diagrams do not invent intermediate states or make them atomic. They use the same finite drawing format as the documentation animations. Every file includes original provenance, source trace and renderer hashes, and observation/card references. No controller calculations, trace-normalization engine, videos, or duplicate source traces are shipped here.

The twelve canary recordings were refreshed on November 27, 2026. They cover Ordinal and Hash, ordinary and virtual roles, forward promotion and rejection of percentage decreases, External scaler handoff, Immediate resizing versus Deferred queued demand, retained A/B pairs, and a singleton peer resuming from zero. Four cases keep A's half unchanged while B rolls to C. Ordinal virtual-role compaction may reassign already-running Pods without starting new ones. Playback is 2:20–2:53 per resizing case and 0:59–1:03 per chained rollout, about 24:27 in total. All 1,422 successful API views remain in order: changed Pod states, counts, percentages or planner fields stay visible for at least one second; unchanged views use a short dwell. The player clock measures presentation time, while each view retains its acquisition time and planner-preview age.

These twelve passing cases combine a full run with focused reruns, not one uninterrupted green run. The original run exposed two harness assumptions: separate percentage and `/scale` writes are not atomic, and Ordinal virtual-role compaction need not preserve logical-role Pod names. The scenario sequence and assertions were corrected. Four Deferred cases were also re-recorded after the read-only observer was taught to use admitted demand. A replacement uses the whole completed scenario; observations from different attempts are never spliced together. Each drawing records its original integration commit, raw-input hashes and implementation/harness component commits in `provenance` and `source.components`:

| Selected cases | Implementation | Harness |
| --- | --- | --- |
| Immediate ordinary roles, Immediate Hash virtual roles, all A→B→C cases | `18e4603` | `1773f94` |
| Four Deferred resizing cases and Immediate Ordinal virtual roles | `ddcfab7` | `46c9d2d` |

Revision display names are scoped to the scenario's DisaggregatedSet owner.
Earlier workloads still visible during setup are labeled `cleanup A`, etc.,
instead of making the current scenario's real A/B/C appear as A2/B2/C2.
Cleanup Pods remain in the observations; only empty cleanup rows are hidden.
The normalizer adapter's hash is included in the drawing provenance.

The canary presentation removes repeated passing-check pages, duplicate trigger explanations and separate pages for recurring Events. It retains their recorded sequence and verdicts in `source.recordedSteps`, per-frame evidence, and `source.recordedOutcome`; failed checks and failed-read markers remain visible. Displayed percentages and structured canary status come from the matching raw API sample; introductions describe expected behavior. Empty cleanup placeholders from preceding cases are hidden, while any observed cleanup Pods remain visible. The compact timeline is defined in `site/scripts/canary-presentation.mjs`. Export it with the standalone renderer, applying `site/scripts/canary-renderer.mjs` to its render source in memory. These adapters change narration, dwell times and empty display rows, not the source observations or controller behavior. For these recordings, the normalizer's numeric validation also accepts a signed `pendingGrowthLimit`: the planner's `surgeCeiling - oldSpec` can legitimately be negative during shrink. Values are not clamped; replica counts remain nonnegative. The adapted normalizer hash is retained alongside the renderer hashes.

Regenerate the bundled small/large recordings from a separate `disaggregatedset-trace-player` checkout:

```sh
pnpm install --frozen-lockfile
pnpm exec playwright install chromium
pnpm export:harness \
  --out /path/to/lws/site/static/trace-player \
  --manifest /path/to/lws/site/data/trace_player.json
```

The exporter invokes the original renderer in all six verified LWS, Mistral, and llm-d light/dark palettes. It verifies source observation coverage, matching geometry, and pixels for every presentation card in every palette. The refreshed canary suite passed all 9,468 frame/palette comparisons with zero differing pixels. The `themes` array fixes palette order; color arrays retain the original scene's exact ink, including green/red metric semantics. There is no heuristic recoloring. Output gzip timestamps are fixed and decoded assets must fit within 16 MiB. Only the selected scenario is downloaded and decompressed in the browser.

These are **prepared drawings**, not normalized traces. Import new normalized traces into the standalone tool; the small site player does not interpret cluster observations. See that tool's `docs/PREPARED_DRAWINGS.md` for details.

Packaged drawings use the shared top-level `hits` dictionary and integer frame
references. Older inline-hit imports are normalized after validation. When
regenerating, compact each decoded drawing with `scripts/compact-animation-hits.mjs`
before deterministic gzip compression; timing and inspection order are unchanged.
