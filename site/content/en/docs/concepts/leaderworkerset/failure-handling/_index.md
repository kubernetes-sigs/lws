---
title: "Failure Handling and Restart Policies"
linkTitle: "Failure Handling"
weight: 70
description: >
  Learn how LeaderWorkerSet handles pod and node failures with configurable restart policies.
aliases:
- /docs/concepts/failure-handling/
---

LeaderWorkerSet provides configurable failure handling for pod groups, ensuring that failures in tightly coupled distributed workloads are handled consistently.

## Restart Policies

Configure the restart behavior for worker and leader pods via `.spec.leaderWorkerTemplate.restartPolicy`:

### RecreateGroupOnPodRestart (Default)

When any pod in a group fails or restarts, the entire replica group (leader + all workers) is deleted and recreated. This ensures all pods in the replica start fresh together and re-initialize collective communication or distributed caches cleanly.

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 2
  leaderWorkerTemplate:
    restartPolicy: RecreateGroupOnPodRestart
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

**Primary use case:** Tightly coupled multi-host distributed inference and training where worker failure breaks collective communication.

### None

Only the failed pod is restarted. Other pods in the group continue running without interruption.

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 2
  leaderWorkerTemplate:
    restartPolicy: None
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

**Primary use case:** Loosely coupled workers or workloads with built-in fault tolerance where individual pods can reconnect independently.

### RecreateGroupAfterStart

When any pod in a group fails, the entire group is recreated **if and only if there are no pods currently pending** in the group. This allows large container image pulls or initial scheduling delays to complete without triggering premature group recreation cascades.

On version 0.9+, this feature is enabled via the `restartPolicy` field:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 2
  leaderWorkerTemplate:
    restartPolicy: RecreateGroupAfterStart
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

On version 0.8, this feature can be enabled via annotation:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
  annotations:
    leaderworkerset.sigs.k8s.io/experimental-recreate-group-after-start: "true"
spec:
  replicas: 2
  leaderWorkerTemplate:
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

## Node Failure Handling

- **With `RecreateGroupOnPodRestart` (default):** When a node hosting any pod in the replica fails, the entire replica group is deleted and recreated on healthy nodes, respecting topology placement constraints.
- **With `None`:** Only the pods residing on the failed node are rescheduled. Other pods in the replica remain running on their existing nodes.
