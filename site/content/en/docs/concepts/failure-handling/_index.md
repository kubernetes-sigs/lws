---
title: "Failure Handling and Restart Policies"
linkTitle: "Failure Handling"
weight: 15
description: >
  Learn how LeaderWorkerSet handles pod failures with configurable restart policies.
---

LeaderWorkerSet provides configurable failure handling for pod groups when failures occur.

## Restart Policies

### RecreateGroupOnPodRestart (Default)

When any pod in a group fails, the entire group is recreated. This ensures all pods start fresh together.

```yaml
spec:
  leaderWorkerTemplate:
    restartPolicy: RecreateGroupOnPodRestart
```

**Use case**: Tightly coupled applications (distributed influence)

### None

Only the failed pod is restarted. Other pods in the group are not affected.

```yaml
spec:
  leaderWorkerTemplate:
    restartPolicy: None
```

### RecreateGroupAfterStart

When any pod in a group fails, the entire group is recreated if and only if there are no pods currently pending. This allows for large image pulls to complete without interruption.

On version 0.9+, this feature can be enabled via the `restartPolicy` field:

```yaml
spec:
  leaderWorkerTemplate:
    restartPolicy: RecreateGroupAfterStart
```

On version 0.8, this feature is experimental and can be enabled via the following annotation:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
  annotations:
    leaderworkerset.sigs.k8s.io/experimental-recreate-group-after-start: true
```

## Node Failure Handling

**With RecreateGroupOnPodRestart (default)**: When a node fails, the entire group is recreated on healthy nodes.

**With None**: Only pods on the failed node are rescheduled. Other pods continue running.

