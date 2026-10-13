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

2. **Coordinated rollouts** — Rollouts across roles are coordinated by DisaggregatedSet to preserve capacity ratios (e.g., prefill-to-decode ratio) throughout the update process. Each role's `rolloutStrategy.rollingUpdateConfiguration` sets its `maxSurge` and `maxUnavailable`, defaulting to `maxSurge: 0, maxUnavailable: 1` like a LeaderWorkerSet. This avoids surge during ordinary rollout steps; [bootstrap recovery can still require spare capacity](#rollout-budgets-and-serving-capacity). Set `maxSurge: 1, maxUnavailable: 0` on a role to bring the new revision up before the old one drains. Partition-based rollout is not supported.

3. **Declarative** — The entire multi-role inference topology is expressed in a single YAML manifest, making it easy to version-control and apply via GitOps.

## Rollout budgets and serving capacity

The planner protects both individual Ready groups and serving capacity. A revision
contributes serving capacity only when every required role in that revision has
at least one Ready group. A new Decode group cannot use an old Prefill group as
its serving counterpart.

For a template-only update with fixed replica targets, each role's availability
floor is `max(0, replicas - maxUnavailable)`. Allowing one role to reach zero does
not override another role's positive serving floor. For example, deleting the
last old Prefill group also removes the old revision's serving Decode capacity,
even if its Decode Pods are still Ready.

Consider `1 Prefill / 2 Decode` with `maxSurge: 0` and `maxUnavailable: 1` on both
roles. Assume each group occupies one slot and the cluster has exactly three:

| Step | Old revision | New revision | Result |
| --- | --- | --- | --- |
| Initial state | 1P / 2D Ready | None | All three slots are occupied. |
| Drain one old Decode | 1P / 1D Ready | None | The old pair still serves. |
| Start new Decode | 1P / 1D Ready | 1D Ready | All slots are occupied again; the new revision cannot serve yet. |
| Bootstrap new Prefill | 1P / 1D Ready | 1P pending / 1D Ready | New Prefill cannot schedule. |

The controller cannot retire the last old Prefill without dropping serving
Decode capacity below its floor of one. Scheduler rejection can trigger an
emergency increase of `maxUnavailable`, but only for roles whose serving floor
is greater than one. It never lowers a positive floor to zero. In this example,
the floors are zero for Prefill and one for Decode, so the fallback cannot
unblock the rollout. The old pair keeps serving while the update remains stalled.

To progress, provide capacity for the new pair or explicitly permit downtime.
For this fixed `1P / 2D` example, keeping Prefill's `maxUnavailable: 1` and setting
Decode's `maxUnavailable: 2` makes both floors zero. With `maxSurge: 0` on both
roles, the controller can retire the old pair and start the replacement within
the three-slot capacity. This permits a service outage during replacement;
Prefill's budget alone does not authorize that outage.

## Scaling during a rollout

This applies to static `roles[].spec.replicas` and External [RoleScaler](role-scaler) targets. Changing replicas or the scaling policy does not create a new revision.

| `scalingPolicy.scaleDownDuringRollout` | Scale-up during a rollout | Scale-down during a rollout |
| --- | --- | --- |
| `Deferred` (default) | Adds target-revision replicas toward the requested count, within rollout budgets. | Defers shrinking already-created target replicas until old revisions drain. Old replicas can still be retired during the rollout. |
| `Immediate` | Adds target-revision replicas toward the requested count, within rollout budgets. | Tries to remove excess old replicas first. If they must remain to serve traffic, it can shrink excess target replicas instead. |

To allow scale-down before old revisions finish draining:

```yaml
spec:
  scalingPolicy:
    scaleDownDuringRollout: Immediate
```

Omitting the policy preserves `Deferred` behavior. `Immediate` means the controller considers reductions now, not that Pods disappear instantly: availability and startup safeguards can delay them.

For roles still required by the target, both old and target reductions protect `effective desired - maxUnavailable` Ready groups, with a minimum of zero. The effective count normally follows the latest request; under `Deferred`, an External role with overlapping old replicas keeps at least the already-created target count. Requesting more capacity raises this floor. If fewer groups are currently Ready, the controller preserves those groups while growing the target; a smaller old revision is not permission to reduce availability further.

If ordinary scale-up becomes blocked by coordination, either policy can use one temporary extra group per eligible role. That role's existing target groups must all be Ready and its total Spec must be at its surge ceiling. The extra cannot stack while the role remains over its ceiling or waits for readiness. This does not enable `Deferred` to shrink the target in response to a lower replica request.

When an `Immediate` target revision has more replicas than its latest desired count, both Ordinal and Hash follow the same rule for each role:

- If any target Pod is assigned to a node, even if it is still loading, keep at least the final desired count. The same protection applies when scheduling information is incomplete.
- If every target group is confirmed unscheduled, the count may go below desired toward `desired + maxSurge - old replicas`. Availability checks and the first group of each required role still limit that reduction. This does not shrink a target already at or below desired.

For example, per role: old Spec/Ready 4, target Spec 5 with none Ready, desired 4, `maxSurge: 1`, and `maxUnavailable: 0`. A scheduled target can reduce to 4; an entirely unscheduled target can reduce to 1. The first case keeps 8 total groups above the latest ceiling of 5 until new readiness permits old groups to drain, potentially for a long time if startup stalls.

This protects the replica count, not particular Pods. It can keep some unscheduled groups alongside scheduled ones; removing excess above desired can still delete scheduled work, and placement may change before deletion.

Renamed-role rollouts can start a bounded next batch using Ready replacements without waiting for another old group to leave; if a target role later loses all Ready groups, recovery under either policy may reclaim extra groups, even Ready ones, while preserving the last Ready group per role and protected old capacity. `Immediate` finishes when old replicas are gone and target Spec and Ready equal the latest desired count, including zero.

See [KEP-1022](https://github.com/kubernetes-sigs/lws/tree/main/keps/1022-DisaggregatedSet-Scaling-During-Rollout) for the exact budgets, renamed-role availability, and recovery rules.
