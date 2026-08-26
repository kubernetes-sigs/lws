---
title: "Dual Pod Templates"
linkTitle: "Dual Pod Templates"
weight: 10
description: >
  Configuring leader and worker pod templates in LeaderWorkerSet.
---

LeaderWorkerSet supports defining distinct specifications for leader and worker pods within a replica.

## Leader and Worker Templates

An LWS replica consists of one leader pod and *N - 1* worker pods (where *N* is defined by `.spec.leaderWorkerTemplate.size`). `size` can be changed on a running LeaderWorkerSet, which recreates every pod in every group. See [Resizing](../resizing/).

You can configure pod specifications using two template fields:

- `workerTemplate` (**required**): Defines the pod template for worker pods. If `leaderTemplate` is not specified, `workerTemplate` applies to the leader pod as well.
- `leaderTemplate` (**optional**): Defines a distinct pod template exclusively for the leader pod.

```yaml
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
        containers:
        - name: leader
          image: leader-coordinator:latest
          resources:
            requests:
              cpu: "2"
              memory: 4Gi
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-engine:latest
          resources:
            limits:
              nvidia.com/gpu: "8"
```

## Common Use Cases

1. **Heterogeneous Hardware:**
   The leader pod can run as a coordinator, HTTP frontend, or driver on standard CPU nodes, while the worker pods run distributed model parallel computation on specialized GPU or TPU accelerator nodes.

2. **Different Container Images or Configurations:**
   The leader pod can run specialized tooling, telemetry sidecars, or API server processes that are not needed on worker pods.

3. **Homogeneous Deployments:**
   When leader and worker pods share the same container configuration, omit `leaderTemplate` and specify only `workerTemplate`. LWS will apply `workerTemplate` to all pods in the replica.
