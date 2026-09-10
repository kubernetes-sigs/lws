---
title: "Startup Policy"
linkTitle: "Startup Policy"
weight: 20
description: >
  Controlling worker creation order relative to the leader pod.
---

The `.spec.startupPolicy` field controls **when the worker StatefulSet is created** relative to the leader pod in each replica.

## Available Policies

LeaderWorkerSet supports two startup policies:

### 1. `LeaderCreated` (Default)

The LWS controller creates the worker StatefulSet as soon as the leader Pod object is created in the Kubernetes API.

- **Behavior:** Leader and worker pods are created in parallel.
- **Readiness:** Does not guarantee that the leader is `Ready` before workers begin starting or running.
- **Use case:** Distributed workloads where leader and worker initialization can proceed concurrently, or where pods coordinate synchronization directly over the network during bootstrap.

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  startupPolicy: LeaderCreated
  replicas: 3
  leaderWorkerTemplate:
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

### 2. `LeaderReady`

The LWS controller delays creating the worker StatefulSet until the leader Pod has achieved the `Ready` condition.

- **Behavior:** The leader pod must successfully pass its readiness probes and reach the `Ready` state before worker pods are scheduled or started.
- **Readiness:** Guarantees that the leader is fully initialized and operational before workers are created.
- **Use case:** Scenarios where the leader acts as a central coordinator, parameter server, or discovery registry that must be fully active and reachable before workers attempt to register or pull state.

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  startupPolicy: LeaderReady
  replicas: 3
  leaderWorkerTemplate:
    size: 4
    leaderTemplate:
      spec:
        containers:
        - name: coordinator
          image: coordinator-image:latest
          readinessProbe:
            httpGet:
              path: /healthz
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 5
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```
