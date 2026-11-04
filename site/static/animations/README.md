# Prepared documentation animations

These JSON assets preserve the documentation scenes as finite drawing data. LWS only needs the small generic player in `site/assets/js/animation-player.ts`; the rollout calculations, teaching schedules, source-trace processing, and annotation logic remain in the separate `disaggregatedset-trace-player` project.

## Format

Each version-1 asset contains a 1920×1080 canvas, deduplicated `primitives` and `hits` dictionaries, and frames with durations, ordered indices, and captions. A frame's `items` reference primitives; its `hits` reference the top-level hover/tap evidence table. Hit order is preserved, including overlapping targets. Primitives are rectangles, circles, line paths, or text. Colors contain either one value or a light/dark pair. Optional motion tracks contain a primitive index and `[time, x, y]` keyframes.

The player also accepts older version-1 drawings with inline frame hits, normalizing them to the shared table after validation. New assets always use indexed hits. To compact an older export without rendering, run from `site/`:

```sh
node scripts/compact-animation-hits.mjs static/animations/example.json
```

This deterministic, idempotent migration changes only hit storage. Captions, frame timing, primitive data, motion tracks, and hit ordering are unchanged.

Playback draws the prepared frame and interpolates positions. It does not calculate replica counts, availability budgets, fractional windows, or routing shares. All original scene states, metric values, and explanations are retained. Text uses Arial and Menlo.

Optional `themes` names the recorded palettes; color arrays contain one value per
theme instead of a light/dark pair. The unlisted trace player exposes all six
palettes with `?themes=all`, while documentation follows the site's LWS theme.
`paletteSource` identifies the original palette export when its colors were
carried forward separately from the current scene text and geometry.

Each file's `provenance` and `source` metadata retain the evidence description, original scene checkpoint, fixture references, and SHA-256 hashes of the relevant renderer and source data. The distinction matters: gating uses observed Pod states with calculated routing shares and illustrative requests; scaling replays real Kubernetes runs with controlled model-loading readiness; fractional-window progression is a deterministic teaching schedule. Scaling panels are independent recordings aligned at the replica-change marker, with compressed playback. Counts come from completed API observations, not planner simulations.

## Regeneration

The authoring tool's historical `scaling` scene produces `scaling.json`; rename
that output to `scaling-down.json` for the current documentation.

Use a separate checkout of `disaggregatedset-trace-player`. From that project's directory:

```sh
pnpm install --frozen-lockfile
pnpm exec playwright install chromium
pnpm export:docs --themes lws,lws-dark --out /path/to/lws/site/static/animations
```

To regenerate only one asset, add `--scene gating`, `--scene scaling`, `--scene scaling-up`, or `--scene fractional`. For recorded scaling input, follow the standalone project's `docs/RECORDED_SCALING.md`. Use `PLAYWRIGHT_CHANNEL=chrome` with the export command to reuse an installed Google Chrome instead of Playwright's downloaded browser.

The tool invokes the original scene renderer, records its finite Canvas operations, and verifies every state boundary in both LWS themes against the source pixels before writing the files. Gating's decorative motion is sampled at 24 fps and interpolated during playback. See the standalone project's `docs/PREPARED_DRAWINGS.md` for the detailed workflow. No videos or generated raster frames are required.

The scaling illustrations share a roughly 44-second presentation length, not a
real-time scale. After regeneration, match scale-down to scale-up by multiplying
its non-final frame durations by `(targetDuration - finalHold) / (originalDuration - finalHold)`.
Keep the final three-second hold, original observations and captured timestamps.
The scale-down asset records this adjustment in `presentationTiming` and provenance.
