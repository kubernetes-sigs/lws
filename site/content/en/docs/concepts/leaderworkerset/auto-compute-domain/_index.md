---
title: "Automatic ComputeDomain Management (MNNVL)"
linkTitle: "ComputeDomain (MNNVL)"
weight: 45
description: >
  Automatic NVIDIA DRA ComputeDomain lifecycle and pod claim injection for Multi-Node NVLink (MNNVL) workloads.
---

Multi-node GPU systems connected by Multi-Node NVLink (MNNVL), such as NVIDIA
GB200 and GB300 NVL72 racks, require pods sharing GPU memory (`cuda_ipc`,
NCCL/NVSHMEM collectives, or NIXL NVLink KV-cache transfer) to join a shared
IMEX domain backed by an NVIDIA DRA `ComputeDomain` (`resource.nvidia.com/v1beta1`).

When the `AutoComputeDomain` feature gate is enabled (Alpha), LWS automatically
creates, isolates across revisions and replica restarts, injects pod DRA claims
for, and garbage-collects `ComputeDomain` resources at two scopes:

1. **Per `LeaderWorkerSet` replica** (`leaderworkerset.sigs.k8s.io/auto-compute-domain: "enabled"`):
   one `ComputeDomain` per active replica, shared by the replica's leader and
   worker GPU containers.
2. **Per `DisaggregatedSet` slice** (`disaggregatedset.x-k8s.io/auto-compute-domain: "enabled"`):
   one `ComputeDomain` per active `(slice, revision)`, shared across enrolled
   roles (such as `prefill` and `decode`) within the same slice.

## Prerequisites and Installation

1. **Kubernetes and NVIDIA DRA driver**: A cluster with Dynamic Resource
   Allocation (DRA) enabled and the NVIDIA GPU DRA driver serving
   `computedomains.resource.nvidia.com/v1beta1`.
2. **Helm RBAC and Feature Gate**: Enable `computeDomainManagement.enabled=true`
   and the `AutoComputeDomain` feature gate when installing LWS:

```bash
helm upgrade --install lws oci://registry.k8s.io/lws/charts/lws \
  --namespace lws-system --create-namespace \
  --set computeDomainManagement.enabled=true \
  --set featureGates.AutoComputeDomain=true
```

When deploying from kustomize manifests instead of Helm, enable `AutoComputeDomain: true`
in the controller manager `Configuration` and grant RBAC permissions on
`computedomains` and `computedomains/finalizers` in group `resource.nvidia.com`:

```yaml
apiVersion: config.lws.x-k8s.io/v1
kind: Configuration
featureGates:
  AutoComputeDomain: true
```

> **Note:** An IMEX channel only connects GPUs within the same physical NVLink
> clique (typically labeled `nvidia.com/gpu.clique`). Pair `auto-compute-domain`
> with topology placement (such as `leaderworkerset.sigs.k8s.io/exclusive-topology`
> or `DisaggregatedSet.spec.placementPolicy`) so pods sharing a `ComputeDomain`
> co-locate on the same NVLink clique.

## Option 1: Per-Replica ComputeDomain (`LeaderWorkerSet`)

Set `leaderworkerset.sigs.k8s.io/auto-compute-domain: "enabled"` on a
`LeaderWorkerSet` to allocate one isolated `ComputeDomain` per replica:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: deepseek-r1
  annotations:
    leaderworkerset.sigs.k8s.io/auto-compute-domain: "enabled"
    leaderworkerset.sigs.k8s.io/exclusive-topology: nvidia.com/gpu.clique
spec:
  replicas: 2
  leaderWorkerTemplate:
    size: 4
    workerTemplate:
      spec:
        containers:
          - name: vllm
            image: nvcr.io/nvidia/vllm:latest
            resources:
              limits:
                nvidia.com/gpu: "4"
          - name: routing-proxy
            image: envoyproxy/envoy:v1.31-latest
```

LWS creates a dedicated `ComputeDomain` (`numNodes: 0`) for each replica and
injects the `lws-compute-domain-channel` `ResourceClaim` only into containers
requesting `nvidia.com/gpu` (automatically skipping CPU-only sidecars such as
`routing-proxy` and CPU-only `leaderTemplate`s).

## Option 2: Per-Slice ComputeDomain (`DisaggregatedSet`)

Set `disaggregatedset.x-k8s.io/auto-compute-domain: "enabled"` on a
`DisaggregatedSet` when roles within each slice transfer KV cache directly over
NVLink:

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: llm-serving
  annotations:
    disaggregatedset.x-k8s.io/auto-compute-domain: "enabled"
spec:
  slices: 2
  placementPolicy:
    sliceTopology:
      topologyKey: nvidia.com/gpu.clique
      policy: ExclusiveSlice
  roles:
    - name: prefill
      spec:
        replicas: 2
        leaderWorkerTemplate:
          size: 2
          workerTemplate:
            spec:
              containers:
                - name: prefill
                  image: nvcr.io/nvidia/vllm:latest
                  resources:
                    limits:
                      nvidia.com/gpu: "4"
    - name: decode
      spec:
        replicas: 1
        leaderWorkerTemplate:
          size: 4
          workerTemplate:
            spec:
              containers:
                - name: decode
                  image: nvcr.io/nvidia/vllm:latest
                  resources:
                    limits:
                      nvidia.com/gpu: "4"
    - name: encoder
      metadata:
        annotations:
          # Opt this role out of the slice ComputeDomain
          disaggregatedset.x-k8s.io/auto-compute-domain: "disabled"
      spec:
        replicas: 1
        leaderWorkerTemplate:
          size: 1
          workerTemplate:
            spec:
              containers:
                - name: encoder
                  image: nvcr.io/nvidia/vllm:latest
                  resources:
                    limits:
                      nvidia.com/gpu: "1"
```

### Per-Role Scope Matrix in `DisaggregatedSet`

- **Intra-role MNNVL with RDMA KV transfer**: Leave `disaggregatedset.x-k8s.io/auto-compute-domain`
  unset on the `DisaggregatedSet` and set
  `leaderworkerset.sigs.k8s.io/auto-compute-domain: "enabled"` on individual
  roles in `spec.roles[i].metadata.annotations`. Each role replica receives its
  own `ComputeDomain`.
- **Opting a role out of a slice domain**: Set
  `disaggregatedset.x-k8s.io/auto-compute-domain: "disabled"` on that role's
  `metadata.annotations` (optionally pairing it with
  `leaderworkerset.sigs.k8s.io/auto-compute-domain: "enabled"` if the role needs
  its own per-replica domain instead).

## Explicit Container Targeting for DRA-Allocated GPUs

By default, LWS targets containers (and restartable `initContainers` with
`restartPolicy: Always`) requesting `nvidia.com/gpu > 0`. When GPUs themselves
are allocated via DRA `ResourceClaim`s rather than `nvidia.com/gpu` extended
resources, annotate the `PodTemplateSpec` with
`leaderworkerset.sigs.k8s.io/compute-domain-containers`:

```yaml
workerTemplate:
  metadata:
    annotations:
      leaderworkerset.sigs.k8s.io/compute-domain-containers: "vllm"
  spec:
    containers:
      - name: vllm
        image: nvcr.io/nvidia/vllm:latest
```
