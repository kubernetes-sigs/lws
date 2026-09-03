---
title: "Rollout Strategy"
linkTitle: "Rollout Strategy"
weight: 60
description: >
  Rolling update configurations, updateOrder, maxUnavailable, and maxSurge in LeaderWorkerSet.
aliases:
- /docs/concepts/rollout-strategy/
---

Rolling update is vital to online services requiring high availability and zero downtime. For LLM inference services, this is particularly important to mitigate stockout and maintain serving capacity during updates.

LeaderWorkerSet supports three primary parameters within `.spec.rolloutStrategy.rollingUpdateConfiguration`: `updateOrder`, `maxUnavailable`, and `maxSurge`:

- `updateOrder`: Controls simultaneous template updates and scale-ups. `ScaleFirst` creates the additional replicas before updating existing replicas and is the default. `RolloutFirst` updates existing replicas before creating additional replicas, which allows old resources to be released in clusters without spare capacity.
- `maxUnavailable`: Indicates how many replicas (groups of pods) are allowed to be unavailable during the update, based on `spec.replicas`. Defaults to 1.
- `maxSurge`: Indicates how many extra replicas can be deployed above `spec.replicas` during the update. Defaults to 0.

{{% alert title="Note" color="info" %}}
`maxSurge` and `maxUnavailable` cannot both be zero at the same time.
{{% /alert %}}

## Example Configuration

Here is a LeaderWorkerSet configured with a rolling update strategy (see sample [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/sample/lws-rollout-strategy.yaml)):

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  rolloutStrategy:
    type: RollingUpdate
    rollingUpdateConfiguration:
      updateOrder: ScaleFirst
      maxUnavailable: 2
      maxSurge: 2
  replicas: 4
  leaderWorkerTemplate:
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: nginx
          image: nginxinc/nginx-unprivileged:1.27
```

## Rolling Update Process

Below is a step-by-step trace of how a rolling update executes for a LeaderWorkerSet with 4 replicas where `maxUnavailable=2` and `maxSurge=2` (step size = `maxUnavailable` + `maxSurge` = 4).

Status indicators:
- ✅ Replica has been updated to the new revision
- ❎ Replica has not yet been updated (running old revision)
- ⏳ Replica is currently undergoing rolling update (not yet ready)

| Stage | Partition | Replicas | R-0 | R-1 | R-2 | R-3 | R-4 (Surge) | R-5 (Surge) | Description |
| :--- | :--- | :--- | :---: | :---: | :---: | :---: | :---: | :---: | :--- |
| **Stage 1** | 0 | 4 | ✅ | ✅ | ✅ | ✅ | | | Steady state before rolling update |
| **Stage 2** | 4 | 6 | ❎ | ❎ | ❎ | ❎ | ⏳ | ⏳ | Rolling update starts; 2 surge replicas created |
| **Stage 3** | 2 | 6 | ❎ | ❎ | ⏳ | ⏳ | ⏳ | ⏳ | Partition decreases to 2; R-2 & R-3 begin update |
| **Stage 4** | 2 | 6 | ❎ | ❎ | ⏳ | ⏳ | ✅ | ⏳ | R-4 becomes ready; partition waits for R-5 |
| **Stage 5** | 0 | 6 | ⏳ | ⏳ | ⏳ | ⏳ | ✅ | ✅ | R-5 becomes ready; partition drops to 0; R-0 & R-1 begin update |
| **Stage 6** | 0 | 6 | ⏳ | ⏳ | ✅ | ✅ | ✅ | ✅ | R-2 and R-3 become ready |
| **Stage 7** | 0 | 4 | ⏳ | ⏳ | ✅ | ✅ | | | Scaled down to 4 replicas; surge replicas reclaimed |
| **Stage 8** | 0 | 4 | ⏳ | ✅ | ✅ | ✅ | | | R-1 becomes ready |
| **Stage 9** | 0 | 4 | ✅ | ✅ | ✅ | ✅ | | | R-0 becomes ready; rolling update complete |

## Update Order

`ScaleFirst` preserves availability during a simultaneous template update and scale-up by creating the additional replicas before updating existing replicas. It requires enough capacity for the old replicas and at least one additional replica to run at the same time.

`RolloutFirst` is intended for capacity-constrained clusters. It holds the current replica count, updates existing replicas according to `maxUnavailable`, waits for them to become ready, and then scales to the desired replica count. For example, changing one replica that requests eight GPUs into two replicas that request four GPUs each requires `RolloutFirst` when only eight GPUs are available:

```yaml
spec:
  replicas: 2
  rolloutStrategy:
    type: RollingUpdate
    rollingUpdateConfiguration:
      updateOrder: RolloutFirst
      maxUnavailable: 1
      maxSurge: 0
```

`RolloutFirst` requires `maxUnavailable` to be greater than zero. It can temporarily reduce availability while existing replicas are replaced.

## MaxUnavailable Feature Gate

`MaxUnavailable` for StatefulSets graduated to Beta in Kubernetes [1.35](https://kubernetes.io/blog/2025/12/17/kubernetes-v1-35-release/#maxunavailable-for-statefulsets), meaning it is enabled by default in supported Kubernetes clusters.
