---
title: "LeaderWorkerSet"
linkTitle: "LeaderWorkerSet"
weight: 10
description: >
  Core concepts of LeaderWorkerSet (LWS) — unit of replication, pod templates, startup policies, topology placement, subgroups, and lifecycle management.
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

---

## LeaderWorkerSet Concepts

Explore the detailed capabilities and configuration options of LeaderWorkerSet:

- **[Multi-Template for Pods](pod-templates/)**: Define distinct specifications for the leader pod (`leaderTemplate`) and worker pods (`workerTemplate`) to support heterogeneous nodes or coordinator patterns.
- **[Startup Policy](startup-policy/)**: Control whether worker StatefulSets are created immediately in parallel (`LeaderCreated`) or delayed until the leader pod is `Ready` (`LeaderReady`).
- **[Exclusive Topology Placement](topology-placement/)**: Ensure all pods in a replica land in the same physical topology domain (e.g., rack, host, or network switch) for maximum inter-node bandwidth.
- **[Subgroups](subgroups/)**: Divide a replica into smaller placement and scheduling units (`subGroupSize`) and isolate leader pods onto separate node pools with `LeaderOnly`.
- **[Volume Claim Templates Support](volume-claim-templates/)**: Provision dedicated PersistentVolumeClaims (PVCs) dynamically for leader and worker pods using `volumeClaimTemplates`.
- **[Rollout Strategy](rollout-strategy/)**: Configure group-level rolling update mechanics using `maxUnavailable` and `maxSurge` for zero-downtime upgrades.
- **[Failure Handling](failure-handling/)**: Configure group restart policies (`RecreateGroupOnPodRestart`, `None`, `RecreateGroupAfterStart`) and node failure recovery.
