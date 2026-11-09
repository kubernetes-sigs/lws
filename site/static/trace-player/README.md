# Prepared harness scenarios

`scenarios/small/` and `scenarios/large/` contain one gzip-compressed drawing per recorded scenario. The catalog is `site/data/trace_player.json`; documentation examples reuse `/animations/*.json`.

These assets replay historical harness recordings, not a live cluster connection. They preserve the original Canvas renderer's observations, event explanations, readiness/limit metrics, outcomes, and inspection details. API snapshots and planner previews were captured independently; the diagrams do not invent intermediate states or make them atomic. They use the same finite drawing format as the documentation animations. Every file includes original provenance, source trace and renderer hashes, and observation/card references. No controller calculations, trace-normalization engine, videos, or duplicate source traces are shipped here.

Regenerate from a separate `disaggregatedset-trace-player` checkout:

```sh
pnpm install --frozen-lockfile
pnpm exec playwright install chromium
pnpm export:harness \
  --out /path/to/lws/site/static/trace-player \
  --manifest /path/to/lws/site/data/trace_player.json
```

The exporter invokes the original renderer in all six verified LWS, Mistral, and llm-d light/dark palettes. It verifies source observation coverage, matching geometry, and pixels for every presentation card in every palette. The `themes` array fixes palette order; color arrays retain the original scene's exact ink, including green/red metric semantics. There is no heuristic recoloring. Output gzip timestamps are fixed and decoded assets must fit within 16 MiB. Only the selected scenario is downloaded and decompressed in the browser.

These are **prepared drawings**, not normalized traces. Import new normalized traces into the standalone tool; the small site player does not interpret cluster observations. See that tool's `docs/PREPARED_DRAWINGS.md` for details.

Packaged drawings use the shared top-level `hits` dictionary and integer frame
references. Older inline-hit imports are normalized after validation. When
regenerating, compact each decoded drawing with `scripts/compact-animation-hits.mjs`
before deterministic gzip compression; timing and inspection order are unchanged.
