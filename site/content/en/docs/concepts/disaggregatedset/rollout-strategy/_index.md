---
title: "Rollout Strategy"
linkTitle: "Rollout Strategy"
weight: 50
description: >
  How DisaggregatedSet coordinates updates across roles, with an animated example of the fractional window and rollout budgets.
---

A serving application may need Encode, Prefill, and Decode to work together. Updating all the Encode groups while barely starting Prefill and Decode can leave new groups without the matching roles they need.

DisaggregatedSet coordinates their replacement **by proportion, not by equal numbers of groups**. The fractional coordination window limits how far one role's rollout can lead another. Separately, each role's `maxSurge` and `maxUnavailable` control extra replicas and availability.

The example below replaces **4 Encode, 13 Prefill, and 7 Decode groups** from revision A with revision B.

<figure class="td-max-width-on-larger-screens">
<!-- Keep a compact area for native playback controls below the picture. -->
<div style="position: relative; padding-top: calc(58.333333% + 2rem);">
<video controls autoplay loop muted playsinline preload="metadata" poster="/images/disaggregatedset-fractional-rollout-poster.png" class="img-fluid d-block" width="1200" height="700" style="position: absolute; inset: 0; width: 100%; height: 100%; object-fit: contain; object-position: top; background: #fff;" aria-label="A rollout of 4 Encode, 13 Prefill, and 7 Decode groups. Old groups leave as replacements become Ready, with removal progress kept inside a moving 25-percentage-point window and availability checked separately.">
  <source src="/videos/disaggregatedset-fractional-rollout.mp4" type="video/mp4">
  <a href="/videos/disaggregatedset-fractional-rollout.mp4">Download the rollout animation.</a>
</video>
</div>
<figcaption class="small text-muted mt-2">Old A groups retire as new B groups become Ready. The moving 25-point window keeps the roles progressing together; both rollout budgets are 10%. Each square is a single-Pod group. Timing is illustrative.</figcaption>
</figure>

## Set the rollout budgets

The unit is a **replica group**, which can contain a leader Pod and worker Pods. A group counts as Ready only when all its expected Pods are Ready. A running leader alone is not enough.

For example, set both budgets to `10%` in each role's `spec.rolloutStrategy.rollingUpdateConfiguration`:

```yaml
rolloutStrategy:
  rollingUpdateConfiguration:
    maxSurge: 10%
    maxUnavailable: 10%
```

- `maxSurge` allows extra groups above the desired count, including groups still starting. Percentages round **up**.
- `maxUnavailable` allows Ready capacity to fall below the desired count. Percentages round **down**.

The minimum Ready count is `desired - maxUnavailable`; the maximum requested count is `desired + maxSurge`. Both count across old and new revisions of the role. Terminating Pods can take time to disappear.

## Hold a rollout with a partition

A role's `partition` stops its rollout part of the way through, so a few groups of the new revision can serve real traffic before the rest follow. A role with `partition: p` keeps `p` groups on older revisions: the rollout advances until the role has `replicas - p` new groups, all Ready, and then waits. Lowering the partition advances it; `0` completes it.

For example, to put one new group of each role into service and check it:

```yaml
roles:
- name: prefill
  spec:
    replicas: 3
    rolloutStrategy:
      rollingUpdateConfiguration:
        maxSurge: 1
        maxUnavailable: 0
        partition: 2
    # ...
- name: decode
  spec:
    replicas: 3
    rolloutStrategy:
      rollingUpdateConfiguration:
        maxSurge: 1
        maxUnavailable: 0
        partition: 2
    # ...
```

Change the template with the partitions at 2; the rollout stops at one new prefill group and one new decode group. Then set the partitions to 1, and to 0 to finish.

- The partition counts groups **per slice**, and it holds the rollout only: with no template change it does nothing, and the budgets and the fractional window still apply while the rollout advances.
- While held, the `Progressing` condition has reason `RolloutPartitioned`, its message lists each role's groups on the current revision, and one `RollingUpdatePartitioned` event is recorded for each hold. `status.roleStatuses[].updatedReplicas` is the number of new groups per role.
- A partition equal to `replicas` holds a template change at zero new groups. A driver such as a progressive-delivery controller can keep it there between releases and lower it step by step.
- `partition` is not supported on a role with `subRoles`.

### Roll back under a partition

The partition holds a rollback the same way. Reverting the template makes the earlier revision the target again, and the groups of the revision you reverted from become old groups that the partition keeps: with `partition: 2` and one new group out, that group stays until the partition is lowered. **To roll back at once, revert the template and set every partition to 0 in the same update.** To roll back step by step, revert the template and lower the partitions as you would for a rollout.

## What is the fractional window?

The window limits how far one role can get ahead of the others. Its width is **`1 / smallest non-zero role count`**, calculated automatically.

For the old revision, progress is the fraction of its original groups removed. The window moves with the least-advanced role; the others can advance within its width. **It is not a batch size.**

The new revision's growth is coordinated separately, using the fraction of each role's target already requested.

## The window is not an unavailability allowance

Fitting inside the window is not enough: a removal must also respect each role's **`maxUnavailable` budget**.

The new revision counts as replacement serving capacity only once it has a Ready group in **every required role**. Groups still starting do not count as Ready capacity.

These checks govern ordinary rollout steps; they cannot prevent unrelated Pod or node failures or guarantee enough GPU capacity. For changing replica counts, see [Scaling during a rollout](../#scaling-during-a-rollout). The [design proposal](https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet#n-dimensional-rolling-update-algorithm) also covers interrupted rollouts and bounded recovery.
