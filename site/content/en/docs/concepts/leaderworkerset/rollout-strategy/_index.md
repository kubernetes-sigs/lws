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

When `.spec.leaderWorkerTemplate` and `.spec.replicas` (scaling up from `oldReplicas` to `newReplicas`) are updated together—or when `spec.replicas` is scaled up mid-rollout—both modes follow the same rules:

1. **Added capacity comes up on the new revision**:
   - In `Ordinal` mode, the controller sets `partition = oldReplicas` while scaling to `newReplicas`, so new ordinals (`oldReplicas .. newReplicas - 1`) are created directly with the new template.
   - In `Hash` mode, although the Deployment controller briefly scales the old `ReplicaSet` before rolling to the new `ReplicaSet`, gating prevents outdated leader pods from ever scheduling or creating workers; they are immediately deleted as the new `ReplicaSet` scales up.
2. **Unready scaled-up groups count against `maxUnavailable`** (evaluated relative to `newReplicas`):
   - **If `newReplicas - oldReplicas >= maxUnavailable`**: No existing old-revision groups are disrupted initially. Existing groups keep running until enough new groups become ready to free room within `maxUnavailable`.
   - **If `newReplicas - oldReplicas < maxUnavailable`**: Up to `maxUnavailable - (newReplicas - oldReplicas)` existing old-revision groups begin updating immediately alongside the newly added groups.

### Resource Deadlock When Reducing Per-Group Resources During Scale-Up ([#717](https://github.com/kubernetes-sigs/lws/issues/717))

On capacity-constrained clusters (for example, a fixed pool of 8 GPUs), a common operation is **halving per-group resources while doubling `spec.replicas` in a single update** (such as `1 × 8 GPUs -> 2 × 4 GPUs`, or `4 × 8 GPUs -> 8 × 4 GPUs`) with default rollout settings (`maxUnavailable: 1`, `maxSurge: 0`).

Because `newReplicas - oldReplicas >= maxUnavailable`, this update **deadlocks in both modes**: the controller refuses to terminate any old `8`-GPU group until a new `4`-GPU group becomes ready, but the new `4`-GPU groups stay `Pending` because the existing `8`-GPU groups still occupy all cluster GPUs.

To avoid or recover from this state:

1. **Update in two steps (recommended)**:
   - First update `.spec.leaderWorkerTemplate` to the smaller per-group resource request while keeping `spec.replicas` unchanged. Once existing groups roll and free capacity, scale `spec.replicas` up.
2. **Increase `maxUnavailable` above the scale-up delta (`maxUnavailable > newReplicas - oldReplicas`)**:
   - **In `Hash` mode**: Setting `maxUnavailable` to at least `(newReplicas - oldReplicas) + 1` (e.g. `2` for `1 -> 2`, or `5` for `4 -> 8`) lets the Deployment immediately terminate one old `8`-GPU group. Once it exits, two new `4`-GPU groups fit into the freed capacity, and because any ready group increases Deployment availability regardless of order, the remaining old groups cascade through the rollout.
   - **In `Ordinal` mode**: Because `partition` only steps down as tail ordinals become contiguously ready from the highest ordinal (`newReplicas - 1`) downward, freeing capacity for a lower pending ordinal (such as `R-4` while `R-7` stays `Pending`) does not advance `partition`. When multiple scaled-up ordinals are `Pending`, single-step recovery requires setting `maxUnavailable: 100%` (or `newReplicas`) so `partition` drops to `0` immediately.

## MaxUnavailable Feature Gate

`MaxUnavailable` for StatefulSets graduated to Beta in Kubernetes [1.35](https://kubernetes.io/blog/2025/12/17/kubernetes-v1-35-release/#maxunavailable-for-statefulsets), meaning it is enabled by default in supported Kubernetes clusters. This feature gate applies to `Ordinal` mode (which uses a leader `StatefulSet`) and to worker StatefulSets; in `Hash` mode, leader rollout availability is managed by the leader `Deployment`.
