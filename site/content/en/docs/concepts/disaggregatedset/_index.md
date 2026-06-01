---
title: "DisaggregatedSet"
linkTitle: "DisaggregatedSet"
weight: 30
description: >
  Understanding DisaggregatedSet — purpose, relationship to LeaderWorkerSet, and when to use it.
---

<!-- toc -->
- [Overview](#overview)
- [Relationship to LeaderWorkerSet](#relationship-to-leaderworkerset)
- [Roles in DisaggregatedSet](#roles-in-disaggregatedset)
- [When to Use DisaggregatedSet vs Plain LWS](#when-to-use-disaggregatedset-vs-plain-lws)
- [Key Design Principles](#key-design-principles)
<!-- /toc -->

## Overview

**DisaggregatedSet** is a Kubernetes controller and CRD (Custom Resource Definition) that extends
LeaderWorkerSet (LWS) to support **disaggregated inference** workloads — use cases where different
phases of inference (e.g., prefill, decode, encode) need to run on separate, independently-scaled
groups of pods.

This is especially useful for large language model (LLM) inference services where:

- The **prefill** phase (generating the initial KV cache from the input prompt) is compute-bound and benefits from larger pod groups.
- The **decode** phase (token-by-token autoregressive generation) is memory-bandwidth-bound and can run on smaller groups.
- The **encode** phase (optional context encoding) may have different resource requirements from either.

DisaggregatedSet was introduced in
[KEP-766](https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet) to address
these multi-phase, multi-resource serving patterns with a single, declarative Kubernetes resource.

## Relationship to LeaderWorkerSet

DisaggregatedSet does **not** replace LeaderWorkerSet — it **orchestrates multiple LeaderWorkerSets**.

Each `role` defined in a `DisaggregatedSet` spec maps to an independent `LeaderWorkerSet`, deployed
in the same namespace. This means:

| Feature | LeaderWorkerSet (LWS) | DisaggregatedSet |
|---|---|---|
| Unit | A single group of homogeneous pods | Multiple groups, each with a distinct role |
| Use case | Uniform inference or training | Disaggregated prefill/decode/encode serving |
| CRD version | `leaderworkerset.x-k8s.io/v1` | `disaggregatedset.x-k8s.io/v1alpha1` |
| Controller namespace | `lws-system` | `disaggregatedset-system` |
| Dependency | None | Requires LWS to be installed first |

```
DisaggregatedSet
├── roles[0]: prefill  →  creates LeaderWorkerSet "disaggdeployment-xxx-prefill"
├── roles[1]: decode   →  creates LeaderWorkerSet "disaggdeployment-xxx-decode"
└── roles[2]: encode   →  creates LeaderWorkerSet "disaggdeployment-xxx-encode"
```

Each child LWS inherits all standard LWS capabilities: rolling updates, subgroup policies,
exclusive placement, volume claim templates, and health monitoring.

## Roles in DisaggregatedSet

A `DisaggregatedSet` spec contains a `roles` list. Each role defines:

| Field | Description |
|---|---|
| `name` | Unique name for this role (e.g., `prefill`, `decode`) |
| `replicas` | Number of LWS replicas (pod groups) for this role |
| `rolloutStrategy` | Independent rolling update config per role |
| `leaderWorkerTemplate` | Pod template defining leader + worker containers |

Roles are independent — you can scale, update, or restart a single role without affecting others.

## When to Use DisaggregatedSet vs Plain LWS

Use **plain LWS** when:
- All inference pods are homogeneous (same model, same resources).
- You do not need to separate prefill from decode.
- You are running training jobs or batch workloads without disaggregation.

Use **DisaggregatedSet** when:
- You are running disaggregated LLM inference (e.g., vLLM with P/D disaggregation, SGLang).
- Different inference phases require different GPU types or different pod group sizes.
- You want to scale prefill and decode replicas independently based on different traffic patterns.
- You are evaluating disaggregated serving architectures and need first-class Kubernetes support.

## Key Design Principles

1. **LWS-native** — DisaggregatedSet is built on top of LWS, not alongside it. This means all LWS features (failure handling, rollout strategy, subgroup topology) are available per role.

2. **Role isolation** — Each role's lifecycle, scaling, and rollout is fully independent. A failed decode role does not impact the prefill role.

3. **Declarative** — The entire multi-role inference topology is expressed in a single YAML manifest, making it easy to version-control and apply via GitOps.

4. **Alpha API** — DisaggregatedSet is currently at `v1alpha1`. The API is subject to change. Pin to a specific controller version in production environments.

## Further Reading

- [KEP-766: DisaggregatedSet design document](https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet)
- [Installation guide](/docs/installation/#installing-the-disaggregatedset-controller)
- [API Reference](/docs/reference/disaggregatedset.v1alpha1/)
- [Examples](/docs/examples/)
