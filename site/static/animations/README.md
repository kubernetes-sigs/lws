# Prepared documentation animations

These JSON assets preserve the documentation scenes as finite drawing data. LWS only needs the small generic player in `site/assets/js/animation-player.ts`; the rollout calculations, teaching schedules, source-trace processing, and annotation logic remain in the separate `disaggregatedset-trace-player` project.

## Format

Each version-1 asset contains a 1920×1080 canvas, a deduplicated primitive dictionary, and frames with durations, ordered primitive indices, captions, and hover/tap evidence. Primitives are rectangles, circles, line paths, or text. Colors contain either one value or a light/dark pair. Optional motion tracks contain a primitive index and `[time, x, y]` keyframes.

Playback draws the prepared frame and interpolates positions. It does not calculate replica counts, availability budgets, fractional windows, or routing shares. All original scene states, metric values, and explanations are retained. Text uses Arial and Menlo.

Each file's `provenance` and `source` metadata retain the evidence description, original scene checkpoint, fixture references, and SHA-256 hashes of the relevant renderer and source data. The distinction matters: gating uses observed Pod states with calculated routing shares and illustrative requests; scaling uses production-planner output with simulated readiness/timing; fractional-window progression is a deterministic teaching schedule.

## Regeneration

Use a separate checkout of `disaggregatedset-trace-player`. From that project's directory:

```sh
pnpm install --frozen-lockfile
pnpm exec playwright install chromium
pnpm export:docs --out /path/to/lws/site/static/animations
```

To regenerate only one asset, add `--scene gating`, `--scene scaling`, or `--scene fractional`. Use `PLAYWRIGHT_CHANNEL=chrome` with the export command to reuse an installed Google Chrome instead of Playwright's downloaded browser.

The tool invokes the original scene renderer, records its finite Canvas operations, and verifies every state boundary in both LWS themes against the source pixels before writing the files. Gating's decorative motion is sampled at 24 fps and interpolated during playback. See the standalone project's `docs/PREPARED_DRAWINGS.md` for the detailed workflow. No videos or generated raster frames are required.
