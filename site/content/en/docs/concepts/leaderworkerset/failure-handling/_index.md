---
title: "Failure Handling and Restart Policies"
linkTitle: "Failure Handling"
weight: 70
description: >
  Learn how LeaderWorkerSet handles pod and node failures with configurable restart policies.
aliases:
- /docs/concepts/failure-handling/
---

LeaderWorkerSet provides configurable failure handling for pod groups, ensuring that pod and node failures in distributed workloads are handled consistently according to the coupling requirements of the application.

Configure the failure and restart behavior via `.spec.leaderWorkerTemplate.restartPolicy`:

### RecreateGroupOnPodRestart (Default)

When any pod in a group fails or restarts, the entire replica group (leader + all workers) is deleted and recreated.

- **Pod Failures:** If a single container or pod fails or restarts, all other pods in the group are terminated and recreated simultaneously to ensure all processes restart fresh and re-initialize collective communication or distributed caches cleanly.
- **Node Failures:** When a node hosting any pod in the replica fails or becomes unreachable, the entire replica group is deleted and recreated on healthy nodes, respecting topology placement constraints.
- **Primary Use Case:** Tightly coupled multi-host distributed inference and training (e.g., tensor-parallel or pipeline-parallel models) where a single pod or node failure breaks collective communication.

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

### None

Only the failed pod is restarted or rescheduled. Other pods in the group continue running without interruption.

- **Pod Failures:** If an individual pod or container fails, only that specific pod is restarted by Kubernetes.
- **Node Failures:** When a node fails, only the pods residing on that failed node are rescheduled. Other pods in the replica remain running on their existing nodes.
- **Primary Use Case:** Loosely coupled workers or workloads with application-level fault tolerance where individual pods can reconnect or recover independently.

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

### RecreateGroupAfterStart

When any pod in a group fails, the entire group is recreated **if and only if there are no pods currently pending** in the group. If any pod in the replica is still in the `Pending` phase (e.g., during image pulls or initial scheduling), the controller skips the failure event without triggering a group-wide recreation.

- **Pod Failures:** Recreates the entire group if a pod fails after all pods in the replica have started (no pods are `Pending`). If any pod in the replica is `Pending`, the failure event is skipped, allowing Kubernetes to handle pod restarts individually and preventing restart cascades during rollout.
- **Node Failures:** If a node fails after all pods in the replica have started, the entire replica group is deleted and recreated on healthy nodes. If the failure occurs while any pod in the replica is `Pending`, group recreation is not triggered.
- **Primary Use Case:** Workloads with large container images or long startup times where you want strict collective restart semantics in production once running, but want to prevent recreation loops during the initial rollout.

{{% alert title="Note" color="info" %}}
The `RecreateGroupAfterStart` restart policy is supported in LWS version 0.9.0+.
{{% /alert %}}

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
