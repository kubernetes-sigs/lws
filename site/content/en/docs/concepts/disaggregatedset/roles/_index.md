---
title: "Roles in DisaggregatedSet"
linkTitle: "Roles"
weight: 10
description: >
  Configuring roles (prefill, decode, encode) and their independent pod specifications in DisaggregatedSet.
---

A **Role** in DisaggregatedSet represents a distinct operational phase in a disaggregated serving architecture (e.g., `prefill`, `decode`, or `encode`). Each role defines its own pod specifications, scaling properties, and replica topology.

## Relationship to Child LeaderWorkerSets

Each role defined in a `DisaggregatedSet` specification maps directly to an independent child `LeaderWorkerSet` managed by the DisaggregatedSet controller:

```
DisaggregatedSet "my-inference"
├── roles[0]: prefill  →  LeaderWorkerSet "my-inference-0-<rev>-prefill"
├── roles[1]: decode   →  LeaderWorkerSet "my-inference-0-<rev>-decode"
└── roles[2]: encode   →  LeaderWorkerSet "my-inference-0-<rev>-encode"
```

Child LeaderWorkerSets follow the naming convention:
`<DisaggregatedSet-name>-<slice>-<revision-hash>-<role-name>`

{{% alert title="Note" color="info" %}}
The revision hash in the child resource name is dynamic across updates. Always select child resources using Kubernetes labels (`disaggregatedset.x-k8s.io/name`, `disaggregatedset.x-k8s.io/role`, `disaggregatedset.x-k8s.io/slice`) rather than hardcoding names.
{{% /alert %}}

---

## Role Configuration Fields

A `DisaggregatedSet` spec defines a `roles` list where each entry represents a role (`DisaggregatedRoleSpec`):

| Field | Type | Description |
| :--- | :--- | :--- |
| `name` | `string` | Unique name for this role within the set (e.g., `prefill`, `decode`, `encode`). |
| `scaling` | `RoleScaling` | Optional scaling configuration. `scaling.mode: External` delegates replica management to an external autoscaler (e.g., HPA/KEDA via `DisaggregatedSetRoleScaler`). Default is `Static`. |
| `spec.replicas` | `*int32` | Number of LWS replicas (pod groups) for this role per slice. |
| `spec.leaderWorkerTemplate` | `LeaderWorkerTemplate` | Full pod template defining the leader and worker pod containers, resource requests/limits, restart policies, and subgroup configurations for this role. |
| `spec.rolloutStrategy` | `RolloutStrategy` | Optional rolling update configuration for this role. DisaggregatedSet coordinates updates across roles to maintain capacity ratios. |

---

## Example Multi-Role Configuration

Here is an example `DisaggregatedSet` defining independent `prefill` and `decode` roles with different pod group sizes and hardware accelerator configurations:

{{< include file="examples/disaggregatedset/roles/prefill-decode.yaml" lang="yaml" >}}

---

## Independent Per-Role Capabilities

Because each role maps to an independent child LeaderWorkerSet, each role inherits all core LWS features tailored to its specific workload phase:

1. **Heterogeneous Hardware:**
   Prefill servers can run on high-bandwidth, high-compute accelerator nodes (e.g., 8-GPU tensor-parallel groups), while decode servers run on memory-optimized nodes (e.g., 2-GPU or 4-GPU groups).
2. **Independent Group Sizes:**
   Each role configures its own `leaderWorkerTemplate.size` and optional `subGroupPolicy`.
3. **Independent Autoscaling:**
   Prefill and decode roles can be scaled dynamically based on distinct metric signals (e.g., time-to-first-token vs. inter-token-latency) using `DisaggregatedSetRoleScaler`.
4. **Dedicated Storage:**
   Each role can configure its own `volumeClaimTemplates` for local model caching or intermediate tensor offloading.

## Virtual roles

`subRoles: [{name: interactive, replicas: 3}, {name: batch, replicas: 1}]` partitions
one parent's shared-template LWS into routing pools, totaling four groups per slice.
Parent `spec.replicas` is ignored and parent `scaling` must be omitted. Children may
use `scaling.mode: External` without `replicas`; their `<ds>-<parent>-<child>` RoleScalers
retain the single-slice restriction. Both Ordinal and Hash group identities are supported.

Route using the controller-owned `disaggregatedset.x-k8s.io/subrole` Pod label alongside
set/role/slice labels. Each child inherits independent rollout budgets; there is no parent cap.
Set `maxSurge` or `maxUnavailable` directly on a sub-role to override that setting:

```yaml
subRoles:
- name: interactive
  replicas: 3
  maxSurge: 1
  maxUnavailable: 0
- name: batch
  replicas: 1
  maxUnavailable: 1
```

Omitted settings still inherit the parent; explicit zero is not omission. Percentages
use the child's own replica target (surge rounds up, unavailability rounds down).
Changing these settings adjusts rollout pacing without creating a new revision.

`status.roleStatuses` lists parent aggregates, with current children nested in
`subRoleStatuses` using local names such as `interactive`. Use the parent total or the
child breakdown, not their sum. `SubRolesAssigned` reports coherent group assignment.
Labels can briefly duplicate during transfers. Ordinal requires a healthy retained prefix;
Hash prepares assignments using accepted per-child Ready floors. Pending operations block
further planning and desired changes until they complete.

Hash scale-down uses Kubernetes `controller.kubernetes.io/pod-deletion-cost` to prefer
removing unwanted groups. ReplicaSet first prefers unscheduled leaders, then compares
phase (Pending before Unknown before Running), then prefers not-Ready leaders, and only
then considers cost. This is intentional: keeping scheduled, healthy leaders takes
priority over our subrole preference. Selection remains best-effort: these checks concern
the leader, not the whole group, and ReplicaSet may not yet have observed a cost update.
For example, scaling `A=5,B=1` to `A=4,B=1` can temporarily remove B's only Ready group.
The controller observes the actual survivors and repairs their labels; it cannot guarantee
each child's availability budget during that interval. The planner still applies independent
budgets. The previous deletion cost is saved on the Pod and restored if it survives,
without overwriting newer external edits. There is no deletion webhook, and DisaggregatedSet
does not delete Pods directly. The normal default-enabled `PodDeletionCost` feature must be
enabled for the preference to apply.

See [KEP-960](https://github.com/kubernetes-sigs/lws/tree/main/keps/960-DisaggregatedSet-subroles)
for assignment ordering, recovery, rollout churn, and removing subroles.
