---
title: "Rollout Strategy"
linkTitle: "Rollout Strategy"
weight: 60
description: >
  Rolling update configurations, maxUnavailable, and maxSurge in LeaderWorkerSet.
aliases:
- /docs/concepts/rollout-strategy/
---

Rolling update is vital to online services requiring high availability and zero downtime. For LLM inference services, this is particularly important to mitigate stockout and maintain serving capacity during updates.

LeaderWorkerSet supports three parameters within `.spec.rolloutStrategy.rollingUpdateConfiguration`:

- `maxUnavailable`: Maximum number (or percentage) of replicas (groups of pods) allowed to be unavailable during the update, relative to `spec.replicas` (percentages are rounded down). Defaults to `1`.
- `maxSurge`: Maximum number (or percentage) of extra replicas that can be created above `spec.replicas` during the update (percentages are rounded up; in `Ordinal` mode, surge is capped at `spec.replicas`). Defaults to `0`.
- `partition`: Lowest ordinal updated to the new template during a rolling update. Replicas with `ordinal >= partition` receive the new template, while replicas with `ordinal < partition` stay on the previous revision. Defaults to `0`. Only supported when `.spec.groupIdentity` is `Ordinal`.

{{% alert title="Note" color="info" %}}
`maxSurge` and `maxUnavailable` cannot both be zero at the same time.
{{% /alert %}}

## Example Configuration

Here is a LeaderWorkerSet configured with a rolling update strategy (see a full runtime example [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/leaderworkerset/basic/vllm.yaml)):

{{< include file="examples/leaderworkerset/rollout-strategy/rolling-update.yaml" lang="yaml" >}}

## Rolling Update Process

In both [Group Identity](../group-identity/) modes (`Ordinal` and `Hash`), `maxUnavailable` and `maxSurge` operate on **complete leader-worker groups**: a group counts as ready only when its leader pod and all of its worker pods are `Ready`.

### `Ordinal` Mode (Default)

In `Ordinal` mode, leader pods are managed by a `StatefulSet`. The controller drives the rollout by coordinating the leader StatefulSet's `replicas` and `partition` from highest ordinal to lowest:

1. **Surge creation**: If `maxSurge > 0`, the controller temporarily scales the leader StatefulSet up to `spec.replicas + maxSurge` and sets `partition` to `spec.replicas`.
2. **Batch progression**: The controller lowers `partition` toward `0` (or `.spec.rolloutStrategy.rollingUpdateConfiguration.partition`) in steps bounded by `maxUnavailable` and active surge groups, waiting for higher ordinals (leader and workers) to be updated and `Ready` before advancing.
3. **Surge reclamation**: Once the remaining unready target groups (`0` through `spec.replicas - 1`) fit within `maxUnavailable`, the controller scales the leader StatefulSet back down to `spec.replicas`.

Below is a step-by-step trace for 4 replicas with `maxUnavailable=2` and `maxSurge=2` (step size = `maxUnavailable` + `maxSurge` = 4).

Status indicators:
- ✅ Replica has been updated to the new revision
- ❎ Replica has not yet been updated (running old revision)
- ⏳ Replica is currently undergoing rolling update (not yet ready)

