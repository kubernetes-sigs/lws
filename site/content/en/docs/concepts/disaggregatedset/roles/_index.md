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

> [!NOTE]
> The revision hash in the child resource name is dynamic across updates. Always select child resources using Kubernetes labels (`disaggregatedset.x-k8s.io/name`, `disaggregatedset.x-k8s.io/role`, `disaggregatedset.x-k8s.io/slice`) rather than hardcoding names.

---

## Role Configuration Fields

A `DisaggregatedSet` spec defines a `roles` list where each entry contains:

| Field | Type | Description |
| :--- | :--- | :--- |
| `name` | `string` | Unique name for this role within the set (e.g., `prefill`, `decode`, `encode`). |
| `replicas` | `*int32` | Number of LWS replicas (pod groups) for this role per slice. |
| `leaderWorkerTemplate` | `LeaderWorkerTemplate` | Full pod template defining the leader and worker pod containers, resource requests/limits, restart policies, and subgroup configurations for this role. |
| `rolloutStrategy` | `RolloutStrategy` | Optional rolling update configuration for this role. DisaggregatedSet coordinates updates across roles to maintain capacity ratios. |
| `scaling` | `RoleScalingMode` | Optional external scaling mode (e.g., enabling HPA via `DisaggregatedSetRoleScaler`). |

---

## Example Multi-Role Configuration

Here is an example `DisaggregatedSet` defining independent `prefill` and `decode` roles with different pod group sizes and hardware accelerator configurations:

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: disaggregatedset-sample
spec:
  roles:
  - name: prefill
    replicas: 2
    leaderWorkerTemplate:
      size: 4
      workerTemplate:
        spec:
          containers:
          - name: vllm-prefill
            image: vllm/vllm-openai:latest
            resources:
              limits:
                nvidia.com/gpu: "8"
  - name: decode
    replicas: 4
    leaderWorkerTemplate:
      size: 2
      workerTemplate:
        spec:
          containers:
          - name: vllm-decode
            image: vllm/vllm-openai:latest
            resources:
              limits:
                nvidia.com/gpu: "4"
```

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
