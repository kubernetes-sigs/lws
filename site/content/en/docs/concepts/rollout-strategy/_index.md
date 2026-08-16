---
title: "Rollout Strategy"
linkTitle: "Rollout Strategy"
weight: 10
description: >
---

Rolling update is vital to online services with zero downtime. For LLM inference services, this is particularly important, which helps to mitigate stockout. Three different configurations are supported in LWS, `updateOrder`, `maxUnavailable`, and `maxSurge`:

- `updateOrder`: Controls simultaneous template updates and scale-ups. `ScaleFirst` creates the additional replicas before updating existing replicas and is the default. `RolloutFirst` updates existing replicas before creating additional replicas, which allows old resources to be released in clusters without spare capacity.
- `maxUnavailable`: Indicates how many replicas are allowed to be unavailable during the update, the unavailable number is based on the spec.replicas. Defaults to 1.
- `maxSurge`: Indicates how many extra replicas can be deployed during the update. Defaults to 0.

Note that `maxSurge` and `maxUnavailable` can not both be zero at the same time.

Here's a leaderWorkerSet configured with rollout strategy, you can find the example [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/sample/lws-rollout-strategy.yaml):

```yaml
spec:
  rolloutStrategy:
    type: RollingUpdate
    rollingUpdateConfiguration:
      updateOrder: ScaleFirst
      maxUnavailable: 2
      maxSurge: 2
  replicas: 4
```

In the following we'll show how rolling update processes for a leaderWorkerSet with four replicas. The rolling step is equal to maxUnavailable(2)+maxSurge(2)=4, three Replica status are simulated here:

- ✅ Replica has been updated
- ❎ Replica hasn't been updated
- ⏳ Replica is in rolling update

|      | Partition | Replicas | R-0 |  R-1 | R-2 | R-3 | R-4 | R-5 | Note |
| ----------- | ----------- | ----------- | ----------- | ----------- | ----------- | ----------- | ----------- | ----------- | ----------- |
| Stage1      | 0 | 4 |  ✅   |  ✅ | ✅ | ✅ |  |  | Before rolling update |
| Stage2   | 4 | 6 |  ❎ | ❎ | ❎ | ❎ | ⏳ | ⏳ | Rolling update started |
| Stage3      | 2 | 6 |  ❎  |  ❎ | ⏳ | ⏳ | ⏳ | ⏳ | Partition changes from 4 to 2 |
| Stage4      | 2 | 6 |  ❎  |  ❎ | ⏳ | ⏳ | ✅ | ⏳ | Since the last Replica is not ready, Partition will not change |
| Stage5   | 0 | 6 |  ⏳ | ⏳ | ⏳ | ⏳ | ✅ | ✅ | Partition changes from 2 to 0 |
| Stage6      | 0 | 6 |  ⏳  |  ⏳ | ✅ | ✅ | ✅ | ✅ | R-2 and R-3 become ready |
| Stage7   | 0 | 4 |  ⏳ | ⏳ | ✅ | ✅ |  |  | Scale down to 4 immediately, reclaiming both surge replicas |
| Stage8     | 0 | 4 |  ⏳  | ✅ |  ✅ | ✅ |  |  | R-1 becomes ready |
| Stage9     | 0 | 4 |  ✅  | ✅ |  ✅ | ✅ |  |  | Rolling update completed |

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

## MaxUnavailable Feature
`MaxUnavailable` was graduated to Beta in Kubernetes [1.35](1.35_release_notes), which means that it is enabled by default.


[1.35_release_notes]: https://kubernetes.io/blog/2025/12/17/kubernetes-v1-35-release/#maxunavailable-for-statefulsets