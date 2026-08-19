---
title: "Subgroups"
linkTitle: "Subgroups"
weight: 40
description: >
  SubGroup scheduling, sizing, and heterogeneous placement in LeaderWorkerSet.
---

A **SubGroup** represents a logical subdivision of Pods within an LWS replica. While LWS manages all pods in a replica as a single lifecycle unit, SubGroups (introduced in [KEP-115](https://github.com/kubernetes-sigs/lws/tree/main/keps/115-Subgroup-support) and [KEP-257](https://github.com/kubernetes-sigs/lws/blob/main/keps/257-Subgroup-leader-only/README.md)) allow you to define smaller scheduling and placement units within the larger group.

This is especially helpful when subsets of Pods have tighter coordination requirements (such as tensor-parallel groups across high-speed interconnects) or differing hardware requirements (such as a CPU leader orchestrating GPU workers).

## SubGroup Sizing

The `subGroupSize` field inside `.spec.leaderWorkerTemplate.subGroupPolicy` determines the number of pods in each subgroup.

```yaml
spec:
  leaderWorkerTemplate:
    size: 16
    subGroupPolicy:
      subGroupSize: 8
```

Sizing constraints:
- `subGroupSize` must not be greater than `size`.
- `size` must be divisible by `subGroupSize` (yielding equal-sized subgroups under `LeaderWorker`), **or** `size - 1` must be divisible by `subGroupSize` (where the leader is an extra pod in the first subgroup under `LeaderWorker`, or excluded under `LeaderExcluded`).

## SubGroup Policy Types

The `subGroupPolicyType` field (`.spec.leaderWorkerTemplate.subGroupPolicy.subGroupPolicyType`) defines how leader and worker pods are partitioned into subgroups:

| Policy Type | Leader Inclusion | Sizing Requirement | Subgroup Partitioning |
| :--- | :--- | :--- | :--- |
| **`LeaderWorker`** (Default) | Leader is included in the first subgroup | `size` or `size - 1` divisible by `subGroupSize` | • If `(size - 1)` divisible: `(0, 1, ... subGroupSize)`, `(subGroupSize + 1, ... 2*subGroupSize)`, ...<br/>• If `size` divisible: `(0, 1, ... subGroupSize - 1)`, `(subGroupSize, ... 2*subGroupSize - 1)`, ... |
| **`LeaderExcluded`** | Leader is excluded from all subgroups | `size - 1` must be divisible by `subGroupSize` | Worker pods form equal subgroups: `(1, ... subGroupSize)`, `(subGroupSize + 1, ... 2*subGroupSize)`, ... |

### 1. `LeaderWorker` (Default)

`LeaderWorker` includes the leader pod in the first subgroup (Subgroup 0):
- If `size - 1` is divisible by `subGroupSize`, the leader is treated as the extra pod in Subgroup 0 (`0, 1, ... subGroupSize`), and all subsequent subgroups contain `subGroupSize` workers.
- If `size` is divisible by `subGroupSize`, all subgroups contain exactly `subGroupSize` pods (Subgroup 0 contains the leader and `subGroupSize - 1` workers).

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 2
  leaderWorkerTemplate:
    size: 16
    subGroupPolicy:
      subGroupPolicyType: LeaderWorker
      subGroupSize: 8
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

### 2. `LeaderExcluded`

`LeaderExcluded` excludes the leader pod from all subgroups so it can be scheduled independently, while worker pods are partitioned into equal subgroups according to `subGroupSize`.

This policy requires `(size - 1)` to be divisible by `subGroupSize`.

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 2
  leaderWorkerTemplate:
    size: 9
    subGroupPolicy:
      subGroupPolicyType: LeaderExcluded
      subGroupSize: 8
    leaderTemplate:
      spec:
        nodeSelector:
          node.kubernetes.io/instance-type: cpu-standard
        containers:
        - name: leader
          image: leader-image:latest
    workerTemplate:
      spec:
        nodeSelector:
          node.kubernetes.io/instance-type: gpu-accelerated
        containers:
        - name: worker
          image: worker-image:latest
```

#### Key Benefits of `LeaderExcluded`

- **Heterogeneous Scheduling:** Allows placing the leader pod on standard CPU nodes while placing all worker pods on GPU/TPU nodes.
- **Independent Placement:** Workers can be co-located on specialized accelerator racks without requiring the leader to consume accelerator node capacity.

## Subgroup Exclusive Placement

The annotation `leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology` defines a **1:1 mapping between an LWS subgroup and a topology domain**:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
  annotations:
    leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology: topology.kubernetes.io/rack
spec:
  replicas: 2
  leaderWorkerTemplate:
    size: 8
    subGroupPolicy:
      subGroupSize: 4
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

In this example, each 4-pod subgroup is scheduled onto its own exclusive rack, while the overall 8-pod replica can span across racks within the same zone or cluster.
