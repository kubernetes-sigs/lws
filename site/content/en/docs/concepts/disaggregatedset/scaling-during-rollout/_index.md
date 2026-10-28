---
title: "Scaling During a Rollout"
linkTitle: "Scaling During a Rollout"
weight: 60
description: >
  Choose when to add or remove capacity during DisaggregatedSet updates.
---

A rolling update replaces revision A with B. Demand can change while both are running: changing replica counts adjusts capacity without creating another revision.

## Choose a scaling policy

Scale-up and scale-down have separate settings under `spec.scalingPolicy`. Both default to `Deferred`:

| Setting | `Deferred` (default) | `Immediate` |
| --- | --- | --- |
| `scaleUpDuringRollout` | Grow B in normal rollout batches, waiting for readiness when needed. | Request enough B groups to fill the desired count after subtracting groups still requested in A. |
| `scaleDownDuringRollout` | Keep requested B groups until old revisions drain. A can still retire. | Reduce safely now, preferring A before B. |

To enable both Immediate behaviors:

```yaml
spec:
  scalingPolicy:
    scaleUpDuringRollout: Immediate
    scaleDownDuringRollout: Immediate
```

You can enable either independently. Choosing Immediate scale-up does not enable Immediate scale-down.

These rules apply to both Ordinal and Hash. Static roles receive requests through `roles[].spec.replicas`; External roles receive autoscaler requests through their [RoleScaler](../role-scaler/).

A replica group is one copy of a role. It may contain several Pods, which must all be Ready for the group to count as Ready.

## When demand increases

Only B grows; A is never scaled back up. The scale-up policy determines whether new groups must wait behind a batch that is still starting.

For example, a role has **4 Ready groups in A** and **1 loading group in B**. Desired replicas increase from **4 to 10**, with `maxSurge: 1` and `maxUnavailable: 0`:

- **Deferred** keeps B at 1 for now: the existing group must become Ready before more groups are requested.
- **Immediate** grows B to **6 = 10 desired − 4 in A**, without waiting for the loading group. A keeps its four Ready groups.

Immediate counts requested groups, including those still loading, across **all** old revisions. A readiness drop alone does not request more groups. If old groups later retire safely, B can fill the newly available gap even without another replica-count change.

This extra growth reaches the desired count; it does not raise `maxSurge` or permit extra old-group deletions. Normal rollout growth may still use the allowed surge, and renamed-role safeguards still apply. Immediate can start more groups sooner, but cannot make them schedulable or Ready sooner.

## When demand decreases

The scale-down policy controls reductions independently of the scale-up policy. **Deferred** waits until old groups are gone before reducing B, which can temporarily keep more resources in use. **Immediate** tries to free resources sooner, removing old groups first; it can also remove excess B groups when safe.

The animation below reduces demand from **37 to 20 Prefill groups** and **19 to 10 Decode groups** while B is still loading. It compares the two **scale-down** policies using autoscaler requests, with both rollout budgets set to 10%. Scale-up remains Deferred in both examples.

<figure class="td-max-width-on-larger-screens">
<!-- Keep a compact area for native playback controls below the picture. -->
<div style="position: relative; padding-top: calc(64.285714% + 2rem);">
<video controls autoplay loop muted playsinline preload="metadata" poster="/images/disaggregatedset-scale-down-comparison-poster.png" width="1400" height="900" class="img-fluid d-block" style="position: absolute; inset: 0; width: 100%; height: 100%; object-fit: contain; object-position: top; background: #fff;" aria-label="After the same demand reduction, Deferred keeps 52 groups while Immediate keeps 36. Immediate retires A and trims B sooner. Both finish with 20 Prefill and 10 Decode groups Ready.">
  <source src="/videos/disaggregatedset-scale-down-comparison.mp4" type="video/mp4">
  <a href="/videos/disaggregatedset-scale-down-comparison.mp4">Download the scale-down comparison.</a>
</video>
</div>
<figcaption class="small text-muted mt-2">Real planner decisions with simulated readiness and timing. This example assumes deletions remove Ready groups first.</figcaption>
</figure>

After watching the animation, you might ask:

**Why not reduce B straight to 20 Prefill groups?** With `maxUnavailable: 10%`, at least **18 must stay Ready**. At 23 groups, 18 are Ready and 5 are loading. Reaching 20 requires three deletions. The planner chooses a count, not which groups are deleted: losing three Ready groups would leave only **15**. It cannot assume that only loading groups will be removed, so it waits for more groups to become Ready.

**Immediate does not mean instant removal.** Both scale-down policies follow your rollout budgets, so extra groups may need to stay while replacements become Ready. Freeing resources earlier does not necessarily make the whole rollout finish sooner.

During scale-down, the difference depends on how far the update has progressed; sometimes both policies can make the same changes. See [policy details and manual scaling](../#scaling-during-a-rollout) for more.
