# Virtual-role walkthrough source

This silent Manim illustration accompanies the Virtual Roles (with llm-d) guide.
It is not a live-cluster trace. Operations show a healthy Ordinal parent in one
slice/revision, followed by a separate two-revision routing counterexample.

## Render

Use Python 3.12, `manim==0.19.0`, `av`, and `imageio-ffmpeg`, with Arial and
Menlo installed. Run from this directory, placing intermediate output outside
the repository:

```sh
python -m manim -s -r 1680,1050 --media_dir /tmp/virtual-roles-render scene.py TypographyCheck
python -m manim -r 1680,1050 --fps 24 --media_dir /tmp/virtual-roles-render scene.py VirtualRoles
python export.py /tmp/virtual-roles-render/videos/scene/1050p24/VirtualRoles.mp4
```

Inspect the typography sample before rendering. Text is generated at 4× size
and then reduced to preserve letter spacing. The export script writes the
site's H.264 MP4 and poster, enables fast start, and decodes every frame.

## Semantics checked

- LWS `c4b4d428b32befd46c41fd6c4642c73aae50c365`: the subrole API and
  `pkg/controllers/disaggregatedset/subrole_groups.go`.
- llm-d-router `23dea2f75087d61a0b7293412a553f51d004bb31`: the context-length
  filter, request director, revision screener, and scheduling profile.
- Numeric pool names are interpreted by the configured llm-d plugin, not LWS.
- `4+2 → 5+2 → 3+4 → 0+4`: Ordinal retains indices 0–3 on the final shrink.
  Existing groups can be relabeled without restarting; assignment is not atomic.
- The last scene illustrates a possible failed routing decision, not a guarantee
  that every Long request fails. Parent-role coverage is not subrole coverage.
- No runtime or controller change is implemented here. Hash exposes the same
  labels, but does not promise the same deletion order or child availability.
