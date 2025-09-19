---
title: "Concepts"
linkTitle: "Concepts"
weight: 4
description: >
  Core LWS Concepts
no_list: true
---

# LeaderWorkerSet (LWS)
An LWS creates a group of pods based on two different templates (the leader template and the worker template), and controls their lifecycle.

## Conceptual Diagram

![LWS diagram](../../images/concept.png)

## Running an Example LeaderWorkerSet

Here is an example LeaderWorkerSet

```
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 3
  leaderWorkerTemplate:
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: nginx
          image: nginxinc/nginx-unprivileged:1.27
          resources:
            limits:
              cpu: "100m"
            requests:
              cpu: "50m"
          ports:
          - containerPort: 8080
```

To list all the pods that belong to a LWS, you can use a command like this:

```
kubectl get pods --selector=leaderworkerset.sigs.k8s.io/name=leaderworkerset-sample
```

The output should be similar to

```
NAME                         READY   STATUS    RESTARTS   AGE
leaderworkerset-sample-0     1/1     Running   0          6m10s
leaderworkerset-sample-0-1   1/1     Running   0          6m10s
leaderworkerset-sample-0-2   1/1     Running   0          6m10s
leaderworkerset-sample-0-3   1/1     Running   0          6m10s
leaderworkerset-sample-1     1/1     Running   0          6m10s
leaderworkerset-sample-1-1   1/1     Running   0          6m10s
leaderworkerset-sample-1-2   1/1     Running   0          6m10s
leaderworkerset-sample-1-3   1/1     Running   0          6m10s
leaderworkerset-sample-2     1/1     Running   0          6m10s
leaderworkerset-sample-2-1   1/1     Running   0          6m10s
leaderworkerset-sample-2-2   1/1     Running   0          6m10s
leaderworkerset-sample-2-3   1/1     Running   0          6m10s
```

## Multi-Template for Pods
LWS support using different templates for leader and worker pods, if a `leaderTemplate` field is specified. If it isn't, the template used for
`workerTemplate` will apply to both leader and worker pods.

```
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 3
  leaderWorkerTemplate:
    size: 4
    leaderTemplate:
      spec:
    workerTemplate:
      spec:
```

## Exclusive LWS to Topology Placement
The LWS annotation `leaderworkerset.sigs.k8s.io/exclusive-topology` defines a 1:1 LWS replica to topology placement. For example,
you want an LWS replica to be scheduled on the same rack in order to maximize cross-node communcation for distributed inference. This
can be done as follows:

```
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
  annotations:
    leaderworkerset.sigs.k8s.io/exclusive-topology: rack
spec:
  replicas: 3
  leaderWorkerTemplate:
  ...
```

### Subgroup and Exclusive Placement
The LWS annotation `leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology` defines a 1:1 between an LWS subgroup to topology placement. This can
be useful for dissagregated serving in order to place the prefill pod group in the same rack, but on a seperate rack from the decode pod group, assuming
same hardware requirements.

```
metadata:
  name: leaderworkerset-sample
  annotations:
    leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology: rack
spec:
  replicas: 3
  leaderWorkerTemplate:
    subGroupPolicy:
      subGroupSize: 2
    size: 4
```
### SubGroups

A **SubGroup** represents a logical subdivision of Pods within a workload. SubGroups are useful when a workload consists of Pods that need to start together or follow a specific execution pattern.

#### SubGroup Size
The `size` of a SubGroup determines how many Pods it contains. For example, if a distributed training job requires 8 Pods, you can define a SubGroup of size 8. The scheduler ensures that all 8 Pods are considered together when making placement decisions. This prevents partial scheduling that could cause the job to hang or waste resources.

#### LeaderOnly SubGroup Type
In some workloads, only a single Pod (the *leader*) is critical to start first, while the other Pods (workers) depend on it. The `LeaderOnly` SubGroup type supports this use case:

- The leader Pod is scheduled first and guaranteed placement.
- Worker Pods in the same SubGroup are scheduled afterwards, following the leader.
- This is especially useful for distributed machine learning jobs (e.g., PyTorch, TensorFlow) or MPI-based HPC jobs where one Pod coordinates the others.

For more details, see [KEP-257: Subgroup LeaderOnly](https://github.com/kubernetes-sigs/lws/blob/main/keps/257-Subgroup-leader-only/README.md).

