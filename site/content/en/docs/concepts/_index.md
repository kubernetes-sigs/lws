---
title: "Concepts"
linkTitle: "Concepts"
weight: 4
description: >
  Core concepts of the LeaderWorkerSet and DisaggregatedSet APIs.
---

The LeaderWorkerSet project provides two complementary Kubernetes APIs designed to address the unique requirements of distributed AI/ML workloads:

1. **LeaderWorkerSet (LWS)** (`leaderworkerset.x-k8s.io/v1`): A foundational API for deploying a group of pods as a single **unit of replication**. LWS addresses multi-node model-parallel inference and distributed training workloads where pods within a replica share fate, require tight co-location, and communicate via high-speed interconnects.
2. **DisaggregatedSet (DS)** (`disaggregatedset.x-k8s.io/v1`): A higher-level orchestration API designed for **disaggregated inference** architectures (e.g., separating prefill and decode phases). DisaggregatedSet manages and coordinates multiple underlying LeaderWorkerSets as distinct roles within a unified logical workload.

## Architecture: Two Complementary APIs

LeaderWorkerSet and DisaggregatedSet work together in a layered architecture:

```
┌─────────────────────────────────────────────────────────────┐
│                    DisaggregatedSet                         │
│  (Multi-role orchestration, ratio-preserving rollouts,      │
│   service discovery, slice management, coordinated drain)   │
└──────────────┬───────────────────────────────┬──────────────┘
               │                               │
               ▼                               ▼
  ┌─────────────────────────┐     ┌─────────────────────────┐
  │ LeaderWorkerSet (Role 1)│     │ LeaderWorkerSet (Role 2)│
  │     e.g., Prefill       │     │      e.g., Decode       │
  ├─────────────────────────┤     ├─────────────────────────┤
  │ • Pod group lifecycle   │     │ • Pod group lifecycle   │
  │ • Leader/worker template│     │ • Leader/worker template│
  │ • Exclusive topology    │     │ • Exclusive topology    │
  │ • Subgroup scheduling   │     │ • Subgroup scheduling   │
  │ • Failure restart policy│     │ • Failure restart policy│
  └─────────────────────────┘     └─────────────────────────┘
```

- **LeaderWorkerSet** provides the core primitive: managing a tightly coupled set of leader and worker pods that are created, scaled, and restarted together.
- **DisaggregatedSet** composes multiple LeaderWorkerSets into a complete serving topology, handling cross-role coordination that individual pod groups cannot manage alone.

## Comparison Matrix

| Feature / Dimension | LeaderWorkerSet (LWS) | DisaggregatedSet (DS) |
| :--- | :--- | :--- |
| **Primary Purpose** | Deploying a group of pods as a unit of replication | Orchestrating multi-role disaggregated serving topologies |
| **Unit of Replication** | Replica = 1 leader Pod + *N* worker Pods | Set = Multiple roles, each mapped to a child LWS |
| **Workload Type** | Homogeneous multi-node inference or distributed training | Heterogeneous multi-role inference (prefill, decode, encode) |
| **CRD** | `leaderworkerset.x-k8s.io/v1` | `disaggregatedset.x-k8s.io/v1` |
| **Rollout Ownership** | LWS controller (`maxUnavailable`, `maxSurge`) | DisaggregatedSet controller (lockstep, ratio-preserving) |
| **Scaling** | Horizontal Pod Autoscaler (HPA) via scale subresource | Independent per-role scaling & full topology slice scaling |
| **Service Discovery** | Headless service per replica (`SubdomainUniquePerReplica`) | Headless service per role with revision-aware routing |
| **Placement & Topology** | Exclusive topology placement & subgroups per replica | Role-level placement policies & slice topology spread |
| **Failure Handling** | `RecreateGroupOnPodRestart`, `None`, `RecreateGroupAfterStart` | Coordinated drain and restart policies across all roles |

## When to Use Which API

### Use LeaderWorkerSet when:
- All inference pods are **homogeneous** (same model sharding, same hardware requirements across all nodes).
- You do not need to disaggregate distinct serving phases (e.g., running standard tensor-parallel inference with vLLM, SGLang, or TensorRT-LLM).
- You are running distributed training, fine-tuning, or data-caching workloads (such as Kubeflow Trainer or Axlearn).
- You need fine-grained control over pod subgroup placement or group restart policies within a single pod group.

### Use DisaggregatedSet when:
- You are deploying **disaggregated LLM inference** where prefill (compute-bound) and decode (memory-bandwidth-bound) phases run on different hardware or require different pod group sizes.
- You need to scale prefill and decode capacities independently based on traffic patterns (e.g., prompt length vs. generation length).
- You require coordinated, lockstep rollouts across multiple roles without disrupting serving ratios or dropping requests.
- You want declarative management of a complex multi-role topology in a single Kubernetes manifest.

---

## Concept Sections

Explore the detailed concepts for each API:

### [LeaderWorkerSet](leaderworkerset/)

Concepts and capabilities of the core LeaderWorkerSet API:

- **[Dual Pod Templates](leaderworkerset/pod-templates/)**: Configure distinct specifications for leader and worker pods.
- **[Startup Policy](leaderworkerset/startup-policy/)**: Control worker creation timing relative to leader pod readiness.
- **[Exclusive Topology Placement](leaderworkerset/topology-placement/)**: Co-locate replica pods onto exclusive topology domains (e.g. rack, host).
- **[Subgroups](leaderworkerset/subgroups/)**: Subgroup sizing, independent placement, and `LeaderOnly` heterogeneous scheduling.
- **[Volume Claim Templates Support](leaderworkerset/volume-claim-templates/)**: Provision persistent storage dynamically for leader and worker pods.
- **[Rollout Strategy](leaderworkerset/rollout-strategy/)**: Rolling update mechanics, `maxUnavailable`, and `maxSurge` configurations for zero-downtime upgrades.
- **[Failure Handling](leaderworkerset/failure-handling/)**: Group restart policies (`RecreateGroupOnPodRestart`, `None`, `RecreateGroupAfterStart`) and node failure recovery.

### [DisaggregatedSet](disaggregatedset/)

Concepts and capabilities of the DisaggregatedSet API:

- **[DisaggregatedSet](disaggregatedset/)**: Architecture, relationship to LeaderWorkerSet, role specifications, coordinated N-dimensional rollouts, slice replication, placement policies, and lifecycle management.