| Stage | Partition | Replicas | R-0 | R-1 | R-2 | R-3 | R-4 (Surge) | R-5 (Surge) | Description |
| :--- | :--- | :--- | :---: | :---: | :---: | :---: | :---: | :---: | :--- |
| **Stage 1** | 0 | 4 | ✅ | ✅ | ✅ | ✅ | | | Before the rolling update |
| **Stage 2** | 4 | 6 | ❎ | ❎ | ❎ | ❎ | ⏳ | ⏳ | Rolling update starts; 2 surge replicas created |
| **Stage 3** | 2 | 6 | ❎ | ❎ | ⏳ | ⏳ | ⏳ | ⏳ | Partition decreases to 2; R-2 & R-3 begin update |
| **Stage 4** | 2 | 6 | ❎ | ❎ | ⏳ | ⏳ | ✅ | ⏳ | R-4 becomes ready; partition waits for R-5 |
| **Stage 5** | 0 | 6 | ⏳ | ⏳ | ⏳ | ⏳ | ✅ | ✅ | R-5 becomes ready; partition drops to 0; R-0 & R-1 begin update |
| **Stage 6** | 0 | 6 | ⏳ | ⏳ | ✅ | ✅ | ✅ | ✅ | R-2 and R-3 become ready |
| **Stage 7** | 0 | 4 | ⏳ | ⏳ | ✅ | ✅ | | | Scaled down to 4 replicas; surge replicas reclaimed |
| **Stage 8** | 0 | 4 | ⏳ | ✅ | ✅ | ✅ | | | R-1 becomes ready |
| **Stage 9** | 0 | 4 | ✅ | ✅ | ✅ | ✅ | | | R-0 becomes ready; rolling update complete |

### `Hash` Mode

When `.spec.groupIdentity` is `Hash`, leader pods are managed by a `Deployment` instead of a `StatefulSet`, and each new group gets a fresh random identity rather than updating an ordinal in place (see [Group Identity](../group-identity/) for details):

1. **Deployment-driven rollout**: `maxUnavailable` and `maxSurge` are passed directly to the leader Deployment's `RollingUpdate` strategy, which scales up the new `ReplicaSet` and scales down the old `ReplicaSet`. (`partition` is not supported in `Hash` mode.)
2. **Whole-group readiness**: Multi-pod groups use the `leaderworkerset.sigs.k8s.io/group-ready` readiness gate on the leader pod, which becomes `True` only after all workers in the group are `Ready`. The Deployment therefore counts a leader as available—and frees budget to terminate another old group—only when its entire group is ready.
3. **Replacement pacing**: New leader pods start with the `leaderworkerset.sigs.k8s.io/group-replacement` scheduling gate:
   - With `groupReplacementPolicy: PostTermination` (default), surge groups are ungated immediately, while replacement groups stay `SchedulingGated` (without creating workers) until the terminating old group's leader and worker pods are fully removed.
   - With `groupReplacementPolicy: Immediate`, replacement groups are ungated immediately without waiting for old groups to finish terminating.
   - At most `spec.replicas` groups on a given revision are ungated at the same time; any extra pods created by `maxSurge` beyond `spec.replicas` stay gated and are removed first when scaling down.

Below is a trace in `Hash` mode (`groupReplacementPolicy: PostTermination`) for 2 replicas with `maxUnavailable=1` and `maxSurge=1`:

| Stage | Old Groups (`rev1`) | New Groups (`rev2`) | Description |
| :--- | :--- | :--- | :--- |
| **Stage 1** | `G-aaa` ✅, `G-bbb` ✅ | *none* | Steady state before rolling update (2 available groups). |
| **Stage 2** | `G-aaa` ✅, `G-bbb` 🗑️ (tearing down) | `G-ccc` ⏳ (surge, ungated)<br>`G-ddd` 🔒 (`SchedulingGated`) | Deployment creates `G-ccc` (surge) and `G-ddd` (replacement) on `rev2` and terminates `G-bbb`. `G-ccc` is ungated immediately; `G-ddd` waits for `G-bbb` (leader + workers) to finish terminating. |
| **Stage 3** | `G-aaa` ✅ | `G-ccc` ⏳, `G-ddd` ⏳ (ungated) | `G-bbb` finishes terminating. `G-ddd` is ungated and creates its workers. |
| **Stage 4** | `G-aaa` 🗑️ (tearing down) | `G-ccc` ✅, `G-ddd` ⏳ | `G-ccc` becomes ready, restoring 2 available groups (`G-aaa` + `G-ccc`). The Deployment terminates `G-aaa`. |
| **Stage 5** | *none* | `G-ccc` ✅, `G-ddd` ✅ | `G-aaa` finishes terminating and `G-ddd` becomes ready. Rollout complete on `rev2`. |

