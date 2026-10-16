---
title: "Exclusive Topology Placement"
linkTitle: "Exclusive Placement"
weight: 30
description: >
  Co-locating LWS pod replicas onto exclusive topology domains for high-speed interconnects.
---

Large distributed AI/ML workloads require high-bandwidth, low-latency inter-node communication (e.g., NVLink, RoCE, or InfiniBand) between pods participating in tensor-parallel or pipeline-parallel model execution.

LeaderWorkerSet provides topology-aware placement features to ensure that all pods within an LWS replica land in the same physical topology domain.

## Placement Policy

`spec.placementPolicy` defines a **1:1 mapping between an LWS replica and a topology domain**:

```yaml
spec:
  placementPolicy:
    type: ExclusiveTopology
    topology: cloud.google.com/gke-nodepool
```

`spec.placementPolicy.subGroup` does the same for each [subgroup](../subgroups/), and needs `leaderWorkerTemplate.subGroupPolicy`. The two levels must use different topology keys:

```yaml
spec:
  placementPolicy:
    type: ExclusiveTopology
    topology: topology.kubernetes.io/zone
    subGroup:
      type: ExclusiveTopology
      topology: cloud.google.com/gke-nodepool
```

The field replaces the annotations described below, which are deprecated and still work. Notes on using it:

- Setting a level in the field and in the matching annotation is accepted only when both name the same topology key. A level whose `type` is `None` uses the annotation.
- The effective placement, from the field or the annotations, is part of the LeaderWorkerSet revision. Moving a value from an annotation to the field keeps the revision and replaces no group. Changing the placement, through either one, rolls out through a new revision.
- Group level exclusive placement cannot be combined with gang scheduling or workload topology constraints in `spec.scheduling`. Subgroup level placement can.
- In a DisaggregatedSet role, exclusive placement must not use the same topology key as the DisaggregatedSet `spec.placementPolicy`, and should use a finer one, for example a slice per rack with a group per host.
- Kueue topology-aware scheduling does not account for the affinity that exclusive placement injects. Admission returns a warning when a pod template also carries Kueue topology annotations.

## Exclusive Topology Annotation

The annotation `leaderworkerset.sigs.k8s.io/exclusive-topology` defines a **1:1 mapping between an LWS replica and a topology domain**.

{{< include file="examples/leaderworkerset/topology-placement/exclusive-topology.yaml" lang="yaml" >}}

Common topology keys include:
- `topology.kubernetes.io/zone`
- `kubernetes.io/hostname`
- Custom node labels representing racks, blocks, or network switches (e.g., `cloud.google.com/gke-placement-group`, `topology.kubernetes.io/rack`, or `topology.kubernetes.io/block`).

## How It Works

1. **Topology Constraint Enforcement:**
   When an LWS replica is created, the LWS controller configures pod affinity/anti-affinity rules so that all pods belonging to a single replica (the leader and its workers) must be scheduled within the same topology domain instance (e.g., the same rack).

2. **Mutual Exclusivity:**
   Pods from different replicas will not share the same topology domain instance if exclusive placement is requested, preventing noisy-neighbor interference and optimizing cross-node bandwidth.

3. **Subgroup Placements:**
   If you need exclusive placement for subsets of pods within a replica (e.g., placing workers on GPU racks while the leader runs on CPU nodes), use [Subgroups](../subgroups/) in combination with `leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology`.
