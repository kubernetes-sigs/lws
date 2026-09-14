---
title: "DisaggregatedSet"
linkTitle: "DisaggregatedSet"
weight: 2
description: >
  Disaggregated (prefill/decode) inference with DisaggregatedSet, including
  disagg-aware rollouts, autoscaling, and topology placement.
---

DisaggregatedSet runs a multi-role inference deployment where each role is its
own child LeaderWorkerSet. The common pattern splits inference into a `prefill`
role for prompt processing and a `decode` role for token generation, so each one
scales and rolls out on its own.

These examples show four things. The `prefill` and `decode` roles are separate.
`spec.slices` fans the role set out into several independent slices. A per-role
`rolloutStrategy` upgrades each role on its own. `spec.placementPolicy`
co-locates a slice's roles in one topology domain and spreads slices across
domains.

Each guide isolates one feature and ships both a `vllm.yaml` and a `sglang.yaml`.

- [Basic](basic/): minimal prefill/decode disaggregation.
- [Autoscaling](autoscaling/): autoscale a role with a `DisaggregatedSetRoleScaler`
  and HPA.
- [Multi-slice](multi-slice/): fan the role set out into independent slices with
  `spec.slices`.
- [Topology-aware scheduling](topology-aware-scheduling/): co-locate a slice's
  roles with `spec.placementPolicy`.

See the [DisaggregatedSet concepts](../../concepts/disaggregatedset/) for the API
details.
