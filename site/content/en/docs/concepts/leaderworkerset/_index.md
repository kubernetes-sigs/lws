---
title: "LeaderWorkerSet"
linkTitle: "LeaderWorkerSet"
weight: 10
description: >
  Core concepts of LeaderWorkerSet (LWS) — unit of replication, relationship with StatefulSet, architecture, and design rationale.
---

**LeaderWorkerSet (LWS)** is a Kubernetes API designed to deploy and manage a group of pods as a single **unit of replication**. It addresses common deployment patterns of distributed AI/ML workloads — such as multi-host inference and distributed fine-tuning — where a model is sharded across multiple accelerators spanning multiple nodes that must be scheduled, scaled, and managed together.

<p align="center">
  <img src="/images/lws-concept.svg" width="550" alt="LWS Concept">
</p>

## Architecture and Relationship with StatefulSet

Under the hood, LeaderWorkerSet implements an **API composition pattern** on top of native Kubernetes primitives. Rather than managing individual pods directly, LWS composes **two tiers of StatefulSets**:

```
LeaderWorkerSet (spec.replicas = 2, size = 4)
│
└── Leader StatefulSet: "<lws-name>" (replicas = 2)
    │
    ├── Replica 0
    │   ├── Leader Pod:         <lws-name>-0
    │   └── Worker StatefulSet: "<lws-name>-0" (replicas = 3, startOrdinal = 1)
    │       ├── Worker Pod:     <lws-name>-0-1  (ordinal 1)
    │       ├── Worker Pod:     <lws-name>-0-2  (ordinal 2)
    │       └── Worker Pod:     <lws-name>-0-3  (ordinal 3)
    │
    └── Replica 1
        ├── Leader Pod:         <lws-name>-1
        └── Worker StatefulSet: "<lws-name>-1" (replicas = 3, startOrdinal = 1)
            ├── Worker Pod:     <lws-name>-1-1  (ordinal 1)
            ├── Worker Pod:     <lws-name>-1-2  (ordinal 2)
            └── Worker Pod:     <lws-name>-1-3  (ordinal 3)
```

### How LWS Manages Pods

1. **Leader StatefulSet:** The LWS controller creates a single **Leader StatefulSet** named `<lws-name>` with `spec.replicas` matching the LWS replica count. This StatefulSet generates the leader pods (`<lws-name>-0`, `<lws-name>-1`, ..., `<lws-name>-(R-1)`), using `leaderTemplate` (or `workerTemplate` if `leaderTemplate` is omitted).
2. **Worker StatefulSets:** For each leader pod, the controller creates a corresponding **Worker StatefulSet** named `<lws-name>-<replica-index>` with `replicas = size - 1` and `startOrdinal = 1`. This creates the worker pods (`<lws-name>-<replica-index>-1` through `<lws-name>-<replica-index>-(size-1)`), using `workerTemplate`.
3. **Headless Service:** A headless service is created for each replica (or shared across the set) to enable predictable, direct pod-to-pod DNS resolution.

---

## Why StatefulSet Was Selected for API Composition

StatefulSet was chosen as the underlying building block for both leader and worker pods due to several critical capabilities required by distributed AI/ML workloads:

### 1. Deterministic Ordinal Identity and Predictable Networking
Distributed training and inference frameworks (such as PyTorch DDP/FSDP, Megatron-LM, vLLM, TensorRT-LLM, and SGLang) rely on static rank assignment (`RANK 0..N-1`, `WORLD_SIZE`), peer identification, and deterministic rendezvous.
- **Leader StatefulSet** assigns deterministic replica indices (`0, 1, ..., R-1`).
- **Worker StatefulSets** assign deterministic worker indices (`1, 2, ..., size-1`).
- In combination with headless services, every leader and worker pod receives a stable, predictable DNS name (`<pod-name>.<service-name>.<namespace>.svc.cluster.local`), eliminating the need for complex dynamic discovery protocols or external service registries.

### 2. Native Dynamic Storage (`volumeClaimTemplates`)
Distributed AI models often require dedicated local storage for caching large model checkpoints, tokenizers, or intermediate KV caches on high-speed NVMe drives attached to each node.
- StatefulSet has built-in support for `volumeClaimTemplates`, which automatically provisions dedicated, persistent storage per pod ordinal.
- By composing with StatefulSet, LWS inherits automatic volume provisioning for both leader and worker pods without needing custom volume controllers.

### 3. Parallel Pod Management
While traditional stateful services (such as databases) deploy pods sequentially, distributed AI/ML replicas require all pods in a group to start simultaneously to establish collective communication (MPI/NCCL) and avoid idle accelerator time.
- LWS configures the worker StatefulSets with `podManagementPolicy: Parallel`, allowing all worker pods in a group to be created and initialized concurrently while preserving their deterministic ordinal names and storage bindings.

### 4. Partitioned Rolling Updates
StatefulSet natively supports partition-based rolling updates (`.spec.updateStrategy.rollingUpdate.partition`).
- LWS leverages this mechanism on the Leader StatefulSet to implement group-level rolling updates (`maxUnavailable` and `maxSurge`), updating replicas in controlled batches while keeping the active serving capacity intact.

### 5. Kubernetes API Composition Best Practices
Building on top of StatefulSet adheres to Kubernetes design principles by reusing battle-tested core controllers:
- The core Kubernetes StatefulSet controller reliably manages low-level container restarts, volume attachments, pod creation retries, and API interactions.
- **LeaderWorkerSet focuses on the higher-level group abstractions that Kubernetes does not provide natively:**
  - Treating a heterogeneous group of pods (leader + workers) as a single replication unit.
  - Group-level failure detection and all-or-nothing restart policies.
  - 1:1 exclusive topology placement for an entire pod group (e.g., rack or block).
  - Group-level rolling updates and coordinated rollouts.
  - Subgroup partitioning for heterogeneous scheduling (CPU leader + GPU workers).
