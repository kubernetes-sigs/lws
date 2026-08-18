---
title: "Concepts"
linkTitle: "Concepts"
weight: 4
description: >
  Core concepts of the LeaderWorkerSet and DisaggregatedSet APIs.
---

This project provides two complementary Kubernetes APIs for distributed AI/ML workloads: **LeaderWorkerSet (LWS)** (`leaderworkerset.x-k8s.io/v1`) and **DisaggregatedSet (DS)** (`disaggregatedset.x-k8s.io/v1`).

## Architecture: Two Complementary APIs

LeaderWorkerSet and DisaggregatedSet work together in a layered architecture:

- **LeaderWorkerSet (LWS)** (`leaderworkerset.x-k8s.io/v1`): A foundational API for deploying a group of pods as a single **unit of replication**. LWS addresses multi-node model-parallel inference and distributed training workloads where pods within a replica share fate, require tight co-location, and communicate via high-speed interconnects.
- **DisaggregatedSet (DS)** (`disaggregatedset.x-k8s.io/v1`): A higher-level orchestration API designed for **disaggregated inference** architectures (e.g., separating prefill and decode phases). DisaggregatedSet manages and coordinates multiple underlying LeaderWorkerSets as distinct roles within a unified logical workload.

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
- You do not need to separate prefill from decode (e.g., running standard tensor-parallel inference with vLLM, SGLang, or TensorRT-LLM).
- You are running distributed training, fine-tuning, batch workloads, or data-caching (such as Kubeflow Trainer or Axlearn).
- You need fine-grained control over pod subgroup placement or group restart policies within a single pod group.

### Use DisaggregatedSet when:
- You are deploying **disaggregated LLM inference** (e.g., vLLM with P/D disaggregation, SGLang, or llm-d) where distinct phases (such as prefill and decode) require different GPU types, different container images, or different pod group sizes.
- You want to scale prefill and decode replicas independently based on traffic patterns (e.g., prompt length vs. generation length).
- You require coordinated, lockstep rollouts across multiple roles without disrupting serving ratios or dropping requests.
- You want declarative management of a complex multi-role topology in a single Kubernetes manifest.
- You are evaluating or adopting disaggregated serving architectures with first-class Kubernetes support.
