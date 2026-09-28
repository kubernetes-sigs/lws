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
  replicas).
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
membership as part of the LWS rollout.

LWS creates the `Workload` before creating member pods. Runtime `PodGroup`
creation depends on the scheduling level and group identity:

- In **Ordinal mode** (the default `spec.groupIdentity: Ordinal`), replica
  ordinals are known upfront. LWS creates the runtime `PodGroup` objects before
  creating member pods.
- In **Hash mode** (`spec.groupIdentity: Hash`) at the whole-LWS level, the
  `PodGroup` name does not depend on a group key, so LWS also creates it before
  member pods.
- In **Hash mode** at the replica or role level, group keys are generated at
  admission. Leader pods are created first with a scheduling gate
  (`leaderworkerset.sigs.k8s.io/group-replacement`). The pod controller creates
  the runtime `PodGroup` objects while reconciling the leader pod, before
  clearing its gate and creating workers.

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

The scheduling policy and immutable constraints cannot be changed in place.
At the replica level, LWS derives runtime PodGroup instances and gang
membership from `replicas` and `size`, so ordinary replica scaling and size
changes remain supported. The alpha implementation has known update edge
cases:

- A size change during a rolling update can deadlock a whole-LWS gang; see
  [#1080](https://github.com/kubernetes-sigs/lws/issues/1080).
- A partitioned scale-down followed by a scale-up at the replica or role level
  can leave a recreated replica waiting on a deleted PodGroup; see
  [#1082](https://github.com/kubernetes-sigs/lws/issues/1082) and the fix in
  [#1108](https://github.com/kubernetes-sigs/lws/pull/1108).

`WorkloadSchedulingCreated=True` on the LeaderWorkerSet means LWS created the
requested scheduling objects. It does not mean the gang was placed or that
the replica is healthy. Use PodGroup conditions and pod placement to inspect
scheduler progress, as shown in the [quickstart](../../../examples/leaderworkerset/gang-scheduling/#inspect-the-scheduling-objects).
