# SVG animation proof of concept

The documentation's four animations use SVG exclusively on this branch.
There is no renderer selector or Canvas rendering path.

Use the normal versioned-site setup in [README.md](README.md), with
`snapshot-docs.py --main-ref HEAD`, then open:

- `/main/docs/concepts/disaggregatedset/rollout-strategy/`
- `/main/docs/concepts/disaggregatedset/scaling-during-rollout/`
- `/main/docs/concepts/disaggregatedset/llm-d-integration/`

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

The historical `canvas-animation` shortcode name is retained so existing pages
and release snapshots do not need edits; its output is now SVG.

## Limitations

This does not migrate the separate trace visualizer. Its scenarios, JSON and
timeline can use SVG, but large imported drawings need benchmarking: this renderer
creates a DOM definition for every primitive. Its GIF/MP4 exporter currently uses
an independent Canvas for pixel encoding and recording, which can remain an
export-only backend even if the on-screen player becomes SVG-only.

Accessibility still uses the existing figure-level summary and hover tooltips;
this does not add keyboard navigation for every shape.

## Verification

```sh
npm run test:integration --prefix site
LWS_SITE_URL=http://localhost:1313 npm run test:animations --prefix site
```

The browser checks exercise all four drawings, their captions and frame order,
motion positions and visibility, themes, hover, playback, exact seeking,
fullscreen and narrow layouts. They default to Chromium; set
`PLAYWRIGHT_BROWSER=firefox` to run with Playwright's installed Firefox build.
