# How to use LeaderWorkerSet Features

Below are some examples to use different features of LeaderWorkerSet.

Deploy a LeaderWorkerSet with 3 groups and 4 workers in each group. You can find an example [here](lws.yaml)

## Multi template for leader/worker pods

LWS support using different templates for leader and worker pods. You could find the example [here](lws-multi-template.yaml),
leader pod's spec is specified in leaderTemplate, and worker pods' spec is specified in workerTemplate.

## Restart Policy

You could specify the RestartPolicy to define the failure handling schematics for the pod group.
By default, only failed pods will be automatically restarted. When the RestartPolicy is set to RecreateGroupOnRestart, it will recreate
the groups on container/pods restarts. All the worker pods will be recreated after the new leader pod is started.
You can find an example [here](lws-restart-policy.yaml).

## Rollout Strategy

Rolling update is vital to online services with zero downtime. For LLM, this is particularly important, which helps to mitigate the stockout situation. Two different configurations are supported in LWS, `maxUnavailable` and `maxSurge`:

- MaxUnavailable: It indicates how many replicas are allowed to be unavailable during the update, the unavailable number is based on the leaderWorkerSet Replicas. Default to 1.
- MaxSurge: It indicates how many extra replicas can be deployed during the update, default to 0.

Note that maxSurge and maxUnavailable can not both be zero at the same time.

Here's a leaderWorkerSet configured with rollout strategy, you can find the example [here](lws-rollout-strategy.yaml):

```yaml
spec:
  rolloutStrategy:
    type: RollingUpdate
    rollingUpdateConfiguration:
      maxUnavailable: 2
      maxSurge: 2
  replicas: 4
```

We'll have a dry run for rolling update process, the rolling step is equal to maxUnavailable(2)+maxSurge(2)=4, three Replica status are simulated here:

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

## Horizontal Pod AutoScaler (HPA)

LWS expose a scale endpoint for HPA to trigger scaling. An example HPA yaml for LWS can be found [here](horizontal-pod-autoscaler.yaml)

## Exclusive Placement

LeaderWorkerSet supports exclusive placement through pod affinity/anti-affinity where pods in the same group will be scheduled on the same accelerator island (such as a TPU slice or a GPU clique), but on different nodes. This ensures 1:1 LWS replica to accelerator island placement.
This feature can be enabled by adding the exclusive topology annotation **leaderworkerset.sigs.k8s.io/exclusive-topology:** as shown [here](lws-exclusive-placement.yaml)
