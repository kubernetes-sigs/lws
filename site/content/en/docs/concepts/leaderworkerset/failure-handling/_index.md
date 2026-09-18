---
title: "Failure Handling and Restart Policies"
linkTitle: "Failure Handling"
weight: 70
description: >
  Learn how LeaderWorkerSet handles pod and node failures with configurable restart policies.
aliases:
- /docs/concepts/failure-handling/
---

LeaderWorkerSet provides configurable failure handling for pod groups, ensuring that pod and node failures in distributed workloads are handled consistently according to the coupling requirements of the application.

Configure the failure and restart behavior via `.spec.leaderWorkerTemplate.restartPolicy`:

### RecreateGroupOnPodRestart (Default)

When any pod in a group fails or restarts, the entire replica group (leader + all workers) is deleted and recreated.

- **Pod Failures:** If a single container or pod fails or restarts, all other pods in the group are terminated and recreated simultaneously to ensure all processes restart fresh and re-initialize collective communication or distributed caches cleanly.
- **Node Failures:** When a node hosting any pod in the replica fails or becomes unreachable, the entire replica group is deleted and recreated on healthy nodes, respecting topology placement constraints.
- **Primary Use Case:** Tightly coupled multi-host distributed inference and training (e.g., tensor-parallel or pipeline-parallel models) where a single pod or node failure breaks collective communication.

{{< include file="examples/leaderworkerset/failure-handling/recreate-group-on-pod-restart.yaml" lang="yaml" >}}

### None

Only the failed pod is restarted or rescheduled. Other pods in the group continue running without interruption.

- **Pod Failures:** If an individual pod or container fails, only that specific pod is restarted by Kubernetes.
- **Node Failures:** When a node fails, only the pods residing on that failed node are rescheduled. Other pods in the replica remain running on their existing nodes.
- **Primary Use Case:** Loosely coupled workers or workloads with application-level fault tolerance where individual pods can reconnect or recover independently.

{{< include file="examples/leaderworkerset/failure-handling/none.yaml" lang="yaml" >}}

### RecreateGroupAfterStart

When any pod in a group fails, the entire group is recreated **if and only if there are no pods currently pending** in the group. If any pod in the replica is still in the `Pending` phase (e.g., during image pulls or initial scheduling), the controller skips the failure event without triggering a group-wide recreation.

- **Pod Failures:** Recreates the entire group if a pod fails after all pods in the replica have started (no pods are `Pending`). If any pod in the replica is `Pending`, the failure event is skipped, allowing Kubernetes to handle pod restarts individually and preventing restart cascades during rollout.
- **Node Failures:** If a node fails after all pods in the replica have started, the entire replica group is deleted and recreated on healthy nodes. If the failure occurs while any pod in the replica is `Pending`, group recreation is not triggered.
- **Primary Use Case:** Workloads with large container images or long startup times where you want strict collective restart semantics in production once running, but want to prevent recreation loops during the initial rollout.

{{% alert title="Note" color="info" %}}
The `RecreateGroupAfterStart` restart policy is supported in LWS version 0.9.0+.
{{% /alert %}}

{{< include file="examples/leaderworkerset/failure-handling/recreate-group-after-start.yaml" lang="yaml" >}}

## Limit Automatic Group Recreation

For `RecreateGroupOnPodRestart` and `RecreateGroupAfterStart`, set
`maxGroupRestarts` to limit how many times LWS can automatically recreate each
replica group. When the field is unset, group recreation remains unlimited. A
value of `0` disables automatic group recreation on the first qualifying
failure.

{{< include file="examples/leaderworkerset/failure-handling/bounded-group-recovery.yaml" lang="yaml" >}}

The budget is tracked independently for each replica group and Pod template
revision. It is consumed only when LWS initiates a group recreation. The field
is not supported with `restartPolicy: None` or `groupIdentity: Hash`.

When a group exhausts its budget, LWS:

1. Terminates the leader and worker Pods to release their scheduled resources.
2. Retains their Pod API objects with cleanup finalizers and stops automatic
   recreation of that group.
3. Sets `Degraded=True` with reason `ReplicaRestartBudgetExceeded`. Other
   replica groups continue running.

{{% alert title="Kubernetes version requirement" color="warning" %}}
Budget exhaustion handling requires Kubernetes 1.27 or later. It relies on the
[Kubernetes 1.27+ Pod deletion flow](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#termination-of-pods)
to stop containers and transition deleted Pods to a terminal phase while
finalizers retain their API objects. The general LWS minimum Kubernetes version
remains unchanged.
{{% /alert %}}

Retaining a Pod API object preserves its status, but does not guarantee that
`kubectl logs` remains available after the container runtime removes the
terminated container. Use an external logging system when logs must survive
group termination.

### Recover an Exhausted Group

After fixing the underlying problem, explicitly recover one exhausted group by
annotating its retained leader Pod:

```shell
kubectl annotate pod <leader-pod-name> leaderworkerset.sigs.k8s.io/recover=true
```

LWS then clears that group's count, removes the cleanup finalizers, and allows
the StatefulSet to create a replacement group with a fresh budget. Editing or
unsetting `maxGroupRestarts`, or deleting retained Pods, does not recover an
exhausted group. LWS deletion, scale-down, and selecting the group for
replacement during a rollout remove retained objects as normal lifecycle
cleanup rather than starting recovery.
