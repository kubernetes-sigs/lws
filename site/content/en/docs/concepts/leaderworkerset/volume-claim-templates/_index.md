---
title: "Volume Claim Templates Support"
linkTitle: "Volume Claim Templates"
weight: 50
description: >
  Configuring persistent storage for leader and worker pods using volumeClaimTemplates.
---

LeaderWorkerSet supports the use of `volumeClaimTemplates` for provisioning dedicated PersistentVolumeClaims (PVCs) for leader and worker pods within each replica.

## Defining `volumeClaimTemplates`

You can declare a list of `volumeClaimTemplates` inside `.spec.leaderWorkerTemplate`. These templates are used to dynamically provision persistent storage across all pods in the LWS replicas.

Containers in `leaderTemplate` and `workerTemplate` can then reference the claim templates by name in their `volumeMounts`:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: lws-storage-sample
spec:
  replicas: 2
  leaderWorkerTemplate:
    size: 4
    volumeClaimTemplates:
      - metadata:
          name: model-cache
        spec:
          storageClassName: standard-rwo
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
              - mountPath: /mnt/models
                name: model-cache
    workerTemplate:
      spec:
        containers:
          - name: worker
            image: worker-image:latest
            volumeMounts:
              - mountPath: /mnt/models
                name: model-cache
```

## How It Works

1. **Independent PVC Provisioning:**
   Each pod created by LWS (both leader and workers) receives its own dedicated PVC instantiated from the template.
2. **Lifecycle Management:**
   PVCs created via `volumeClaimTemplates` follow standard Kubernetes volume lifecycle rules and are preserved across individual container restarts.
3. **Common Use Cases:**
   - **Local Model Weights Caching:** Caching large model weights, checkpoints, or tokenizer assets on high-performance block storage (e.g. SSDs) attached to each pod.
   - **Scratch Space:** Providing scratch space for distributed tensor serialization or intermediate KV cache storage.
