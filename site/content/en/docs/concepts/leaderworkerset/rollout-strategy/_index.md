---
title: "Rollout Strategy"
linkTitle: "Rollout Strategy"
weight: 60
description: >
  Rolling update budgets, partition, maxUnavailable, and maxSurge in LeaderWorkerSet.
aliases:
- /docs/concepts/rollout-strategy/
---

Rolling update is vital to online services requiring high availability and zero downtime. For LLM inference services, this is particularly important to mitigate stockout and maintain serving capacity during updates.

LeaderWorkerSet supports three primary parameters within `.spec.rolloutStrategy.rollingUpdateConfiguration`: `partition`, `maxUnavailable`, and `maxSurge`:

- `partition`: Protects groups below the specified ordinal from template updates. Defaults to 0.
- `maxUnavailable`: Indicates how many replicas (groups of pods) are allowed to be unavailable during the update, based on `spec.replicas`. Defaults to 1.
- `maxSurge`: Indicates how many extra replicas can be deployed above `spec.replicas` during the update. Defaults to 0.

{{% alert title="Note" color="info" %}}
`maxSurge` and `maxUnavailable` cannot both be zero at the same time.
{{% /alert %}}

## Example Configuration

Here is a LeaderWorkerSet configured with a rolling update strategy (see a full runtime example [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/leaderworkerset/basic/vllm.yaml)):

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  rolloutStrategy:
    type: RollingUpdate
    rollingUpdateConfiguration:
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

## Combined Template Update and Scale-Up

When a template update accompanies scale-up, or desired replicas grow during an
ongoing rollout, the controller uses whole-group availability instead of waiting
for a continuous Ready suffix. Ordinary scaling and rollouts retain their existing
behavior when no combined operation is active.

This changes the default combined-update behavior: with `maxUnavailable: 1`, an
old group can be replaced while additions remain Pending. One eight-GPU group can
release resources while changing to two four-GPU groups on an eight-GPU cluster.
No ordering opt-in is needed:

```yaml
spec:
  replicas: 2
  rolloutStrategy:
    type: RollingUpdate
    rollingUpdateConfiguration:
      maxUnavailable: 1
      maxSurge: 0
```

The controller persists initial non-surge replicas `B` through HPA changes and
revision supersession. With desired replicas `D` and resolved unavailable budget
`U`, the floor is `max(0,min(B,D)-U)`. Percentages resolve against `D`: unavailable
rounds down and surge rounds up. Whole Ready additions provide credit; Pending
additions do not. Leaders and revision-sized workers must be nonterminating and
owned by the current group. Outstanding replacements cannot spend credit twice.

With `U=0`, Ready old replacement waits for additional whole-group Ready capacity.
Surge can provide capacity when desired growth no longer does. Both budgets can
be positive. The bound concerns controller-authorized disruption based on observed
health, not independent failures or a strict rollout-first/scale-first order.

The native StatefulSet remains `RollingUpdate`. The controller reserves all stale
leaders exposed by its partition and can directly delete an authorized lower old
leader behind an updated Pending ordinal. It conservatively blocks when repairing
a lower unavailable old group would expose an unaffordable Ready old higher group.
There is no universal liveness or DaemonSet-style independent selection guarantee.
Reservation storage is bounded; a full window waits for replacements to recover.

## MaxUnavailable Feature Gate

The combined controller does not require the native StatefulSet
`MaxUnavailableStatefulSet` feature gate. It uses partition fencing and
preconditioned leader deletion without increasing the supported-cluster requirement.
Envtest does not run the native StatefulSet controller and is not older-cluster E2E
validation.
