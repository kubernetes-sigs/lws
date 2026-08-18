---
title: "Slices in DisaggregatedSet"
linkTitle: "Slices"
weight: 20
description: >
  Replicating multi-role serving topologies with DisaggregatedSet slices.
---

A **Slice** in DisaggregatedSet (introduced in [KEP-846](https://github.com/kubernetes-sigs/lws/tree/main/keps/846-disaggregatedset-slices)) represents a complete, self-contained replica of the entire multi-role serving topology (e.g., a full pair of `prefill` and `decode` roles).

Configured via the top-level `spec.slices` field, slices allow you to scale and duplicate an entire disaggregated inference architecture as a single declarative unit.

```
DisaggregatedSet "my-inference" (spec.slices = 2)
│
├── Slice 0 (e.g., Rack A / Domain 0)
│   ├── Role "prefill" → LeaderWorkerSet "my-inference-0-<rev>-prefill"
│   └── Role "decode"  → LeaderWorkerSet "my-inference-0-<rev>-decode"
│
└── Slice 1 (e.g., Rack B / Domain 1)
    ├── Role "prefill" → LeaderWorkerSet "my-inference-1-<rev>-prefill"
    └── Role "decode"  → LeaderWorkerSet "my-inference-1-<rev>-decode"
```

---

## Why Use Slices?

### 1. Scaling Identical Topologies Without YAML Duplication
Running multiple identical copies of a multi-role serving setup previously required duplicating manifests or creating multiple `DisaggregatedSet` resources. With `spec.slices`, scaling from 1 to *N* identical copies is a single-line configuration change:

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: disaggregatedset-sample
spec:
  slices: 3
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
  - name: decode
    replicas: 4
    leaderWorkerTemplate:
      size: 2
      workerTemplate:
        spec:
          containers:
          - name: vllm-decode
            image: vllm/vllm-openai:latest
```

### 2. Accelerator Domain Confinement & Fault Isolation
Disaggregated inference architectures (such as vLLM or SGLang with KV-cache transfer) benefit when prefill and decode pods communicating with each other are confined to the same physical domain (e.g., an NVLink rack or high-bandwidth switch).
- Each slice provides a stable unit of identity that can be pinned or spread across physical topology domains.
- A failure or network disruption in one domain affects only that slice, while other slices continue serving traffic uninterrupted.

### 3. Independent Rolling Updates per Slice
Each slice operates as an independent rolling update domain:
- When a pod template or image is updated, the controller rolls out updates across slices independently.
- Each slice maintains a version-synchronized serving topology (e.g., prefill and decode are updated in lockstep within the slice).
- Transient version differences between separate slices during a rollout do not break intra-slice KV cache transfer.

### 4. Zero-Downtime Scaling
Modifying `spec.slices` is treated strictly as a scale operation:
- **Scale-Up:** Adding a slice (e.g., increasing `slices` from 2 to 3) creates the new slice at the current active revision without restarting or modifying existing slices.
- **Scale-Down:** Reducing `slices` deletes the highest-indexed slice and gracefully terminates its child LeaderWorkerSets and headless services.

---

## Naming and Identification

Child resources belonging to a slice include the slice index in their name and labels:

### Resource Naming Format
`<DisaggregatedSet-name>-<slice-index>-<revision-hash>-<role-name>`

Example for `slices: 2`:
- `my-inference-0-7f9b8c-prefill`
- `my-inference-0-7f9b8c-decode`
- `my-inference-1-7f9b8c-prefill`
- `my-inference-1-7f9b8c-decode`

### Kubernetes Labels
Every child resource is labeled with:
- `disaggregatedset.x-k8s.io/name`: Name of the parent DisaggregatedSet
- `disaggregatedset.x-k8s.io/slice`: Slice index (`"0"`, `"1"`, ...)
- `disaggregatedset.x-k8s.io/role`: Role name (`"prefill"`, `"decode"`)
