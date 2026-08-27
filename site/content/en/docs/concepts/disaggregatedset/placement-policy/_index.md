---
title: "Placement Policy in DisaggregatedSet"
linkTitle: "Placement Policy"
weight: 30
description: >
  Co-locating roles within slices and spreading slices across topology domains.
---

Disaggregated inference workloads have strict placement requirements:
- **Intra-Slice Locality:** Prefill and decode pods within the same slice exchange KV-cache data over high-speed networks and should be co-located within the same low-latency domain (such as an NVL72 rack or InfiniBand island).
- **Inter-Slice Isolation:** Slices of the same DisaggregatedSet should be spread across distinct physical domains so that hardware or power failures take down at most one slice.
- **Domain Exclusivity:** Highly sensitive production workloads may require exclusive access to an accelerator domain to prevent noisy-neighbor performance degradation.

DisaggregatedSet provides the `spec.placementPolicy` API (introduced in [KEP-848](https://github.com/kubernetes-sigs/lws/tree/main/keps/848-disaggregatedset-placement-policy)) to declaratively manage these constraints.

---

## Placement Policy Types

The `spec.placementPolicy` field defines the placement strategy and the target topology node-label key:

```yaml
spec:
  placementPolicy:
    type: ExclusiveSlice # or None, ExclusiveTopology
    topology: topology.kubernetes.io/rack
```

### 1. `None` (Default)
The controller injects no placement constraints. Pods for all roles and slices are scheduled according to standard Kubernetes scheduling rules and any explicit node selectors or affinities defined directly on the pod templates.

### 2. `ExclusiveSlice`
- **Co-location:** All roles (e.g. prefill and decode) belonging to the same slice are co-located in the same topology domain.
- **Spread:** Different slices of the same DisaggregatedSet are scheduled onto separate topology domains.
- **Sharing:** Slices from *other* DisaggregatedSets or other cluster workloads are permitted to share the domain if capacity allows.

**Use Case:** Cost-efficient production serving where you want low-latency KV-cache handoff within each slice and fault isolation across your slices, while allowing dense cluster bin-packing.

### 3. `ExclusiveTopology`
- **Co-location & Spread:** Performs everything `ExclusiveSlice` does (co-locates roles within a slice and spreads slices across domains).
- **Exclusivity Among DisaggregatedSets:** Ensures that a topology domain holds at most one slice across all DisaggregatedSets (a 1:1 domain-to-slice mapping). Pods from other DisaggregatedSet slices are prevented from landing in the same domain.

{{% alert title="Note" color="info" %}}
Exclusivity is enforced via injected pod anti-affinity targeting DisaggregatedSet labels. Unrelated, non-DisaggregatedSet workloads do not match this anti-affinity and can still share the domain unless node taints or dedicated node pools are configured.
{{% /alert %}}

**Use Case:** Production serving workloads that require dedicated accelerator domains without interference from other DisaggregatedSet slices.

---

## Comparison Matrix

| Policy Type | Intra-Slice Co-location | Inter-Slice Spread (Same Set) | Sharing with Other DisaggregatedSets |
| :--- | :---: | :---: | :---: |
| **`None`** | ❌ None | ❌ None | ✅ Allowed |
| **`ExclusiveSlice`** | ✅ Same domain | ✅ Different domains | ✅ Allowed |
| **`ExclusiveTopology`** | ✅ Same domain | ✅ Different domains | ❌ Disallowed (1:1 Domain Mapping) |

---

## Example Configuration

Below is an example DisaggregatedSet configured with `ExclusiveSlice` placement across physical racks:

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: disaggregatedset-sample
spec:
  slices: 2
  placementPolicy:
    type: ExclusiveSlice
    topology: topology.kubernetes.io/rack
  roles:
  - name: prefill
    spec:
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
    spec:
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

In this example:
- **Slice 0** (prefill + decode pods) lands together on **Rack A**.
- **Slice 1** (prefill + decode pods) lands together on **Rack B**.

---

## How It Works Under the Hood

The DisaggregatedSet controller translates `placementPolicy` into Kubernetes `podAffinity` and `podAntiAffinity` rules and injects them into the generated child `LeaderWorkerSet` pod templates:

1. **Pod Affinity (Co-location):**
   Injected so that pods matching the parent DisaggregatedSet name and slice index (`disaggregatedset.x-k8s.io/slice`) must be scheduled in the same `topologyKey` domain.
2. **Pod Anti-Affinity (Spread & Exclusivity):**
   Injected so that pods matching the parent DisaggregatedSet with a *different* slice index (or any DisaggregatedSet for `ExclusiveTopology`) cannot land in the same domain.
3. **Hardware Agnostic:**
   The `topology` field uses standard Kubernetes node labels (e.g., `topology.kubernetes.io/rack`, `cloud.google.com/gke-placement-group`, `topology.kubernetes.io/zone`), making the placement policy fully portable across GPU, TPU, and CPU clusters.
4. **Rollout Semantics:**
   Affinity rules are injected when child LeaderWorkerSets are created. Changing `placementPolicy` on an active DisaggregatedSet takes effect on the next rollout.
