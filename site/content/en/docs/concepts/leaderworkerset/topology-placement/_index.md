---
title: "Exclusive Topology Placement"
linkTitle: "Exclusive Placement"
weight: 30
description: >
  Co-locating LWS pod replicas onto exclusive topology domains for high-speed interconnects.
---

Large distributed AI/ML workloads require high-bandwidth, low-latency inter-node communication (e.g., NVLink, RoCE, or InfiniBand) between pods participating in tensor-parallel or pipeline-parallel model execution.

LeaderWorkerSet provides topology-aware placement features to ensure that all pods within an LWS replica land in the same physical topology domain.

## Exclusive Topology Annotation

The annotation `leaderworkerset.sigs.k8s.io/exclusive-topology` defines a **1:1 mapping between an LWS replica and a topology domain**.

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
  annotations:
    leaderworkerset.sigs.k8s.io/exclusive-topology: topology.kubernetes.io/zone
spec:
  replicas: 3
  leaderWorkerTemplate:
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

Common topology keys include:
- `topology.kubernetes.io/zone`
- `kubernetes.io/hostname`
- Custom node labels representing racks, blocks, or network switches (e.g., `cloud.google.com/gke-placement-group`, `topology.kubernetes.io/rack`, or `topology.kubernetes.io/block`).

## How It Works

1. **Topology Constraint Enforcement:**
   When an LWS replica is created, the LWS controller configures node affinity/anti-affinity rules so that all pods belonging to a single replica (the leader and its workers) must be scheduled within the same topology domain instance (e.g., the same rack).

2. **Mutual Exclusivity:**
   Pods from different replicas will not share the same topology domain instance if exclusive placement is requested, preventing noisy-neighbor interference and optimizing cross-node bandwidth.

3. **Subgroup Placements:**
   If you need exclusive placement for subsets of pods within a replica (e.g., placing workers on GPU racks while the leader runs on CPU nodes), use [Subgroups](../subgroups/) in combination with `leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology`.
