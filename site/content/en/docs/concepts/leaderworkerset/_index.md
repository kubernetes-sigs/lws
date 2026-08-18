---
title: "LeaderWorkerSet"
linkTitle: "LeaderWorkerSet"
weight: 10
description: >
  Core concepts of LeaderWorkerSet (LWS) — unit of replication, pod templates, startup policies, topology placement, and subgroups.
---

# LeaderWorkerSet (LWS)

LeaderWorkerSet (LWS) is a Kubernetes API designed to deploy and manage a group of pods as a single **unit of replication**. It addresses common deployment patterns of distributed AI/ML workloads — such as multi-host inference and distributed fine-tuning — where a model is sharded across multiple accelerators spanning multiple nodes that must be scheduled, scaled, and managed together.

## Conceptual Diagram

<p align="center">
  <img src="/images/lws-concept.svg" width="550" alt="LWS Concept">
</p>

## Running an Example LeaderWorkerSet

Here is an example LeaderWorkerSet manifest:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 3
  leaderWorkerTemplate:
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: nginx
          image: nginxinc/nginx-unprivileged:1.27
          resources:
            limits:
              cpu: "100m"
            requests:
              cpu: "50m"
          ports:
          - containerPort: 8080
```

To list all pods that belong to an LWS:

```bash
kubectl get pods --selector=leaderworkerset.sigs.k8s.io/name=leaderworkerset-sample
```

The output is structured with ordinal indices for the leader and workers:

```
NAME                         READY   STATUS    RESTARTS   AGE
leaderworkerset-sample-0     1/1     Running   0          6m10s
leaderworkerset-sample-0-1   1/1     Running   0          6m10s
leaderworkerset-sample-0-2   1/1     Running   0          6m10s
leaderworkerset-sample-0-3   1/1     Running   0          6m10s
leaderworkerset-sample-1     1/1     Running   0          6m10s
leaderworkerset-sample-1-1   1/1     Running   0          6m10s
leaderworkerset-sample-1-2   1/1     Running   0          6m10s
leaderworkerset-sample-1-3   1/1     Running   0          6m10s
leaderworkerset-sample-2     1/1     Running   0          6m10s
leaderworkerset-sample-2-1   1/1     Running   0          6m10s
leaderworkerset-sample-2-2   1/1     Running   0          6m10s
leaderworkerset-sample-2-3   1/1     Running   0          6m10s
```

## Multi-Template for Pods

LWS supports using different pod templates for the leader and worker pods via the optional `leaderTemplate` field. If `leaderTemplate` is omitted, the `workerTemplate` definition applies to both leader and worker pods.

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 3
  leaderWorkerTemplate:
    size: 4
    leaderTemplate:
      spec:
        containers:
        - name: leader
          image: leader-image:latest
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

## Startup Policy

`.spec.startupPolicy` controls **when the worker StatefulSet is created** relative to its leader Pod. There are two options:

- **`LeaderCreated` (default):** The LWS controller **creates the worker StatefulSet as soon as** the leader Pod object is created. This does not guarantee any readiness order between the leader and workers.
- **`LeaderReady`:** The LWS controller **delays creating the worker StatefulSet until** the leader Pod is `Ready`.

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  startupPolicy: LeaderReady
  replicas: 3
  leaderWorkerTemplate:
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: nginx
          image: nginxinc/nginx-unprivileged:1.27
```

## Exclusive LWS to Topology Placement

The annotation `leaderworkerset.sigs.k8s.io/exclusive-topology` defines a 1:1 placement constraint between an LWS replica and a topology domain. For example, to ensure all pods in an LWS replica are scheduled on the same rack to maximize inter-node communication bandwidth:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
  annotations:
    leaderworkerset.sigs.k8s.io/exclusive-topology: rack
spec:
  replicas: 3
  leaderWorkerTemplate:
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

### Subgroups

A **SubGroup** represents a logical subdivision of Pods within a workload replica. While LWS as a whole ensures that Pods in a workload can be scheduled as a group, SubGroups are useful when only subsets of Pods have tighter co-location requirements.

For example, in disaggregated serving, one SubGroup can represent prefill servers and another can represent decode servers. Each Pod within a SubGroup might need to be scheduled on the same rack for low-latency interconnects, while different SubGroups can be placed on separate racks within the same zone.

#### SubGroup Size

The `size` of a SubGroup determines how many Pods it contains. For example, if a prefill server requires 8 Pods to operate together, you can define a SubGroup of size 8. The scheduler ensures that all 8 Pods are considered together when making placement decisions.

#### SubGroupType: LeaderOnly

- The `LeaderOnly` type creates a SubGroup exclusively for the leader.
- Workers are placed into separate subgroups according to the configured size, rather than being grouped with the leader.
- This enables heterogeneous scheduling — for example, placing the leader Pod on CPU nodes while placing worker Pods on GPU nodes with exclusive placement to ensure workers land on the same GPU rack.

For more details, see:
- [KEP-115: Subgroup Support](https://github.com/kubernetes-sigs/lws/tree/main/keps/115-Subgroup-support)
- [KEP-257: Subgroup LeaderOnly](https://github.com/kubernetes-sigs/lws/blob/main/keps/257-Subgroup-leader-only/README.md)

#### Subgroup Exclusive Placement

The annotation `leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology` defines a 1:1 mapping between an LWS subgroup and a topology domain:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
  annotations:
    leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology: rack
spec:
  replicas: 3
  leaderWorkerTemplate:
    subGroupPolicy:
      subGroupSize: 2
    size: 4
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: worker-image:latest
```

## Volume Claim Templates Support

LWS supports `volumeClaimTemplates` for leader and worker pods, allowing the use of storage classes to dynamically provision persistent volumes:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: lws
spec:
  replicas: 2
  leaderWorkerTemplate:
    size: 2
    volumeClaimTemplates:
      - metadata:
          name: persistent-storage
        spec:
          storageClassName: default
          accessModes: ["ReadWriteOnce"]
          resources:
            requests:
              storage: 100Gi
    leaderTemplate:
      spec:
        containers:
          - name: leader
            image: leader-image:latest
            volumeMounts:
              - mountPath: /mnt/volume
                name: persistent-storage
    workerTemplate:
      spec:
        containers:
          - name: worker
            image: worker-image:latest
            volumeMounts:
              - mountPath: /mnt/volume
                name: persistent-storage
```

## Related Topics

- [Rollout Strategy](rollout-strategy/) — Rolling update configurations, `maxUnavailable`, and `maxSurge`.
- [Failure Handling](failure-handling/) — Pod failure detection and configurable restart policies.
