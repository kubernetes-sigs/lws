---
title: "Pod Termination Policy"
linkTitle: "Pod Termination Policy"
weight: 75
description: >
  Controlling group pod termination behavior during deletion, rebuilds, and scale-down.
---

The `.spec.podTerminationPolicy` field controls how leader and worker pods within a replica group are terminated when a group is deleted, rebuilt, or scaled down.

## Background

In LeaderWorkerSet, worker StatefulSets are owned by their respective leader pod.
By default, Kubernetes garbage collection deletes owned child objects sequentially after the owner object is completely removed from the API server.

When pods configure large `terminationGracePeriodSeconds` (common in large AI/ML training and inference workloads to flush weights, model checkpoints, or gracefully drain connections), sequential termination can significantly delay group rebuilds and resource cleanup:
1. Leader pod is deleted and waits for its termination grace period (e.g. 60–120s).
2. Only after the leader pod is completely removed from etcd, the worker StatefulSet and worker pods begin their termination grace period (another 60–120s).
3. The total group teardown takes up to **2 × terminationGracePeriodSeconds**.

With concurrent termination, group teardown and recreation time is halved.

## Available Policies

LeaderWorkerSet supports two pod termination policies:

### 1. `Default` (Default)

Under the `Default` policy, existing sequential deletion behavior is preserved.

- **Behavior:** The leader pod terminates first. Once the leader pod has fully terminated and been removed, the Kubernetes garbage collector cascades deletion to the worker StatefulSet and its worker pods.
- **Use case:** Workloads where worker pods depend on an active leader pod during their shutdown sequence, or where existing sequential behavior is desired.

{{< include file="examples/leaderworkerset/pod-termination-policy/default.yaml" lang="yaml" >}}

### 2. `Parallel`

Under the `Parallel` policy, leader and worker pods are terminated concurrently.

- **Behavior:**
  - **Leader Pod Deletion / Group Rebuild:** As soon as the leader pod enters termination (`DeletionTimestamp != nil`), the controller proactively initiates foreground deletion on the worker StatefulSet, terminating leader and worker pods concurrently.
  - **Scale-Down / Rolling Update:** Terminating groups shut down their leader and worker pods in parallel.
  - **LeaderWorkerSet Deletion:** When the LeaderWorkerSet resource is deleted, worker StatefulSets are deleted concurrently with the leader pods.
  - **Safe Rebuild:** If a replacement leader pod starts reconciling while the previous group's worker StatefulSet is still terminating, the controller waits until the old worker StatefulSet is fully cleaned up before provisioning the new group.
- **Use case:** Distributed training or inference workloads where leader and worker pods can shut down independently, and fast group recovery or rapid scale-down/deletion is critical.

{{< include file="examples/leaderworkerset/pod-termination-policy/parallel.yaml" lang="yaml" >}}
