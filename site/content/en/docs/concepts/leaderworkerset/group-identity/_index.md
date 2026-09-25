---
title: "Group Identity"
linkTitle: "Group Identity"
weight: 80
description: >
  Ordinal and Hash group identity modes, and when to use each.
---

Group identity controls how LeaderWorkerSet names its groups and which core controller manages the leader pods. It is set once at creation through `.spec.groupIdentity` and cannot change afterward.

Two modes are supported:

- **`Ordinal`** (default): leader pods are managed by a leader StatefulSet and groups are named by StatefulSet ordinals (`0`, `1`, ..., `R-1`). This is the behavior described in [Architecture](../).
- **`Hash`**: leader pods are managed by a Deployment and each group is named by a random key. A group's identity is not reused when the group is replaced.

{{% alert title="Note" color="info" %}}
`groupIdentity` is immutable. To move a workload between modes, recreate the LeaderWorkerSet. Within a [DisaggregatedSet](../../disaggregatedset/) a role can change modes, which rolls the affected LeaderWorkerSets as a normal template update.
{{% /alert %}}

## When to Use Hash Mode

Ordinal identity numbers the live groups `0` through `replicas-1`, so scale down can only remove the highest ordinal. If there's already an unhealthy group, scale down cannot target it, so it deletes a healthy group and leaves the unhealthy one in place to be rebuilt.

Hash mode makes groups interchangeable. Leaders run under a Deployment, whose ReplicaSet removes unscheduled and not-ready groups first, so scale down sheds the unhealthy groups and spares the healthy ones.

Serving workloads behind an autoscaler usually want this. Workloads that rely on stable ordinal identity or predictable DNS, such as distributed training, should keep the default.

## How Hash Mode Works

The controller owns a Deployment named after the LeaderWorkerSet instead of a leader StatefulSet, with `maxSurge` and `maxUnavailable` mapped onto the Deployment strategy.

Each leader carries a `leaderworkerset.sigs.k8s.io/group-ready` readiness gate, set ready only once the group's worker StatefulSet is ready. So a leader is ready only when its whole group is, which is what makes the ReplicaSet rank scale-down victims on group health.

The pod webhook assigns each leader a random group key at creation, stored on the leader and inherited by its workers. A random input is needed because leaders are created through `generateName` and have no name yet at admission. Each worker StatefulSet is named after its leader's host name, so admission can derive worker addresses before the leader pod has a name. The webhook gives the leader a DNS record under the headless service, published even while not-ready so workers can resolve it through `LWS_LEADER_ADDRESS`. Groups of size 1 get no readiness gate and no worker StatefulSet. [Exclusive placement](../topology-placement/) keys off the group key exactly as in Ordinal mode.

{{% alert title="Note" color="warning" %}}
A recreated group gets a new key, so its group index label and leader DNS name change. Treat group identity as ephemeral and do not cache the leader address across a replacement.
{{% /alert %}}

## Group Replacement

Delegating leaders to a Deployment also changes what happens when a group is replaced. `.spec.groupReplacementPolicy` controls it:

- **`PostTermination`** (default): a replacement group waits until the group it replaces is fully removed before it schedules, matching Ordinal mode.
- **`Immediate`**: a replacement group schedules as soon as it is created, reproducing plain ReplicaSet behavior. Hash mode only.

In Ordinal mode a replacement cannot start before its predecessor is gone, so it lands on the freed capacity. A ReplicaSet has no such constraint: it creates the replacement immediately, and on a full cluster the new group preempts unrelated workloads instead of waiting for its predecessor's capacity. `PostTermination` restores the Ordinal sequencing with a scheduling gate:

1. The pod webhook gates every Hash-mode leader at admission, so the scheduler ignores it and it triggers no preemption.
2. While gated, the controller creates nothing for the group: no worker StatefulSet, no service, no pods.
3. With `T` groups tearing down and `G` gated leaders ordered by creation time, the controller admits the oldest `G - T`. A group is tearing down while any of its pods still exists but its leader is terminating or gone. Each removed group admits one replacement, and gated leaders with no tearing-down group behind them, such as a scale up or `maxSurge` pod, are admitted at once.

The count keys off the old group's pods, not the leader's phase or object. A pod is removed only after the kubelet frees its resources, which is when the scheduler counts that capacity as free. The leader object alone is unreliable: on a rollout or scale down the ReplicaSet deletes it in the background, so it can vanish seconds before its workers release capacity.

{{% alert title="Note" color="info" %}}
`groupReplacementPolicy` is a live setting, not part of the LeaderWorkerSet revision, so switching it takes effect on the next reconcile without rolling any group.
{{% /alert %}}

A waiting replacement is visible as a `SchedulingGated` pod and counts as a not-ready group in `status.replicas`.

## Unsupported Combinations

Validation rejects Hash mode combined with features that depend on stable StatefulSet identity:

- **[Volume claim templates](../volume-claim-templates/):** persistent storage is meant to reattach to a successor with the same identity, which Hash mode never reuses. Use generic ephemeral volumes for per-group scratch storage.
- **`rollingUpdateConfiguration.partition`:** partition is defined over ordinals, which a Deployment has no equivalent for.

`groupReplacementPolicy: Immediate` is also rejected with `groupIdentity: Ordinal`, where a StatefulSet always waits for the previous leader pod to be gone.
