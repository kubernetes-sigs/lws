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

- `maxUnavailable`: Indicates the maximum number (or percentage) of replicas (groups of pods) that are allowed to be unavailable during the update, relative to `spec.replicas` (percentages are rounded down). Defaults to `1`.
- `maxSurge`: Indicates the maximum number (or percentage) of extra replicas that can be created above `spec.replicas` during the update (percentages are rounded up; in `Ordinal` mode, surge is also capped at `spec.replicas`). Defaults to `0`.
- `partition`: Indicates the ordinal at which the LeaderWorkerSet should be partitioned during a rolling update. Replicas with an ordinal greater than or equal to `partition` are updated to the new template, while replicas with an ordinal less than `partition` remain on the previous revision. Defaults to `0`. Only supported when `.spec.groupIdentity` is `Ordinal`.

{{% alert title="Note" color="info" %}}
`maxSurge` and `maxUnavailable` cannot both be zero at the same time.
{{% /alert %}}

## Example Configuration

Here is a LeaderWorkerSet configured with a rolling update strategy (see a full runtime example [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/leaderworkerset/basic/vllm.yaml)):

{{< include file="examples/leaderworkerset/rollout-strategy/rolling-update.yaml" lang="yaml" >}}

## Rolling Update Process

In both [Group Identity](../group-identity/) modes (`Ordinal` and `Hash`), `maxUnavailable` and `maxSurge` operate at the granularity of **complete leader-worker groups**: a group counts as ready and available only when its leader pod and all of its worker pods are `Ready`. However, the underlying rollout mechanism differs depending on `.spec.groupIdentity`.

### `Ordinal` Mode (Default)

In `Ordinal` mode, leader pods are managed by a leader `StatefulSet` (`podManagementPolicy: Parallel`), and the LeaderWorkerSet controller drives the rollout by dynamically coordinating the leader StatefulSet's `.spec.replicas` and `.spec.updateStrategy.rollingUpdate.partition`:

1. **Surge creation**: When `.spec.leaderWorkerTemplate` is updated and `maxSurge > 0`, the controller temporarily increases the leader StatefulSet's `replicas` to `spec.replicas + maxSurge` (creating surge groups at ordinals `spec.replicas` through `spec.replicas + maxSurge - 1`) and initializes the StatefulSet's `partition` to `spec.replicas`.
2. **Partitioned batch progression**: The controller lowers the leader StatefulSet's `partition` from `spec.replicas` down toward `0` (or down to `.spec.rolloutStrategy.rollingUpdateConfiguration.partition`, if specified) in steps bounded by `maxUnavailable` plus the number of active surge groups.
3. **Whole-group readiness check**: Before lowering `partition` to the next batch, the controller waits for all groups from the highest ordinal down to the current `partition` (both leader and worker pods) to be updated to the new revision and `Ready`. When a leader pod at an ordinal `< spec.replicas` is recreated on the new revision, its worker `StatefulSet` is updated to the new revision as well.
4. **Surge reclamation**: As target groups (`0` through `spec.replicas - 1`) finish updating and become ready, once the remaining unready target groups fit within `maxUnavailable`, the controller scales the leader StatefulSet back down to `spec.replicas` to reclaim the temporary surge groups.

Below is a step-by-step trace of how a rolling update executes in `Ordinal` mode for a LeaderWorkerSet with 4 replicas where `maxUnavailable=2` and `maxSurge=2` (effective step size = `maxUnavailable` + `maxSurge` = 4).

Status indicators:
- ✅ Replica has been updated to the new revision
- ❎ Replica has not yet been updated (running old revision)
- ⏳ Replica is currently undergoing rolling update (not yet ready)

