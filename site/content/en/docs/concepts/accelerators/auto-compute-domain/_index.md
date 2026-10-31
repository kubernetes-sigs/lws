---
title: "Automatic ComputeDomain Management (NVIDIA MNNVL)"
linkTitle: "NVIDIA GPU (ComputeDomain)"
weight: 20
description: >
  Automatic NVIDIA DRA ComputeDomain lifecycle and pod claim injection for Multi-Node NVLink (MNNVL) workloads across LeaderWorkerSet and DisaggregatedSet.
---

Multi-node GPU systems connected by Multi-Node NVLink (MNNVL), such as NVIDIA
GB200 and GB300 NVL72 racks, require pods sharing GPU memory (`cuda_ipc`,
NCCL/NVSHMEM collectives, or NIXL NVLink KV-cache transfer) to join a shared
IMEX domain backed by an NVIDIA DRA `ComputeDomain` (`resource.nvidia.com/v1beta1`).

When the `AutoComputeDomain` feature gate is enabled (Alpha), LWS automatically
creates, isolates across revisions and replica restarts, injects pod DRA claims
for, and garbage-collects `ComputeDomain` resources at two scopes:

1. **Per `LeaderWorkerSet` replica** (`leaderworkerset.sigs.k8s.io/auto-compute-domain: "enabled"`):
   one `ComputeDomain` per leader Pod, shared by the replica's leader and worker
   GPU containers. A restarted or replaced group gets a new `ComputeDomain`.
2. **Per `DisaggregatedSet` slice** (`disaggregatedset.x-k8s.io/auto-compute-domain: "enabled"`):
   one `ComputeDomain` per active `(slice, revision)`, shared across enrolled
   roles (such as `prefill` and `decode`) within the same slice.

## Prerequisites and Installation

1. **Kubernetes and NVIDIA DRA driver**: A cluster with Dynamic Resource
   Allocation (DRA) enabled and the NVIDIA GPU DRA driver serving
   `computedomains.resource.nvidia.com/v1beta1`. The controller manager exits at
   startup if the gate is enabled and this API is not served.
2. **Helm RBAC and Feature Gate**: Enable the `AutoComputeDomain` feature gate
   when installing LWS. The chart grants the ComputeDomain RBAC rules when
   either the gate or `computeDomainManagement.enabled` is set:

```bash
helm upgrade --install lws oci://registry.k8s.io/lws/charts/lws \
  --namespace lws-system --create-namespace \
  --set featureGates.AutoComputeDomain=true
```

When deploying from kustomize manifests instead of Helm, enable `AutoComputeDomain: true`
in the controller manager `Configuration`:

```yaml
apiVersion: config.lws.x-k8s.io/v1
kind: Configuration
featureGates:
  AutoComputeDomain: true
```

The kustomize manifests do not grant ComputeDomain permissions. Without them
the ComputeDomain informer cannot sync and the controller manager fails to
start. Add a ClusterRole bound to the controller manager's ServiceAccount
(`lws-controller-manager` in `lws-system` by default):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: lws-computedomain-manager
rules:
  - apiGroups: ["resource.nvidia.com"]
    resources: ["computedomains"]
    verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
  - apiGroups: ["resource.nvidia.com"]
    resources: ["computedomains/finalizers"]
    verbs: ["update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: lws-computedomain-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: lws-computedomain-manager
subjects:
  - kind: ServiceAccount
    name: lws-controller-manager
    namespace: lws-system
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
    type: ExclusiveSlice
    topology: nvidia.com/gpu.clique
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

## Lifecycle and Operational Behavior

- **Rollouts**: Setting or removing either `auto-compute-domain` annotation
  changes the workload revision, so it triggers a rolling update of all groups.
  Unset and `"disabled"` produce the same revision, so upgrading LWS does not
  restart existing workloads.
- **Ownership and cleanup**: The NVIDIA `ComputeDomain` controller tears down
  the IMEX `DaemonSet` as soon as a `ComputeDomain` is marked for deletion, so
  LWS never deletes one while a Pod references its `ResourceClaimTemplate`.
  Managed `ComputeDomain`s therefore carry no owner reference, which would let
  garbage collection delete them while Pods drain. The managing workload's UID
  is recorded in the `leaderworkerset.sigs.k8s.io/compute-domain-owner-uid`
  label instead. The `LeaderWorkerSet` deletes an obsolete replica
  `ComputeDomain` once its leader Pod is gone and no Pod references it (group
  restart, rolling update, or scale-down). The `DisaggregatedSet` deletes a
  slice `ComputeDomain` once no child `LeaderWorkerSet` remains for its
  `(slice, revision)` and no Pod references it. When the `LeaderWorkerSet` or
  `DisaggregatedSet` itself is deleted, LWS deletes its `ComputeDomain`s once
  their last Pod exits.
- **Protection finalizer**: Each managed `ComputeDomain` carries the
  `leaderworkerset.sigs.k8s.io/compute-domain-protection` finalizer. When a
  `ComputeDomain` is deleted directly, LWS removes the finalizer only once no
  Pod references its `ResourceClaimTemplate`.
- **Name collisions**: If a `ComputeDomain` with the generated name exists and
  is not managed by the same workload, LWS does not adopt it. It emits an
  `UnexpectedComputeDomainOwner` Warning event and keeps the leader gated or the
  slice unscaled.
- **Reserved names**: The `lws-compute-domain-channel` claim name and the
  `leaderworkerset.sigs.k8s.io/compute-domain-prefix` and
  `leaderworkerset.sigs.k8s.io/compute-domain-name` template annotations are
  managed by LWS. The admission webhook rejects them in user templates.
- **Admission**: Opting in is rejected while the feature gate is disabled or
  while the `ComputeDomain` API is not served. Objects that opted in earlier
  remain updatable.

### Disabling the Feature Gate

With the gate disabled, LWS stops creating `ComputeDomain`s and injecting
claims, and it no longer deletes `ComputeDomain`s or releases the protection
finalizer. Pod templates do not change when the gate is toggled, so running
groups are not restarted.

- New Pods of opted-in `LeaderWorkerSet`s start without a `ComputeDomain` claim.
- A `DisaggregatedSet` enrolled in slice `ComputeDomain`s does not create new
  child `LeaderWorkerSet`s for enrolled roles (for example on scale-up or a new
  revision), because they would lack the claim that existing slices carry.
  Reconciliation reports an error until the gate is re-enabled or the
  annotation is removed.

Before disabling the gate, remove the `auto-compute-domain` annotations and wait
for the managed `ComputeDomain`s to be deleted. Because managed `ComputeDomain`s
have no owner reference, those of a workload deleted while the gate is disabled
are not garbage collected; delete them by hand after their Pods are gone. A
`ComputeDomain` that is deleted while the gate is disabled stays in
`Terminating` until the `leaderworkerset.sigs.k8s.io/compute-domain-protection`
finalizer is removed by re-enabling the gate or by hand, after its Pods are gone.
