---
title: "Resizing Groups"
linkTitle: "Resizing"
weight: 65
description: >
  Changing the number of pods per group on a running LeaderWorkerSet.
---

`.spec.leaderWorkerTemplate.size` can be changed on a running LeaderWorkerSet. This was added in [KEP-552](https://github.com/kubernetes-sigs/lws/tree/main/keps/552-worker-resizing) so that a group can be resized with `kubectl apply` or a GitOps commit, instead of deleting and recreating the object.

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 2
  leaderWorkerTemplate:
    size: 4      # edit this and re-apply
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

## Resizing Recreates Every Pod

A resize is not an in-place operation. `size` is part of `leaderWorkerTemplate`, and `leaderWorkerTemplate` is what LWS stores in a `ControllerRevision`, so changing `size` produces a new revision exactly like changing a container image does. Every pod in every group is replaced, leaders included, even though only the worker count changed.

This happens because each pod carries its group size as the `leaderworkerset.sigs.k8s.io/size` annotation, which is read once at pod creation to populate `LWS_GROUP_SIZE`. An existing pod cannot learn about the new size, so it has to be recreated.

The replacement runs through the normal [rollout strategy](../rollout-strategy/): groups are updated in batches according to `maxUnavailable` and `maxSurge`, and a non-zero `rollingUpdate.partition` will hold back the groups below it. Plan a resize the same way you would plan an image rollout, not as a scaling operation.

If you need to add capacity without disturbing pods that are already serving, scale `.spec.replicas` instead. That adds whole groups and leaves existing ones running. Growing a group in place, where new workers join a running group and the engine reconfigures itself, is not supported. It was listed as a non-goal in KEP-552.

## Resizing With Subgroups

`subGroupSize` is immutable, but `size` is not. The two are validated against each other on every update, so a resize that breaks the [subgroup sizing rules](../subgroups/) is rejected by the webhook with `size or size - 1 must be divisible by subGroupSize`.

The rejection is reported against `spec.leaderWorkerTemplate.subGroupPolicy.subGroupSize`, even though `size` is the field you changed. With `subGroupSize: 8`, valid sizes are 8, 9, 16, 17, 24, 25 and so on. To move to a size that does not fit, recreate the LeaderWorkerSet with both fields set together.

## Crossing size: 1

A group of `size: 1` is a leader on its own, and LWS creates no worker StatefulSet for it. Resizing across that boundary is supported in both directions, and the worker StatefulSet appears or disappears as part of the rollout rather than being scaled. Each worker StatefulSet is owned by its leader pod, so replacing the leader takes the old worker StatefulSet with it.