| Stage | Partition | Replicas | R-0 | R-1 | R-2 | R-3 | R-4 (Surge) | R-5 (Surge) | Description |
| :--- | :--- | :--- | :---: | :---: | :---: | :---: | :---: | :---: | :--- |
| **Stage 1** | 0 | 4 | ✅ | ✅ | ✅ | ✅ | | | Steady state before rolling update |
| **Stage 2** | 4 | 6 | ❎ | ❎ | ❎ | ❎ | ⏳ | ⏳ | Rolling update starts; 2 surge replicas created |
| **Stage 3** | 2 | 6 | ❎ | ❎ | ⏳ | ⏳ | ⏳ | ⏳ | Partition decreases to 2; R-2 & R-3 begin update |
| **Stage 4** | 2 | 6 | ❎ | ❎ | ⏳ | ⏳ | ✅ | ⏳ | R-4 becomes ready; partition waits for R-5 |
| **Stage 5** | 0 | 6 | ⏳ | ⏳ | ⏳ | ⏳ | ✅ | ✅ | R-5 becomes ready; partition drops to 0; R-0 & R-1 begin update |
| **Stage 6** | 0 | 6 | ⏳ | ⏳ | ✅ | ✅ | ✅ | ✅ | R-2 and R-3 become ready |
| **Stage 7** | 0 | 4 | ⏳ | ⏳ | ✅ | ✅ | | | Scaled down to 4 replicas; surge replicas reclaimed |
| **Stage 8** | 0 | 4 | ⏳ | ✅ | ✅ | ✅ | | | R-1 becomes ready |
| **Stage 9** | 0 | 4 | ✅ | ✅ | ✅ | ✅ | | | R-0 becomes ready; rolling update complete |

### `Hash` Mode

When `.spec.groupIdentity` is set to `Hash`, leader pods are managed by a Kubernetes `Deployment` instead of a `StatefulSet`. Rather than updating existing ordinals in place, each newly created group receives a fresh 5-character hash key (`leaderworkerset.sigs.k8s.io/group-key`) and its own worker `StatefulSet`.

A rolling update in `Hash` mode works as follows:

1. **Deployment RollingUpdate strategy**: `maxUnavailable` and `maxSurge` are mapped directly to the leader Deployment's `.spec.strategy.rollingUpdate` (`RollingUpdateDeployment`). The Deployment controller scales up the new-revision `ReplicaSet` and scales down the old-revision `ReplicaSet` according to those budgets.
   {{% alert title="Note" color="info" %}}
   `partition` is not supported when `.spec.groupIdentity` is `Hash` and must be omitted or set to `0`.
   {{% /alert %}}
2. **Whole-group readiness via the `group-ready` readiness gate**: For multi-pod groups (`.spec.leaderWorkerTemplate.size > 1`), every leader pod is injected with the `leaderworkerset.sigs.k8s.io/group-ready` readiness gate. The pod controller sets this condition to `True` only after the group's worker `StatefulSet` has been created and all `size - 1` worker pods are `Ready`. Because the Deployment controller requires all readiness gates on a leader pod to be `True` before counting that pod as available, `maxUnavailable` and `maxSurge` automatically pace the rollout by **entire groups**—a newly created group does not free budget to terminate another old group until both its leader and all of its workers are ready.
3. **Replacement sequencing via `.spec.groupReplacementPolicy` and scheduling gates**: Every new `Hash`-mode leader pod starts with the `leaderworkerset.sigs.k8s.io/group-replacement` scheduling gate (and a `controller.kubernetes.io/pod-deletion-cost: "-100"` annotation while gated). While a leader pod is `SchedulingGated`, the Kubernetes scheduler ignores it and the controller withholds creating its worker `StatefulSet`:
   - **`PostTermination` (default)**: The controller counts how many old groups are currently tearing down (`T`, where a group is tearing down if its leader pod is terminating or deleted while any leader or worker pod in that group still exists) against the number of gated leader pods (`G`), admitting the oldest `G - T` gated leader pods on the current revision:
     - **Surge groups** created via `maxSurge` do not correspond to a tearing-down group (`G > T`), so their scheduling gate is lifted immediately; they schedule and create their worker StatefulSets right away.
     - **Replacement groups** created when the Deployment scales down old-revision leader pods within `maxUnavailable` stay `SchedulingGated` (with no worker `StatefulSet`) until the terminating old group's leader and worker pods have completely exited and released their node resources.
   - **`Immediate`**: The scheduling gate is lifted on the first reconcile without waiting for tearing-down groups to finish terminating, allowing replacement groups to schedule and create workers concurrently with old group teardown.
