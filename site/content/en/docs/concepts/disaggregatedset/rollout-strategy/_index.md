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

{{< canvas-animation data="/animations/fractional.json" title="Fractional rollout window: 4 Encode, 13 Prefill, and 7 Decode groups" >}}
Old A groups retire as new B groups become Ready. The moving 25-point window keeps the roles progressing together; both rollout budgets are 10%. Each square is a single-Pod group. Timing is illustrative.
{{< /canvas-animation >}}

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
- `maxUnavailable` allows Ready capacity to fall below the desired count. Percentages round **down**. If both budgets resolve to `0` (for example, `20%` with `maxSurge: 0` below 5 groups), the controller uses `maxUnavailable: 1`.

The minimum Ready count is `desired - maxUnavailable`; the maximum requested count is `desired + maxSurge`. Both count across old and new revisions of the role. Terminating Pods can take time to disappear.

## One percentage for all roles

This feature is provided by [the percentage-canary implementation in #1307](https://github.com/kubernetes-sigs/lws/pull/1307) and requires its controller and matching CRDs.

Use one set-wide percentage to hold a rollout for canary analysis:

```yaml
spec:
  rolloutStrategy:
    canary:
      percent: 25
      dimension: Groups
```

By default, this requests 25% of each role's replica groups on the new revision, independently in every slice. It works with both Ordinal and Hash groups, including virtual roles: each sub-role gets its own share. It does **not** set a traffic percentage or account for different GPU sizes.

To divide complete slices instead, set `dimension: Slices` inside the `canary` block. With eight slices of 1 Prefill + 1 Decode and `canary.percent: 25`, slices 0 and 1 roll to B while six stay on A. At 50%, slices 2 and 3 also roll to B; returning to 25% is rejected. A subsequent B → C update runs inside the selected slices, leaving the others on A.

Choose `Groups` (the default) or `Slices` explicitly; there is no automatic choice. Dimension changes are rejected during a partial canary, and `Slices` also requires an unchanged slice count. Finish at 100% or omit the percentage before changing them. Slice autoscaling is not part of this feature. See the [Argo CD slice example](../argo-cd/#canary-complete-slices).

- Set `0` with the template change to start with all surviving groups old. Raise it to advance. **At `100` or with the percentage omitted, the rollout finishes and the old baseline can be cleaned up.** The completed version becomes the next canary's baseline. Without an old revision, initial creation still starts the full desired workload.
- Counts round up, keeping at least one old and one new group during a partial canary. At 25%, 4 groups become 3 old + 1 new; 8 groups become 6 old + 2 new. With just 2 groups, any partial percentage leaves 1 old + 1 new.
- A group canary starting at one group stays on the baseline. If both versions already have groups, an Immediate reduction to one instead retains their last pair, avoiding repeated restarts when demand moves between one and two. This costs one extra group per affected role/sub-role per slice and may exceed a zero-surge ceiling by one. Status reports the delayed reduction rather than an exact settled hold. Zero demand and finalization release retention. See [capacity and autoscaling details](../argo-cd/#when-autoscaling-asks-for-one-group).
- Another template update replaces the canary while retaining its baseline. At 50%, an A/B hold becomes A/C: B rolls to C within the canary half, while A's groups remain unchanged at fixed demand and percentage. See the [animated example](../argo-cd/#replace-the-canary-while-keeping-its-baseline).
- **Only Immediate applies demand changes during a partial canary**, independently for `scaleUpDuringRollout` and `scaleDownDuringRollout`. With Immediate in both directions, at 25%, scaling 3 old + 1 new up to 8 gives 6 old + 2 new; scaling back to 4 returns to 3 old + 1 new. Deferred keeps the previously accepted count in its direction, including for zero requests. Promotion and B → C replacement still proceed at that size. Saved counts survive restarts; switching to Immediate applies only the latest request. Readiness, `maxSurge`, and `maxUnavailable` still apply. At 100% or omission, ordinary policies resume, including normal Deferred readiness-paced scale-up.
- Percentage changes only move forward during a partial canary: `25 → 50` is allowed, but `50 → 25` and `25 → 0` are rejected. Start at `0` to hold the baseline before the first promotion. LWS records the oldest revision with surviving groups as the baseline and keeps that choice across controller restarts. This is not a judgment that the baseline is healthy at the application level.
- Starting a partial canary from `100` or an omitted percentage requires `Available=True` for the current generation. Admission rejects a quick `50 → 100 → 25` until finalization actually completes. This also applies when starting the first canary on an existing set; initial creation without history still proceeds normally.
- Restoring the surviving baseline's templates makes it the desired target and converges fully to it, even if the percentage remains set. During a partial canary, baseline LWS templates survive even at zero replicas. Percentage changes alone cannot recreate a deleted revision after finalization at 100% or with the percentage omitted.
- The embedded LWS `partition` field remains unsupported. This control belongs to DisaggregatedSet, not the child LWS rollout.

{{% alert title="Rollback requires restoring the templates" color="danger" %}}
**Restore the stable templates in Git and remove `canary.percent` in the same change.** Removing the percentage alone would finish deploying the new templates. Wait for the rollback to finish, then explicitly set an initial percentage with the next canary release's templates.
{{% /alert %}}

A settled, fully Ready hold reports `Progressing=True` with reason `CanaryHeld`, and its message includes the percentage and observed new/desired counts. For Git-driven promotion, health configuration, and optional manual actions with stock Argo CD, see [Canary Rollouts with Argo CD](../argo-cd/). This is not native Argo Rollouts workload support.

Some requests cannot form a valid canary:

- A positive partial group canary (`1`–`99`) cannot split a one-group role. Keep that whole slice on A and continue scaling it, so it remains a complete serving revision. `Progressing` explains the limitation rather than reporting a successful canary hold. Whole-slice canaries can avoid this limitation when at least two slices exist.
- If another role resumes from zero at one group, keep already-issued canary groups in its peers. For example, retain Prefill A/B while Decode resumes on A only. B's Prefill does not count as serving until B also has Decode; further canary growth waits for Decode to split. This is not a settled hold.
- With Immediate in the corresponding direction, existing roles may scale to and from zero. If every role reaches zero during a canary, keep A and the target's LWS templates at zero; scaling up restores the split rather than promoting the target. In slice mode, role resizing keeps the same selected slices.
- The recorded baseline must still contain the requested role/sub-role templates. Finish renames or role-layout changes before starting another canary. A missing template or ambiguous baseline identity cannot safely be guessed: `Progressing` explains why work is waiting. There is no separate canary-blocked condition.

## What is the fractional window?

The window limits how far one role can get ahead of the others. Its width is **`1 / smallest non-zero role count`**, calculated automatically.

For the old revision, progress is the fraction of its original groups removed. The window moves with the least-advanced role; the others can advance within its width. **It is not a batch size.**

The new revision's growth is coordinated separately, using the fraction of each role's target already requested.

## The window is not an unavailability allowance

Fitting inside the window is not enough: a removal must also respect each role's **`maxUnavailable` budget**.

The new revision counts as replacement serving capacity only once it has a Ready group in **every required role**. Groups still starting do not count as Ready capacity.

## Watch availability during a rollout

The DisaggregatedSet reports two different signals:

- `Available` is **completion**: every role has reached its desired count, with all of it Ready and updated. It is False during every rollout and scale-up, even when nothing has stopped serving.
- `MinimumReplicasAvailable` is **serving capacity**: every role and sub-role has at least its availability floor. It stays True through a rollout or scale-up that remains within each role's `maxUnavailable`. Use it for readiness checks and alerts.

A successful partial canary hold reports `Progressing=True` with reason `CanaryHeld` and **`Available=False`**, even when all requested groups are Ready and serving. Do not use `Available=False` alone as an outage alert during a hold. Monitor `MinimumReplicasAvailable` alongside application health; the replica floor does not check errors, latency, or model output.

During a partial canary, serving capacity follows the demand the controller has admitted, not a queued Deferred request. Requesting zero does not make a still-required role optional; a queued increase from an admitted zero does not reactivate it yet. Status still exposes the latest requested counts so the queued change remains visible.

Each entry in `status.roleStatuses` (and its `subRoleStatuses`) shows the numbers behind it:

- `minAvailableReplicas` is the floor, `desired - maxUnavailable` per slice, totalled across slices. It is unset while the role's target is unknown, and the condition is then `Unknown`, unless another role is already below its floor, which makes it `False`.
- `availableReplicas` counts Ready groups that can actually serve: those in a revision of a slice where **every required role** has a Ready group. A Ready Prefill group in the old revision does not count without a Ready old Decode, even when new Decode groups are Ready.

For example, with `maxUnavailable: 1`, a 4-group Decode role being replaced reports `minAvailableReplicas: 3`. While three old groups still serve, `MinimumReplicasAvailable` stays True; if one more is lost before its replacement is Ready, it becomes False and names the role, for example `decode (2/3)`.

A scale-up is judged against the new target. Raising Decode from 4 to 6 with `maxUnavailable: 1` sets the floor to 5, so the condition is False until one new group is Ready. Use a larger `maxUnavailable`, such as a percentage, if scale-ups should not count as a shortfall.

These checks govern ordinary rollout steps; they cannot prevent unrelated Pod or node failures or guarantee enough GPU capacity. For changing replica counts, see [Scaling during a rollout](../#scaling-during-a-rollout). The [design proposal](https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet#n-dimensional-rolling-update-algorithm) also covers interrupted rollouts and bounded recovery.
