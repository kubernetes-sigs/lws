---
title: "Subgroups"
linkTitle: "Subgroups"
weight: 40
description: >
  SubGroup scheduling, sizing, and heterogeneous placement in LeaderWorkerSet.
---

A **SubGroup** represents a logical subdivision of Pods within an LWS replica. While LWS ensures that all pods in a replica are managed together, SubGroups allow you to define smaller scheduling and placement units within the larger group.

This is especially helpful when subsets of Pods have tighter coordination or differing hardware requirements.

## SubGroup Sizing

The `subGroupSize` field inside `.spec.leaderWorkerTemplate.subGroupPolicy` determines the number of pods in each subgroup.

For example, if a model requires 8 worker pods to form a single tensor-parallel group, you can configure a SubGroup of size 8:

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
      subGroupSize: 8
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

The scheduler treats each SubGroup as an independent placement unit, preventing partial scheduling issues.

## SubGroupType: LeaderOnly

By default, the leader pod is included in the first subgroup. Setting the subgroup type to `LeaderOnly` isolates the leader into its own exclusive subgroup, while worker pods are partitioned into separate subgroups according to `subGroupSize`.

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
      subGroupPolicyType: LeaderOnly
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

### Benefits of `LeaderOnly`

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

## Further Reading

- [KEP-115: Subgroup Support](https://github.com/kubernetes-sigs/lws/tree/main/keps/115-Subgroup-support)
- [KEP-257: Subgroup LeaderOnly](https://github.com/kubernetes-sigs/lws/blob/main/keps/257-Subgroup-leader-only/README.md)
