---
title: "DisaggregatedSet"
linkTitle: "DisaggregatedSet"
weight: 20
description: >
  Learn about DisaggregatedSet, a higher-level abstraction for coordinating multi-role disaggregated inference workloads.
---

`DisaggregatedSet` is a Kubernetes Custom Resource Definition (CRD) introduced in LeaderWorkerSet (LWS) to manage and orchestrate disaggregated inference workloads. 

It acts as a higher-level controller over `LeaderWorkerSet` resources, enabling users to deploy, scale, and update multi-role serving architectures (such as prefill/decode splitting for Large Language Models) as a single logical unit.

For details on the initial proposal and design, see [KEP-766: DisaggregatedSet](https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet).

---

## Why Disaggregated serving?

In LLM serving, inference is split into two phases:
1. **Prefill (Prompt phase):** Compute-bound phase that processes input tokens and generates the first token. This phase benefits from high computation capacity (e.g., larger GPU counts or memory bandwidth).
2. **Decode (Generation phase):** Memory-bandwidth-bound phase that generates subsequent tokens one by one. This phase benefits from high memory speed but requires fewer compute resources per token.

By disaggregating these phases onto separate, specialized GPU pools, serving frameworks like [vLLM](https://github.com/vllm-project/vllm), [SGLang](https://github.com/sgl-project/sglang), and [NVIDIA Dynamo](https://github.com/ai-dynamo/dynamo) can optimize throughput, reduce inter-token latency, and improve hardware utilization.

---

## Concept and Architecture

A `DisaggregatedSet` defines the configuration for multiple **roles** (at least 2, and up to 10). Under the hood, the `DisaggregatedSet` controller automatically creates and manages a separate `LeaderWorkerSet` for each role.

### Naming Conventions

Managed `LeaderWorkerSet` resources are named using the following format:
```
{disaggregatedset-name}-{revision-hash}-{role-name}
```
- `revision-hash` is a truncated hash of all the role templates to ensure coordinated updates and revision tracking.
- `role-name` corresponds to the name of the role configured in the spec (e.g., `prefill`, `decode`).

### Labels and Annotations

The following labels are automatically applied to the managed LeaderWorkerSets and associated pods:
- `disaggregatedset.x-k8s.io/name`: The name of the parent `DisaggregatedSet`.
- `disaggregatedset.x-k8s.io/role`: The specific role the resource belongs to.
- `disaggregatedset.x-k8s.io/revision`: The revision hash representing the current template configuration.

An annotation `disaggregatedset.x-k8s.io/initial-replicas` is used internally by the controller on the `DisaggregatedSet` to track the starting replica counts during rollouts.

---

## Coordinated N-Dimensional Rolling Updates

Deploying updates across independent serving roles is challenging. If a prefill pool is updated to a new version/configuration while the decode pool remains on the old version, request processing can fail. 

`DisaggregatedSet` solves this by introducing a coordinated **N-dimensional rolling update algorithm**:

1. **Lockstep Rollouts:** The controller coordinates updates across all roles, scaling up new revisions and scaling down old revisions in lockstep.
2. **Surge and Availability Constraints:** The update honors per-role `maxSurge` and `maxUnavailable` configurations. 
3. **Scale-Up Priority:** New replicas are scaled up and verified to be `Ready` before old replicas are scaled down to ensure the cluster capacity never drops below minimum requirements.
4. **Coordinated Drain:** If a rollout is interrupted or if any role's replica count drops to `0`, the controller forces all related roles to `0` to prevent orphaned single-role workloads from wasting resources or receiving broken traffic.