## Combined Template Update and Scale-Up

When one update changes `.spec.leaderWorkerTemplate` and increases `.spec.replicas` from `baseline`, or increases `spec.replicas` during an active rollout, both modes follow the same rules:

1. **Added capacity starts on the new revision**:
   - In `Ordinal` mode, the controller sets `partition = baseline` and scales to `spec.replicas` plus any `maxSurge`. New ordinals (`baseline .. spec.replicas - 1`) start directly with the new template.
   - In `Hash` mode, `group-replacement` scheduling gates keep outdated leader pods unscheduled without workers while the new `ReplicaSet` adds pods.
2. **Availability-floor pacing**:
   - The controller paces disruption against the non-surge floor `max(0, min(baseline, spec.replicas) - maxUnavailable)`. For this floor, a percentage `maxUnavailable` resolves against `min(baseline, spec.replicas)`, so a large scale-up does not widen disruption on existing groups. Elsewhere, percentage `maxUnavailable` and `maxSurge` resolve against desired `spec.replicas`:
     - **Capacity-constrained reconfiguration (`maxUnavailable >= 1`)**: `Pending` additions do not consume the `maxUnavailable` budget of existing groups. For example, an update from `1 × 8 GPUs` to `2 × 4 GPUs` with `maxUnavailable: 1, maxSurge: 0` replaces the old group immediately. The two `4`-GPU groups then schedule into the freed capacity.
     - **Zero-downtime growth (`maxUnavailable: 0, maxSurge > 0`)**: With `maxUnavailable: 0`, the floor equals `baseline`, so the controller disrupts no existing group while new groups start. Each added or surge group that reaches whole-group `Ready` (leader and all workers) grants credit to replace one old group. At least `baseline` `Ready` groups stay available throughout the rollout.
     - **Superseding updates and scale-down**: A superseding template update raises `baseline` to the current count of whole-group `Ready` groups, up to `spec.replicas`. This update protects `Ready` additions and ignores `Pending` additions. The rollout stays active until every group runs the target revision (and, with a user `partition`, groups at or above it are `Ready`). A scale-down clamps the floor to `max(0, min(baseline, spec.replicas) - maxUnavailable)`. In `Ordinal` mode, a scale-down during an active rollout can keep `Ready` extra ordinals and raise `partition` to hold the floor. A scale-down with no active rollout removes excess groups immediately, even when some remaining groups are not `Ready`.

## MaxUnavailable Feature Gate

In `Ordinal` mode, the controller sets `maxUnavailable` on the leader `StatefulSet`. The StatefulSet controller applies that field only when the Kubernetes [`MaxUnavailableStatefulSet`](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#maximum-unavailable-pods) feature gate is enabled. The default of the gate depends on the Kubernetes version:

| Kubernetes version | `MaxUnavailableStatefulSet` default |
| :--- | :--- |
| 1.34 | Disabled (Alpha) |
| 1.35.0 through 1.35.3 | Enabled (Beta) |
| 1.35.4 and later 1.35 patches, 1.36 | Disabled (Beta, see [kubernetes#137904](https://github.com/kubernetes/kubernetes/pull/137904)) |
| 1.37 and later | Enabled (Beta) |

LeaderWorkerSet does not need the feature gate. A rolling update replaces up to `maxUnavailable` groups at a time with the gate enabled or disabled:

- **Gate enabled**: the StatefulSet controller replaces the outdated leaders.
- **Gate disabled**: the StatefulSet controller replaces one leader at a time and waits at a leader that is not `Ready`. The LeaderWorkerSet controller then deletes the next outdated leader pods itself, as far as `maxUnavailable` and the availability floor allow. It records a `GroupsUpdating` event on the LeaderWorkerSet for each leader pod that it deletes.

The controller does not set `maxUnavailable` on worker StatefulSets. In `Hash` mode, the leader `Deployment` manages rollout availability and the feature gate has no effect.
