---
title: "Gang scheduling"
linkTitle: "Gang scheduling"
weight: 4
description: >
  Run a CPU-only LeaderWorkerSet with one native scheduling gang per replica.
---

This quickstart creates two LeaderWorkerSet replicas of two pods each. The
empty `spec.scheduling: {}` opts into native Workload-Aware Scheduling and
selects the default behavior: the leader and worker in each replica form an
independent gang. Kubernetes admits both members of a replica together or
keeps both pending.

Before starting, complete the [native gang scheduling installation
prerequisites](../../../installation/#enable-native-gang-scheduling).

## Create the LeaderWorkerSet

The example requests only CPU and memory, so it does not require accelerators
or a device plugin.

{{< include file="examples/leaderworkerset/gang-scheduling/lws.yaml" lang="yaml" >}}

Apply it from the documentation site:

```shell
kubectl apply -f https://lws.sigs.k8s.io/examples/leaderworkerset/gang-scheduling/lws.yaml
```

Wait for both replicas to become ready:

```shell
kubectl wait leaderworkerset/lws-gang \
  --for=jsonpath='{.status.readyReplicas}'=2 --timeout=5m
```

## Inspect the scheduling objects

LWS creates one `Workload` and two `PodGroup` objects. Each PodGroup has a gang
minimum of two, matching the replica size:

```shell
kubectl get workloads,podgroups \
  -l leaderworkerset.sigs.k8s.io/name=lws-gang

kubectl get podgroups \
  -l leaderworkerset.sigs.k8s.io/name=lws-gang \
  -o custom-columns='NAME:.metadata.name,MIN-COUNT:.spec.schedulingPolicy.gang.minCount,INITIALLY-SCHEDULED:.status.conditions[?(@.type=="PodGroupInitiallyScheduled")].status'
```

Confirm that every pod names its PodGroup and has been assigned a node:

```shell
kubectl get pods \
  -l leaderworkerset.sigs.k8s.io/name=lws-gang \
  -o custom-columns='NAME:.metadata.name,NODE:.spec.nodeName,PODGROUP:.spec.schedulingGroup.podGroupName'
```

For the scheduling lifecycle and restrictions, see the [gang scheduling
concepts](../../../concepts/leaderworkerset/gang-scheduling/).

## Clean up

Deleting the LeaderWorkerSet also deletes its Workload and PodGroups:

```shell
kubectl delete -f https://lws.sigs.k8s.io/examples/leaderworkerset/gang-scheduling/lws.yaml
```
