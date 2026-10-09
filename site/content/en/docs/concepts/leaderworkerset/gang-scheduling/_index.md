---
title: "Gang Scheduling"
linkTitle: "Gang Scheduling"
weight: 35
description: >
  Native all-or-nothing scheduling for LeaderWorkerSet replicas.
---

Native gang scheduling uses the Kubernetes Workload-Aware Scheduling APIs to
admit related pods as a unit. LWS owns the `Workload` and `PodGroup` objects
and sets each managed pod's `spec.schedulingGroup`; users express the intent
only on the LeaderWorkerSet.

Gang scheduling is opt-in. A LeaderWorkerSet without `spec.scheduling` keeps
the normal pod-by-pod scheduling behavior and LWS creates no native scheduling
objects.

## Scheduling levels

LWS supports three mutually exclusive scheduling levels in `spec.scheduling`:

- **Replica level (`spec.scheduling.replica`, or `spec.scheduling: {}`)**:
  One `PodGroup` per replica covering its leader and workers together (`size`
  pods).
- **Whole-LWS level**: Set a top-level `schedulingPolicy`,
  `schedulingConstraints`, or `disruptionMode` under `spec.scheduling`, for
  example `spec.scheduling.schedulingPolicy.gang: {}`. This creates one
  `PodGroup` for the entire LeaderWorkerSet (`replicas * size` pods across all
  replicas). While a rollout increases `size`, its `gang.minCount` counts the
  replicas that the rollout has not replaced yet with their previous size.
- **Role level (`spec.scheduling.replica.leader` or
  `spec.scheduling.replica.worker`)**: Setting either role selects this level.
  LWS creates separate `PodGroup` objects for the leader and workers within
  each replica.

Phase 1 accepts exactly one active scheduling level. Hierarchical gang-of-gangs scheduling is not supported, and the active level cannot be changed after creation.

## Per-replica default

The shortest opt-in is the replica level:

```yaml
spec:
  scheduling: {}
```

An empty scheduling block selects the replica level (`spec.scheduling.replica`) and defaults to a gang policy. For an
LWS with `replicas: R` and `leaderWorkerTemplate.size: S`, LWS creates:

- one `Workload` containing a stable replica PodGroup template; and
- one runtime `PodGroup` per active replica, each with `gang.minCount: S`.

Each replica's leader and workers are therefore admitted together, while
different replicas remain independent. One replica can run without waiting
for capacity for every other replica in the LWS. Scaling `replicas` adds or
removes independent PodGroups, and changing `size` updates the derived gang
membership as part of the LWS rollout. Replicas that the rollout has not
replaced yet keep PodGroups with the `gang.minCount` of their previous size.

LWS creates the `Workload` before creating member pods. Runtime `PodGroup`
creation depends on the scheduling level:

- At the **whole-LWS level**, the `PodGroup` name does not depend on a
  replica, so LWS creates it before member pods in both group identity modes.
- At the **replica or role level**, leader pods are created with a scheduling
  gate (`leaderworkerset.sigs.k8s.io/group-replacement`). The pod controller
  creates the runtime `PodGroup` objects of a replica while reconciling its
  gated leader pod, before clearing the gate and creating workers.

At the replica and role levels, every leader pod gets `PodGroup` objects of
its own, so a recreated leader never waits on a `PodGroup` of its predecessor
that is being deleted:

- In **Hash mode** (`spec.groupIdentity: Hash`), the `PodGroup` names contain
  the group key that admission generates for each leader pod.
- In **Ordinal mode** (the default `spec.groupIdentity: Ordinal`), a recreated
  leader keeps its name, group index, and revision, so admission adds a random
  5-character group incarnation to the names:
  `<lws-name>-<uid-hash>.<incarnation>-<group-index>[-<role>]-<revision>`.
  Workers inherit the incarnation of their leader.

In all cases, LWS sets `spec.schedulingGroup.podGroupName` on each pod so kube-scheduler can match member pods to the correct gang.

## Compatibility and update restrictions

Native scheduling is an alpha feature. See the [installation
requirements](../../../installation/#enable-native-gang-scheduling) for the
supported LWS and Kubernetes versions, cluster feature gate, LWS feature gate,
and scheduler provider.

The initial implementation has these important restrictions:

- Set `spec.scheduling` when the LeaderWorkerSet is created. It cannot be
  added or removed later, and the active scheduling level (whole-LWS, replica, or role) cannot change.
- The default `startupPolicy: LeaderCreated` is compatible with a per-replica
  gang. `LeaderReady` is rejected because workers are not created until the
  leader is ready, so the complete gang could never form.
- Do not set `spec.schedulingGroup` in the leader or worker pod template. LWS
  derives and sets revision-aware PodGroup names.
- Gang scheduling cannot be combined with the
  `leaderworkerset.sigs.k8s.io/exclusive-topology` annotation in this alpha
  release.
- Leader and worker templates must use the same `priorityClassName`. That
  effective priority class cannot change after creation.
- Phase 1 accepts exactly one active scheduling level among whole-LWS
  (top-level policy, constraints, or disruption mode), replica
  (`spec.scheduling.replica`), and role (`spec.scheduling.replica.leader` or
  `spec.scheduling.replica.worker`). Hierarchical gang-of-gangs scheduling is
  not supported.

`schedulingPolicy`, `schedulingConstraints`, `disruptionMode`, and
`resourceClaims` are immutable after creation (except `gang.minCount`, which
follows replica count and group size).
At the replica level, LWS derives runtime PodGroup instances and gang
membership from `replicas` and `size`, so ordinary replica scaling and size
changes remain supported.

`WorkloadSchedulingCreated=True` on the LeaderWorkerSet means LWS created the
requested scheduling objects. It does not mean the gang was placed or that
the replica is healthy. Use PodGroup conditions and pod placement to inspect
scheduler progress, as shown in the [quickstart](../../../examples/leaderworkerset/gang-scheduling/#inspect-the-scheduling-objects).

## Upgrading LWS

Upgrading LWS does not restart running replicas. Replicas whose leader pod was
admitted by an earlier version keep their `PodGroup` objects, named without a
group incarnation, until their leader pod is recreated.

While the upgrade rolls out, the pod webhook and the controller can briefly
run different LWS versions. If the new webhook admits a leader pod whose
workers the earlier controller then creates, the leader and its workers join
different `PodGroup` objects. LWS recreates such a replica by deleting its
leader pod, as long as none of its workers is scheduled, and records a
`RecreateGroup` event.

Downgrading LWS keeps running replicas running. A replica that the newer
version admitted but that has not started yet can wait for `PodGroup` objects
the earlier version does not create. Delete its leader pod to recreate it.
