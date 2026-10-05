---

title: "Troubleshooting"
linkTitle: "Troubleshooting"
weight: 10
date: 2025-03-25
description: >
  LWS troubleshooting tips.
no_list: true
---

## 1. Infinite StatefulSet Creation Loops

When creating StatefulSets, you might encounter an issue where the `kubectl get pod` output shows an infinite loop of pod creation, as illustrated below:

```
vllm-alpha-distributed-serving-0                             0/1     Running             0          10s
vllm-alpha-distributed-serving-0-0                           1/1     Running             0          10s
vllm-alpha-distributed-serving-0-0-0                         0/1     ContainerCreating   0          10s
vllm-alpha-distributed-serving-0-0-0-0                       0/1     ContainerCreating   0          10s
vllm-alpha-distributed-serving-0-0-0-0-0                     0/1     ContainerCreating   0          10s
vllm-alpha-distributed-serving-0-0-0-0-0-0                   0/1     ContainerCreating   0          10s
```

### Cause

This issue arises on Kubernetes clusters running versions earlier than 1.27, where the `StatefulSetStartOrdinal` feature gate is not enabled. In such cases, the LeaderWorkerSet controllers enter infinite reconciliation loops, potentially exhausting cluster resources.

### Solution

To resolve this issue:
- Upgrade your Kubernetes cluster to version **1.26 or higher**. Versions below 1.26 may exhibit unexpected behavior.
- For Kubernetes version 1.26, manually enable the `StatefulSetStartOrdinal` feature gate.
- For versions above 1.26, this feature gate is enabled by default.

---

## 2. Rolling Update of Leader Pods During LWS Upgrade

When upgrading from LWS version 0.5.0 to 0.6.0 or later, all Leader Pods configured with SubGroup will undergo a rolling update.

### Cause

In LWS version 0.5.0, an annotation key was set as `leaderworkerset.gke.io/subgroup-size`. Starting from version 0.6.0, this key was changed to `leaderworkerset.sigs.k8s.io/subgroup-size` as part of [this pull request](https://github.com/kubernetes-sigs/lws/pull/434). As a result, upgrading the LWS controller triggers a rolling update.

### Solution

Rolling updates typically do not impact system operations. However, it is recommended to monitor the system closely during the upgrade process.

---

## 3. Unable to Create LWS Object with a Name Exceeding 51 Characters

When creating an LWS object with a name longer than 51 characters, the worker pods fail to start. The error message appears as follows:

```
Pod "<worker-pod-name>" is invalid: metadata.labels: Invalid value: <worker-sts-name>-<10-character-hash>": must be no more than 63 characters
```

### Cause

This issue occurs because StatefulSet names exceeding 57 characters prevent pods from starting, as described in [this Kubernetes issue](https://github.com/kubernetes/kubernetes/issues/64023). Since LWS relies on StatefulSets, it is not possible to create an LWS object with a name longer than 51 characters.

### Solution

The name limit for LWS objects is calculated as `(51 - int(replicas / 10))`. This is because the worker StatefulSet name grows by one character for replicas above 9, another character for replicas above 99, and so on. Ensure that the LWS object name adheres to this limit to avoid issues. With `groupIdentity: Hash`, the limit is 43 characters, or 54 for groups of size 1, and admission enforces it.

---

## 4. Native Gang Scheduling Does Not Progress

### Check for an admission error

If `kubectl apply` rejects a LeaderWorkerSet that has `spec.scheduling`, read
the admission error before looking for a status condition. The object does not
exist when admission fails. Common errors report that:

- the `WorkloadAwareScheduling` feature gate is not enabled;
- a scheduler provider is not configured; or
- the `scheduling.k8s.io/v1beta1` `Workload` or `PodGroup` API is not
  available.

Confirm that the cluster serves both APIs and that the controller is configured
for native scheduling:

```shell
kubectl api-resources --api-group=scheduling.k8s.io | grep -E 'workloads|podgroups'
kubectl logs deployment/lws-controller-manager -n lws-system
```

See the [installation prerequisites](../installation/#enable-native-gang-scheduling)
for the required cluster and controller settings.

### Verify scheduling object creation

After admission succeeds, check the LWS scheduling condition:

```shell
kubectl get leaderworkerset <name> \
  -o jsonpath='{.status.conditions[?(@.type=="WorkloadSchedulingCreated")]}{"\n"}'
```

If the condition is `False`, its reason is one of `APINotAvailable`,
`UnsupportedProviderCapability`, `InvalidSchedulingConfiguration`,
`WorkloadCreateFailed`, `PodGroupCreateFailed`, `ParentWorkloadNotReady`,
or `PodGroupCleanupBlocked`. Its message contains the underlying
reconciliation error. If the condition is absent, inspect the controller
logs.

Then list the objects owned by the LWS:

```shell
kubectl get workloads,podgroups,pods \
  -l leaderworkerset.sigs.k8s.io/name=<name>
```

### Diagnose a pending gang

A gang remaining `Pending` is expected when kube-scheduler cannot place all
`minCount` members together. Inspect the PodGroup, pods, and recent events:

```shell
kubectl describe podgroup <podgroup-name>
kubectl describe pods \
  -l leaderworkerset.sigs.k8s.io/name=<name>
kubectl get events --sort-by=.lastTimestamp
```

Check that one replica's combined CPU, memory, and extended-resource requests
fit the available nodes. Also check node selectors, affinity, taints and
tolerations, unbound volumes, and resource quotas. Adding capacity or relaxing
the blocking constraint lets kube-scheduler retry the whole gang.

`PodGroupInitiallyScheduled=True` records that the group completed its initial
placement once. It is not current replica health; use the LeaderWorkerSet
`Available`/`Progressing` conditions and pod readiness for that.
