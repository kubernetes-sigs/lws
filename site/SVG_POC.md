# SVG animation proof of concept

The documentation's four animations use SVG by default on this branch. Choose
**Renderer: Canvas** in any player to compare the same timestamp without
reloading or downloading another asset. `?renderer=svg` and `?renderer=canvas`
select the initial renderer for every animation on a page.

Use the normal versioned-site setup in [README.md](README.md), with
`snapshot-docs.py --main-ref HEAD`, then open:

- `/main/docs/concepts/disaggregatedset/rollout-strategy/?renderer=svg`
- `/main/docs/concepts/disaggregatedset/scaling-during-rollout/?renderer=svg`
- `/main/docs/concepts/disaggregatedset/llm-d-integration/?renderer=svg`

Pause and switch renderers, or enter fullscreen/browser zoom to compare text.
SVG avoids scaling a fixed-resolution bitmap; it does not enlarge small labels.

## Scope

- Same JSON, validation, timeline, interpolation, controls and hover metadata.
- No additional dependency, regenerated media, or changed drawing schema.
- SVG geometry/text are built once in `defs`; ordered `use` elements compose
  each frame. Moving elements retain their nodes and update their transforms.
- Each player gets unique IDs, so multiple drawings cannot reference each
  other's primitives. Drawing strings are assigned as text, not injected markup.
- Light/dark themes, reduced motion, offscreen pausing, seeking and fullscreen
  retain the shared player's behavior.

This is a comparison experiment, not a replacement of the separate trace
visualizer or its GIF/MP4 export pipeline. SVG still uses the existing figure-level
accessible summary and hover tooltips; it does not add keyboard navigation for
every shape. Keeping both renderers intentionally adds some code for comparison.

## Verification

```sh
npm run test:integration --prefix site
LWS_SITE_URL=http://localhost:1313 LWS_RENDERER=svg npm run test:animations --prefix site
LWS_SITE_URL=http://localhost:1313 LWS_RENDERER=canvas npm run test:animations --prefix site
```

The browser checks exercise all four drawings, their captions and frame order,
renderer switching without resetting time, themes, hover, playback, exact seeking,
fullscreen and narrow layouts. They default to Chromium; set
`PLAYWRIGHT_BROWSER=firefox` to run with Playwright's installed Firefox build.