4. **Per-revision admission cap**: At most `spec.replicas` groups on any single revision are ungated and admitted at the same time. If `maxSurge` causes the new-revision `ReplicaSet` to create more than `spec.replicas` pods (or if a newer rollout starts before an older revision's gated pods were admitted), any excess pods beyond `spec.replicas` on that revision—as well as any still-gated pods from superseded revisions—remain `SchedulingGated` (carrying `pod-deletion-cost: "-100"` so the ReplicaSet deletes them first when scaling down).

Below is a trace of a rolling update in `Hash` mode (`groupReplacementPolicy: PostTermination`) for a LeaderWorkerSet with 2 replicas where `maxUnavailable=1` and `maxSurge=1`:

| Stage | Old Groups (`rev1`) | New Groups (`rev2`) | Description |
| :--- | :--- | :--- | :--- |
| **Stage 1** | `G-aaa` ✅, `G-bbb` ✅ | *none* | Steady state before rolling update (2 available groups). |
| **Stage 2** | `G-aaa` ✅, `G-bbb` 🗑️ (tearing down) | `G-ccc` ⏳ (surge, ungated)<br>`G-ddd` 🔒 (`SchedulingGated`) | Deployment scales `rev2` ReplicaSet to 2 (`maxSurge=1` + 1 replacement) and terminates `G-bbb` (`maxUnavailable=1`). With `G=2` gated leaders and `T=1` tearing-down group, `G-ccc` (`G - T = 1`) is ungated immediately and creates its workers, while `G-ddd` waits for `G-bbb` (leader + workers) to finish terminating. |
| **Stage 3** | `G-aaa` ✅ | `G-ccc` ⏳, `G-ddd` ⏳ (ungated) | All pods of `G-bbb` finish terminating (`T=0`). `G-ddd` has its scheduling gate removed, schedules, and creates its worker StatefulSet. |
| **Stage 4** | `G-aaa` 🗑️ (tearing down) | `G-ccc` ✅, `G-ddd` ⏳ | `G-ccc` (leader + all workers) becomes ready (`group-ready=True`), bringing available groups back to 2 (`G-aaa` + `G-ccc`). The Deployment terminates the remaining old group `G-aaa`. |
| **Stage 5** | *none* | `G-ccc` ✅, `G-ddd` ✅ | `G-aaa` finishes terminating and `G-ddd` (leader + all workers) becomes ready. Rolling update complete with 2 groups on `rev2`. |

## Combined Template Update and Scale-Up

When `.spec.leaderWorkerTemplate` and `.spec.replicas` (scaling up from `oldReplicas` to `newReplicas`) are updated at the same time—or when `spec.replicas` is scaled up while a rolling update is already in progress—both `Ordinal` and `Hash` modes follow two core principles:

1. **New capacity is always created on the new revision**: The `newReplicas - oldReplicas` newly added groups are created directly with the updated template rather than scaling up on the old revision first.
2. **Unready scaled-up groups consume the `maxUnavailable` budget**: Because `maxUnavailable` is evaluated relative to the new target `newReplicas`, the `newReplicas - oldReplicas` new groups count as unavailable until their leader and worker pods are fully ready:
   - If **`newReplicas - oldReplicas >= maxUnavailable`**, **no existing old-revision groups are disrupted initially**. All `oldReplicas` existing groups continue serving traffic on the old revision until enough newly added groups become ready to free room within `maxUnavailable`.
   - If **`newReplicas - oldReplicas < maxUnavailable`**, all `newReplicas - oldReplicas` new groups start on the new revision immediately, and up to `maxUnavailable - (newReplicas - oldReplicas)` existing old-revision groups begin updating at the same time using the remaining unavailability budget.

The exact mechanics in each mode are detailed below.

### Combined Update and Scale-Up in `Ordinal` Mode

When a template update and a scale-up from `oldReplicas` to `newReplicas` occur together in `Ordinal` mode:

1. **Initial reconcile — scale up on the new revision while holding `partition` at `oldReplicas`**:
   - On the reconcile that detects the spec change (`newReplicas > oldReplicas`), the controller sets the leader StatefulSet's `partition` to `oldReplicas` and sets its `replicas` to `newReplicas` (deferring any `maxSurge` replicas until the next reconcile).
   - Because the newly added ordinals `oldReplicas .. newReplicas - 1` have ordinals `>= partition`, the StatefulSet immediately creates them on the **new** revision.
   - Existing groups at ordinals `0 .. oldReplicas - 1` have ordinals `< partition`, so they remain running on the **old** revision without disruption.
2. **Subsequent reconcile — apply `maxSurge` (if configured)**:
   - Once the leader StatefulSet reflects `newReplicas`, if `maxSurge > 0`, the controller scales the leader StatefulSet up to `newReplicas + maxSurge` (with percentage `maxSurge` calculated against `newReplicas`), creating temporary surge groups at ordinals `newReplicas .. newReplicas + maxSurge - 1` on the new revision.
3. **Advancing `partition` into existing groups (`0 .. oldReplicas - 1`)**:
   - The controller computes the next partition as `newReplicas - continuousReadyTailReplicas - maxUnavailable` (clamped so `partition` never increases), where `continuousReadyTailReplicas` is the number of contiguous updated and ready groups counting downward from the highest ordinal (`newReplicas + activeSurge - 1`):
     - **When `newReplicas - oldReplicas >= maxUnavailable`**: Because `newReplicas - maxUnavailable >= oldReplicas`, `partition` stays clamped at `oldReplicas` while the new tail groups are starting up. Existing groups (`0 .. oldReplicas - 1`) are not rolled until enough of the newly scaled-up (and surge) tail ordinals become continuously ready so that `newReplicas - continuousReadyTailReplicas - maxUnavailable < oldReplicas`.
     - **When `newReplicas - oldReplicas < maxUnavailable`**: On the reconcile after the scale-up is applied, `newReplicas - maxUnavailable < oldReplicas`, so `partition` drops immediately to `newReplicas - maxUnavailable`. This starts updating `maxUnavailable - (newReplicas - oldReplicas)` existing groups at ordinals `(newReplicas - maxUnavailable) .. (oldReplicas - 1)` alongside the new scale-up groups, while ordinals `< newReplicas - maxUnavailable` wait for the tail ordinals to become ready.
   - **Scaling up mid-rollout**: If a rollout is already underway (`partition < oldReplicas`) when `spec.replicas` is increased to `newReplicas`, `partition` is held at its current value while ordinals `oldReplicas .. newReplicas - 1` (plus any surge ordinals) are created on the new revision. `partition` does not advance lower until all new tail ordinals (`oldReplicas .. newReplicas - 1`) and the in-flight ordinals (`partition .. oldReplicas - 1`) are continuously ready.

### Combined Update and Scale-Up in `Hash` Mode

When a template update and a scale-up from `oldReplicas` to `newReplicas` occur together in `Hash` mode:

1. **Single Deployment update**:
   - The controller updates the leader Deployment's `.spec.replicas` to `newReplicas` and `.spec.template` to the new revision in a single apply (with percentage `maxUnavailable` and `maxSurge` evaluated by the Deployment controller against `newReplicas`).
2. **New capacity and surge on the new ReplicaSet**:
   - The Deployment controller never scales up the old-revision `ReplicaSet`. Instead, it creates (or scales up) the new-revision `ReplicaSet` to add the `newReplicas - oldReplicas` scaled-up groups plus up to `maxSurge` surge groups (up to `newReplicas + maxSurge` total leader pods across old and new ReplicaSets).
3. **How existing old-revision groups are rolled**:
   - The Deployment controller requires at least `minAvailable = newReplicas - maxUnavailable` available groups (`group-ready = True`) across all ReplicaSets before scaling down old pods:
     - **When `newReplicas - oldReplicas >= maxUnavailable`**: At most `oldReplicas` groups are currently available, which is already `<= newReplicas - maxUnavailable`. Consequently, the Deployment controller **keeps all `oldReplicas` old-revision groups running** and only begins terminating old groups after enough new-revision groups become fully ready (`group-ready = True`, meaning leader + all workers ready) to push total available groups above `newReplicas - maxUnavailable`.
     - **When `newReplicas - oldReplicas < maxUnavailable`**: If all `oldReplicas` existing groups are ready, available groups exceed `newReplicas - maxUnavailable` by `maxUnavailable - (newReplicas - oldReplicas)`. The Deployment controller immediately terminates up to `maxUnavailable - (newReplicas - oldReplicas)` old-revision groups while creating the `newReplicas - oldReplicas` scale-up groups and replacement/surge groups on the new ReplicaSet.
4. **Scheduling gate and admission behavior**:
   - Under `groupReplacementPolicy: PostTermination`, the `newReplicas - oldReplicas` scaled-up groups (and any `maxSurge` groups) do not wait on any tearing-down group (`G > T`), so their `leaderworkerset.sigs.k8s.io/group-replacement` scheduling gate is lifted immediately in creation order and they begin creating their worker StatefulSets right away. Any additional new-revision leader pods created to replace terminated old-revision groups wait in `SchedulingGated` until the terminated old groups' leader and worker pods are completely removed.
   - Because `Hash` mode caps admitted (ungated) groups on a single revision at `spec.replicas` (`newReplicas`), at most `newReplicas` groups on the new revision can be ungated and scheduled concurrently; if `maxSurge > 0` causes the new-revision `ReplicaSet` to hold more than `newReplicas` pods at once, any pods beyond `newReplicas` stay `SchedulingGated` and are removed first when the `ReplicaSet` scales down to `newReplicas`.

| Aspect during Combined Template Update + Scale-Up | `Ordinal` Mode | `Hash` Mode |
| :--- | :--- | :--- |
| **Where new capacity (`newReplicas - oldReplicas`) is created** | New ordinals `oldReplicas .. newReplicas - 1` on the **new** revision (`partition` starts at `oldReplicas`). | New-revision `ReplicaSet` on the leader `Deployment` (with fresh group hashes). |
| **When `newReplicas - oldReplicas >= maxUnavailable`** | `partition` stays clamped at `oldReplicas`; no existing group (`0 .. oldReplicas - 1`) is updated until enough new tail groups (and surge groups) are continuously ready. | Available groups (`<= oldReplicas`) are `<= newReplicas - maxUnavailable`; no old-revision group is terminated until enough new-revision groups reach `group-ready = True`. |
| **When `newReplicas - oldReplicas < maxUnavailable`** | `partition` drops to `newReplicas - maxUnavailable` on the next reconcile, rolling `maxUnavailable - (newReplicas - oldReplicas)` existing ordinals while the rest wait for tail ordinals to become ready. | Deployment immediately scales down the old `ReplicaSet` by up to `maxUnavailable - (newReplicas - oldReplicas)` groups while scaling up the new `ReplicaSet`. |
| **`maxSurge` behavior** | Added on the second reconcile at ordinals `newReplicas .. newReplicas + maxSurge - 1` and reclaimed once remaining unready target ordinals fit within `maxUnavailable`. | Created immediately on the new `ReplicaSet` (up to `newReplicas + maxSurge` total pods across ReplicaSets); admission on the new revision is capped at `newReplicas` ungated groups. |
| **Unblocking constrained-capacity scale-up via `maxUnavailable` ([#717](https://github.com/kubernetes-sigs/lws/issues/717))** | Requires `maxUnavailable: 100%` (or `newReplicas`) when multiple new ordinals are `Pending`, because `partition` only advances as tail ordinals become contiguously ready from `newReplicas - 1` downward. | Setting `maxUnavailable > newReplicas - oldReplicas` (for example, `newReplicas - oldReplicas + 1`) is sufficient to cascade the rollout, because any newly ready group increments Deployment availability regardless of order. |

### Resource Deadlock When Reducing Per-Replica Resources During Scale-Up ([#717](https://github.com/kubernetes-sigs/lws/issues/717))

A common operational scenario on capacity-constrained clusters (such as fixed GPU reservations) is **reducing per-group resource requests while scaling up `.spec.replicas` in the same update** so that total resource usage stays within the same cluster capacity.

For example, consider a cluster with **8 GPUs total** (or 4 nodes with 8 GPUs each = 32 GPUs):
- **Initial state**: `replicas: 1` (or `4`), with each group requesting **8 GPUs** (consuming all available GPUs).
- **Target state**: `replicas: 2` (or `8`), with each group updated to request **4 GPUs**, and default rollout settings (`maxUnavailable: 1`, `maxSurge: 0`).

Even though the final desired state (`2 × 4 = 8 GPUs`, or `8 × 4 = 32 GPUs`) fits within the cluster's capacity, applying both changes at the same time with `newReplicas - oldReplicas >= maxUnavailable` causes a **resource deadlock in both `Ordinal` and `Hash` modes**:

- **In `Ordinal` mode**: The controller sets `partition = oldReplicas` (`1`) and scales the leader `StatefulSet` to `newReplicas` (`2`). Ordinal `R-1` is created with the new `4`-GPU template, while `R-0` (`< partition`) remains running on the old `8`-GPU template. Because `R-0` still occupies all 8 GPUs, `R-1` stays `Pending` (`Unschedulable`). Because `R-1` never becomes `Ready`, `partition` never drops to `0` to replace `R-0` and release its GPUs.
- **In `Hash` mode**: The leader `Deployment` scales the new-revision `ReplicaSet` up while requiring `minAvailable = newReplicas - maxUnavailable` (`2 - 1 = 1`) available groups before scaling down the old `ReplicaSet`. Because only the existing `8`-GPU group is available (`1 <= minAvailable`), the Deployment cannot terminate the old group until a new `4`-GPU group reaches `group-ready = True`—which never happens because the old group is holding all 8 GPUs.

#### How to Avoid or Resolve This Deadlock

1. **Perform the update in two steps (recommended for both `Ordinal` and `Hash` modes)**:
   - **Step 1**: Update `.spec.leaderWorkerTemplate` to reduce the per-group resource request (for example, from `8` GPUs to `4` GPUs) while keeping `.spec.replicas` at `oldReplicas`. With `maxUnavailable: 1`, existing groups roll one by one to the smaller footprint and free cluster capacity.
   - **Step 2**: Once the template rollout completes (or once enough groups have updated to free capacity), scale `.spec.replicas` up to `newReplicas`.
2. **Increase `maxUnavailable` above the scale-up delta (`maxUnavailable > newReplicas - oldReplicas`)**:
   - **In `Hash` mode**: Setting `maxUnavailable` to at least `(newReplicas - oldReplicas) + 1` (for example, `maxUnavailable: 2` when scaling `1 -> 2`, or `maxUnavailable: 5` when scaling `4 -> 8`) allows the Deployment to immediately terminate `maxUnavailable - (newReplicas - oldReplicas)` old `8`-GPU groups. Under `groupReplacementPolicy: PostTermination`, once one old `8`-GPU group finishes terminating, its freed 8 GPUs allow **two** `4`-GPU groups on the new `ReplicaSet` (`1` replacement + `1` scaled-up group) to schedule and reach `group-ready = True`. Because the Deployment counts any ready group toward `minAvailable` regardless of creation order, that net increase in available groups immediately frees budget to terminate the next old `8`-GPU group, cascading until all `newReplicas` groups are running on the new revision.
   - **In `Ordinal` mode**: For a `1 -> 2` transition, setting `maxUnavailable: 2` (or `100%`) drops `partition` to `newReplicas - maxUnavailable = 0` on the second reconcile, terminating `R-0` so both `R-0` and `R-1` can schedule with 4 GPUs each. However, for larger multi-replica transitions (such as `4 × 8 GPUs -> 8 × 4 GPUs`), `Ordinal` mode only lowers `partition` below `newReplicas - maxUnavailable` when tail ordinals are **contiguously ready from the highest ordinal (`newReplicas - 1`) downward**. When one old `8`-GPU group (`R-3`) is replaced by a `4`-GPU `R-3`, only one of the pending tail ordinals (`R-4 .. R-7`) can fit onto the remaining 4 GPUs of that node (typically `R-4`), leaving `R-7` still `Pending` and keeping `continuousReadyTailReplicas` at `0`. Consequently, single-step recovery in `Ordinal` mode when multiple tail ordinals are `Pending` requires setting `maxUnavailable` to `newReplicas` (`100%`) so `partition` drops all the way to `0`.

## MaxUnavailable Feature Gate

`MaxUnavailable` for StatefulSets graduated to Beta in Kubernetes [1.35](https://kubernetes.io/blog/2025/12/17/kubernetes-v1-35-release/#maxunavailable-for-statefulsets), meaning it is enabled by default in supported Kubernetes clusters. This feature gate applies to `Ordinal` mode (which relies on a leader `StatefulSet`) and to worker StatefulSets; in `Hash` mode, leader rollout availability is managed by the leader `Deployment`.
