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

## Node Failure Handling

**With RecreateGroupOnPodRestart (default)**: When a node fails, the entire group is recreated on healthy nodes.

**With None**: Only pods on the failed node are rescheduled. Other pods continue running.

