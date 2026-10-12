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

DisaggregatedSet does **not** replace LeaderWorkerSet — it **orchestrates multiple LeaderWorkerSets**.

Each `role` defined in a `DisaggregatedSet` spec maps to an independent `LeaderWorkerSet`, deployed
in the same namespace. Child LeaderWorkerSets use a **slice index** and a **revision hash** in their names:

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

## Key Design Principles

1. **LWS-native** — DisaggregatedSet is built on top of LWS, not alongside it. This means LWS features (failure handling, subgroup topology, exclusive placement) are available per role. Note: rollout strategy is owned by the DisaggregatedSet controller, which replaces the per-LWS rollout to coordinate updates across roles.

2. **Coordinated rollouts** — Rollouts across roles are coordinated by DisaggregatedSet to preserve capacity ratios (e.g., prefill-to-decode ratio) throughout the update process. Partition-based rollout is not supported.

3. **Declarative** — The entire multi-role inference topology is expressed in a single YAML manifest, making it easy to version-control and apply via GitOps.

## Scaling during a rollout

To apply changing replica targets while old and target revisions still overlap,
opt in on the DisaggregatedSet:

```yaml
spec:
  scalingPolicy:
    duringRollout: AdvanceRollout
```

This applies to both static `roles[].spec.replicas` and External
[RoleScaler](role-scaler) targets. Replica or policy edits do not create another
template revision. Omitting the policy, or choosing `RolloutCoupled`, preserves
the existing rollout behavior and defers target-revision reductions.

With `AdvanceRollout`, each reconciliation uses the latest target for the surge
ceiling (`target + maxSurge`) and availability floor (`target - maxUnavailable`,
clamped to zero). Raising a target does not make pending Pods Ready: the existing
pending-work and fractional-coordination bounds still apply. If observed Ready
capacity is already below the new floor, the planner does not deliberately
reduce it further.

Safe old-revision drains are preferred over target-revision reductions. If old
capacity cannot safely drain, excess target replicas can be reduced using the
same retained-readiness checks. Old and target revisions never spend the same
Ready credit in one decision. Completion requires exact target Spec and Ready,
including when the desired replica count is zero.

Ordinal and Hash use the same rule to preserve startup work. If any Pod in a target role is already assigned to a node, an ordinary target-size correction does not reduce that role below its final desired count. Pending Pods may already be assigned and loading a model; they do not need to be Ready to receive this protection. Only a target whose groups are all confirmed unscheduled can be reduced further toward the latest surge ceiling. Incomplete observations keep the protective bound.

For example, with old Spec/Ready 4, target Spec 5, desired 4, `maxSurge: 1`, and `maxUnavailable: 0`, a target with scheduled Pods can reduce to 4 rather than 1. The controller temporarily keeps 8 total groups above the new ceiling of 5 until readiness allows old groups to drain. A mixed target keeps some unscheduled groups too; replicas genuinely above the final desired count can still be removed. This is based on observed scheduling state, not a reservation against a Pod becoming scheduled before deletion.

When old and target role names are disjoint, target readiness also replaces a
fraction of the old roles' availability baseline. A target reduction must retain
that already-used replacement credit: the target's own `maxUnavailable` cannot
spend an old role's budget. This uses the same retained-readiness checks, including
pending deletions and the no-worsening rule when availability is already low.

Scale changes can leave a role above its latest surge ceiling while historical
old-role fractions block its reduction. Under `AdvanceRollout`, an old drain
may bypass that fraction window only to release existing excess:
`max(0, totalOldSpec + targetSpec - desired - maxSurge)`. The exception cannot
drain below the ceiling, bypass availability or split required old roles.
Only roles with a positive desired count use this correction; removed roles
keep their old fraction window.
Ordinary retirement keeps its historical fraction window; pending-work budgets
keep their original size baseline. This is an opt-in scale correction, not an
extra growth allowance.

The existing bounded emergency behavior remains: an unavailable target role
can need one bootstrap surge slot; with a moving target, a Ready role blocked
by fractional coordination at its new ceiling can also need one slot. Slots
cannot be stacked while that role is above its ceiling or awaiting readiness.
The scheduler-unschedulable availability fallback remains a last resort after
ordinary work and bootstrap progress are unavailable.
