---
title: "Rollout Strategy"
linkTitle: "Rollout Strategy"
weight: 10
description: >
---

Rolling update is vital to online services with zero downtime. For LLM inference services, this is particularly important, which helps to mitigate stockout. Two different configurations are supported in LWS, `maxUnavailable` and `maxSurge`:

- `MaxUnavailable`: Indicates how many replicas are allowed to be unavailable during the update, the unavailable number is based on the spec.replicas. Defaults to 1.
- `MaxSurge`: Indicates how many extra replicas can be deployed during the update. Defaults to 0.

Note that maxSurge and maxUnavailable can not both be zero at the same time.

Here's a leaderWorkerSet configured with rollout strategy, you can find the example [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/sample/lws-rollout-strategy.yaml):

```yaml
spec:
  rolloutStrategy:
    type: RollingUpdate
    rollingUpdateConfiguration:
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
| Stage6      | 0 | 6 |  ⏳  |  ⏳ | ⏳ | ✅ | ✅ | ✅ |  |
| Stage7   | 0 | 5 |  ⏳ | ✅ | ⏳ | ✅ | ✅ | | Reclaim a Replica for the accommodation of unready ones |
| Stage8     | 0 | 4 |  ✅  | ⏳ |  ✅ | ✅ | | | Release another Replica |
| Stage9     | 0 | 4 |  ✅  | ✅ |  ✅ | ✅ | | | Rolling update completed |

## MaxUnavailable Feature
`MaxUnavailable` currently requires the [MaxUnavailableStatefulSet][max_unavailable] to be enabled. See upstream discussion [here][max_unavailable_enhancement] and LWS side discussion [here][lws_max_unavailable_enhancement]



[feature_gate]: https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/
[start_ordinal]: https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#start-ordinal
[max_unavailable]: https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#maximum-unavailable-pods
[max_unavailable_enhancement]: https://github.com/kubernetes/enhancements/issues/961
[lws_max_unavailable_enhancement]: https://github.com/kubernetes-sigs/lws/issues/315