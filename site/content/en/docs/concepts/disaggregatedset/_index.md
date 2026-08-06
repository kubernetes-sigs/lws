---
title: "DisaggregatedSet"
linkTitle: "DisaggregatedSet"
weight: 30
description: >
  Understanding DisaggregatedSet — purpose, relationship to LeaderWorkerSet, and when to use it.
---

**DisaggregatedSet** is a Kubernetes controller and CRD (Custom Resource Definition) that extends
LeaderWorkerSet (LWS) to support **disaggregated inference** workloads — use cases where different
roles (e.g., prefill, decode, encode) need to run on separate, independently-scaled
groups of pods.

This is especially useful for large language model (LLM) inference services where:

- The **prefill** role (generating the initial KV cache from the input prompt) is compute-bound and benefits from larger pod groups.
- The **decode** role (token-by-token autoregressive generation) is memory-bandwidth-bound and can run on smaller groups.
- The **encode** role (optional context encoding) may have different resource requirements from either.

DisaggregatedSet was introduced in
[KEP-766](https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet) to address
these multi-role, multi-resource serving patterns with a single, declarative Kubernetes resource.

![DisaggregatedSet concept](/images/ds-concept.svg)

## Relationship to LeaderWorkerSet

DisaggregatedSet does **not** replace LeaderWorkerSet — it **orchestrates one or more LeaderWorkerSets**.

Each `role` defined in a `DisaggregatedSet` spec maps to an independent `LeaderWorkerSet`, deployed
in the same namespace. This means:

| Feature | LeaderWorkerSet (LWS) | DisaggregatedSet |
|---|---|---|
| Unit | A single group of homogeneous pods | One or more groups, each with a distinct role |
| Use case | Uniform inference or training | Disaggregated prefill/decode/encode serving |
| CRD version | `leaderworkerset.x-k8s.io/v1` | `disaggregatedset.x-k8s.io/v1` |
| Controller namespace | `lws-system` | `lws-system` |
| Dependency | None | None (bundled in LWS from v0.9.0) |

Child LeaderWorkerSets use a **slice index** and a **revision hash** in their names:

```
DisaggregatedSet "my-inference"
├── roles[0]: prefill  →  LeaderWorkerSet "my-inference-0-<rev>-prefill"
├── roles[1]: decode   →  LeaderWorkerSet "my-inference-0-<rev>-decode"
└── roles[2]: encode   →  LeaderWorkerSet "my-inference-0-<rev>-encode"
```

Naming format: `<DisaggregatedSet-name>-<slice>-<revision-hash>-<role-name>`.
The revision hash is dynamic — always select child resources with labels
(`disaggregatedset.x-k8s.io/name`, `disaggregatedset.x-k8s.io/role`,
`disaggregatedset.x-k8s.io/slice`) rather than hardcoding names.

Each child LWS inherits standard LWS capabilities such as subgroup policies,
exclusive placement, volume claim templates, and health monitoring. Rollout
strategy for the set is owned by the DisaggregatedSet controller (see below).

## Roles in DisaggregatedSet

A `DisaggregatedSet` spec contains a `roles` list. Each role defines:

| Field | Description |
|---|---|
| `name` | Unique name for this role (e.g., `prefill`, `decode`) |
| `replicas` | Number of LWS replicas (pod groups) for this role (per slice) |
| `rolloutStrategy` | Rolling update config for this role (DisaggregatedSet coordinates rollouts across roles; `partition`-based rollout is not supported) |
| `leaderWorkerTemplate` | Pod template defining leader + worker containers |
| `scaling` | Optional external scaling mode (for example HPA via `DisaggregatedSetRoleScaler`) |
| `subRoles` | Optional routing pools that share this role's template but have independent replica targets or scalers |

DisaggregatedSet coordinates lifecycle and rollouts across roles. Each role's replica count,
rollout strategy, and pod template can be configured independently, while the controller manages
them as a single cohesive unit.

Sub-roles are useful when pools need the same runtime configuration but different routing queues,
such as short-context and long-context decode. They share one physical LWS; the parent replica
count is the sum of the sub-role targets. The controller assigns each LWS group with the mutable
`disaggregatedset.x-k8s.io/subrole` Pod label, so changing pool membership does not restart Pods.
When `subRoles` is present, replica ownership moves to the sub-roles and parent `replicas` is
ignored.

Optional top-level fields such as `slices` (replicate the full role topology) and
`placementPolicy` (topology co-location / spread) are covered in the
[examples and operations guide](/docs/examples/disaggregatedset/).

## When to Use DisaggregatedSet vs Plain LWS

Use **plain LWS** when:
- All inference pods are homogeneous (same model, same resources).
- You do not need to separate prefill from decode.
- You are running training jobs or batch workloads without disaggregation.

Use **DisaggregatedSet** when:
- You are running disaggregated LLM inference (e.g., vLLM with P/D disaggregation, SGLang).
- Different inference phases require different GPU types or different pod group sizes.
- You want to scale prefill and decode replicas independently based on different traffic patterns.
- You need independently routed pools that share one role configuration.
- You are evaluating disaggregated serving architectures and need first-class Kubernetes support.

## Key Design Principles

1. **LWS-native** — DisaggregatedSet is built on top of LWS, not alongside it. This means LWS features (failure handling, subgroup topology, exclusive placement) are available per role. Note: rollout strategy is owned by the DisaggregatedSet controller, which replaces the per-LWS rollout to coordinate updates across roles.

2. **Coordinated rollouts** — Rollouts across roles are coordinated by DisaggregatedSet to preserve capacity ratios (e.g., prefill-to-decode ratio) throughout the update process. Partition-based rollout is not supported.

3. **Declarative** — The entire multi-role inference topology is expressed in a single YAML manifest, making it easy to version-control and apply via GitOps.

## Further Reading

- [KEP-766: DisaggregatedSet design document](https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet)
- [Installation guide](/docs/installation/#disaggregatedset)
- [API Reference](/docs/reference/disaggregatedset.v1/)
- [Labels and annotations](/docs/reference/labels-annotations-and-environment-variables/)
- [Examples and operations](/docs/examples/disaggregatedset/)
- In-repo samples: [`config/samples/disaggregatedset_v1_disaggregatedset.yaml`](https://github.com/kubernetes-sigs/lws/blob/main/config/samples/disaggregatedset_v1_disaggregatedset.yaml), [`config/samples/disaggregatedset_v1_3role.yaml`](https://github.com/kubernetes-sigs/lws/blob/main/config/samples/disaggregatedset_v1_3role.yaml)
