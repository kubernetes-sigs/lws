---
title: "Scaling During a Rollout"
linkTitle: "Scaling During a Rollout"
weight: 60
description: >
  Understand Deferred and Immediate scaling during DisaggregatedSet updates, with a side-by-side animation.
---

A rolling update replaces revision A with B. Demand can change while both are running: changing replica counts adjusts capacity without creating another revision.

The example below reduces demand from **37 to 20 Prefill groups** and **19 to 10 Decode groups** while B is still loading. It compares Deferred and Immediate using autoscaler requests, with both rollout budgets set to 10%.

<figure class="td-max-width-on-larger-screens">
<!-- Keep a compact area for native playback controls below the picture. -->
<div style="position: relative; padding-top: calc(64.285714% + 2rem);">
<video controls autoplay muted playsinline preload="metadata" poster="/images/disaggregatedset-scale-down-comparison-poster.png" width="1400" height="900" class="img-fluid d-block" style="position: absolute; inset: 0; width: 100%; height: 100%; object-fit: contain; object-position: top; background: #fff;" aria-label="After the same demand reduction, Deferred keeps 52 groups while Immediate keeps 36. Immediate retires A and trims B sooner. Both finish with 20 Prefill and 10 Decode groups Ready.">
  <source src="/videos/disaggregatedset-scale-down-comparison.mp4" type="video/mp4">
  <a href="/videos/disaggregatedset-scale-down-comparison.mp4">Download the scale-down comparison.</a>
</video>
</div>
<figcaption class="small text-muted mt-2">Real planner decisions with simulated readiness and timing. This example assumes deletions remove Ready groups first.</figcaption>
</figure>

After watching the animation, you might ask:

**Why not reduce B straight to 20 Prefill groups?** With `maxUnavailable: 10%`, at least **18 must stay Ready**. At 23 groups, 18 are Ready and 5 are loading. Reaching 20 requires three deletions. The planner chooses a count, not which groups are deleted: losing three Ready groups would leave only **15**. It cannot assume that only loading groups will be removed, so it waits for more groups to become Ready.

## Choose a scaling policy

`spec.scalingPolicy.scaleDownDuringRollout` controls how reductions happen:

| Policy | Demand increases | Demand decreases |
| --- | --- | --- |
| `Deferred` (default) | Grow B during the rollout. | Keep requested B groups until old revisions drain. A can still retire. |
| `Immediate` | Grow B during the rollout. | Reduce safely now, preferring A before B. |

To choose Immediate:

```yaml
spec:
  scalingPolicy:
    scaleDownDuringRollout: Immediate
```

Static roles receive requests through `roles[].spec.replicas`; External roles receive autoscaler requests through their [RoleScaler](../role-scaler/).

A replica group is one copy of a role. It may contain several Pods, which must all be Ready for the group to count as Ready.

During **scale-up**, both policies behave the same: they add groups to B without scaling A back up. Immediate does not make scale-up faster. The update continues as the new groups become Ready.

During **scale-down**, Deferred waits until the old groups are gone before reducing B, which can keep more resources in use temporarily. Immediate tries to free resources sooner, removing old groups first; it can also remove excess B groups when safe.

**Immediate does not mean instant removal.** Both policies follow your rollout budgets, so extra groups may need to stay while replacements become Ready. Freeing resources earlier does not necessarily make the whole rollout finish sooner.

During scale-down, the difference depends on how far the update has progressed; sometimes both policies can make the same changes. See [policy details and manual scaling](../#scaling-during-a-rollout) for more.
