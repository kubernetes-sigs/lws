---
title: "Google Cloud TPU Integration"
linkTitle: "Google Cloud TPU"
weight: 10
description: >
  Automatic TPU worker discovery environment variable injection and multi-slice subgroup scoping across LeaderWorkerSet and DisaggregatedSet.
---

Multi-host Google Cloud TPU workloads (`libtpu`, JAX, PyTorch/XLA, vLLM-TPU, and SGLang) require each TPU worker process in a slice to know its zero-based worker ID and the ordered network addresses of every peer TPU worker in the same physical TPU slice.

Whenever a Pod managed by a `LeaderWorkerSet` — whether standalone (in `Ordinal` or `Hash` [group identity](../../leaderworkerset/group-identity/) mode) or created as a role of a `DisaggregatedSet` — requests `google.com/tpu` resources, LWS automatically injects the required TPU coordination environment variables at pod admission.

## Automatic Detection and Injected Environment Variables

When at least one container or init container in a Pod requests or limits `google.com/tpu > 0`, the LWS mutating pod webhook injects the following environment variables into the TPU container(s):

| Variable | Description | Example (`size: 2`, `UniquePerReplica` default subdomain) |
| :--- | :--- | :--- |
| `TPU_WORKER_ID` | Zero-based rank of the TPU worker within the group (or within its [subgroup](../../leaderworkerset/subgroups/) when `subGroupPolicy` is configured). | `0` |
| `TPU_WORKER_HOSTNAMES` | Comma-separated `<hostname>.<subdomain>` DNS names of all TPU workers in the group (or subgroup), ordered by `TPU_WORKER_ID`. | `tpu-serve-0.tpu-serve-0,tpu-serve-0-1.tpu-serve-0` |
| `TPU_PROCESS_ADDRESSES` | Comma-separated `<hostname>.<subdomain>:<port>` coordination endpoints matching `TPU_WORKER_HOSTNAMES`. | `tpu-serve-0.tpu-serve-0:8476,tpu-serve-0-1.tpu-serve-0:8476` |
| `TPU_PROCESS_PORT` | Coordination port for the container. Defaults to `8476` if not already set on the container. | `8476` |
| `TPU_NAME` | Identifier for the group's leader hostname (`<lws-name>-<ordinal>` in `Ordinal` mode, or `<lws-name>-<hash>` in `Hash` mode). | `tpu-serve-0` |

### Customizing or Overriding Injection

- **Custom coordination port (`TPU_PROCESS_PORT`):** If you set `TPU_PROCESS_PORT` on a container (for example, `8478`), LWS preserves your port value and uses it when constructing `TPU_PROCESS_ADDRESSES`.
- **Full manual override:** If a container already defines `TPU_WORKER_ID` or `TPU_WORKER_HOSTNAMES`, LWS skips TPU environment variable injection on that Pod so user-defined values are left untouched.

## Homogeneous vs. Heterogeneous (CPU Leader) Topologies

### 1. Homogeneous TPU Group (Leader Requests TPUs)

When `leaderTemplate` is omitted (or `leaderTemplate` also requests `google.com/tpu`), the leader Pod participates as `TPU_WORKER_ID=0`, and the controller marks the worker `StatefulSet` template with `leaderworkerset.sigs.k8s.io/leader-requests-tpus: "true"` so worker Pods offset their IDs accordingly (`1 .. size-1`).

Pair multi-host TPU slices with `leaderworkerset.sigs.k8s.io/exclusive-topology: cloud.google.com/gke-nodepool` (or [Kueue TAS](../../../examples/leaderworkerset/topology-aware-scheduling/)) so each replica's Pods co-locate on the same physical TPU slice node pool:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: tpu-serve
  annotations:
    leaderworkerset.sigs.k8s.io/exclusive-topology: cloud.google.com/gke-nodepool
spec:
  replicas: 2
  leaderWorkerTemplate:
    size: 4
    workerTemplate:
      spec:
        nodeSelector:
          cloud.google.com/gke-tpu-accelerator: tpu-v5-lite-podslice
          cloud.google.com/gke-tpu-topology: 4x4
        containers:
          - name: vllm
            image: vllm/vllm-tpu:latest
            resources:
              limits:
                google.com/tpu: "4"
```

### 2. CPU-Only Leader with TPU Workers

When `leaderTemplate` runs a CPU-only coordinator, router, or Ray head (no `google.com/tpu` request) and only `workerTemplate` requests `google.com/tpu`:

- The leader Pod does **not** receive `TPU_*` environment variables and is omitted from `TPU_WORKER_HOSTNAMES` and `TPU_PROCESS_ADDRESSES`.
- Worker Pods shift their ranks down by one (`TPU_WORKER_ID = 0 .. size-2`) so the TPU worker mesh remains contiguous starting at `0`.

## Multi-Slice TPU Topologies with Subgroups

A single TPU pod slice provides high-speed Inter-Chip Interconnect (ICI) within a fixed slice topology, while larger models (such as pipeline-parallel serving across multiple TPU slices) group several TPU slices into a single replica and communicate across slices over Data Center Networking (DCN).

When [Subgroups](../../leaderworkerset/subgroups/) (`spec.leaderWorkerTemplate.subGroupPolicy`) are configured:

- `TPU_WORKER_ID` (`0 .. subGroupSize-1`), `TPU_WORKER_HOSTNAMES`, and `TPU_PROCESS_ADDRESSES` are scoped **to each Pod's own subgroup** rather than the entire replica, matching the physical TPU slice boundary expected by `libtpu`.
- Pair `subGroupPolicy` with `leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology: cloud.google.com/gke-nodepool` so each subgroup lands on a dedicated TPU slice node pool, while Pods discover the replica-wide coordinator via `LWS_LEADER_ADDRESS`.
- With `subGroupPolicyType: LeaderExcluded` (for example, `size: 9`, `subGroupSize: 4` with a CPU-only `leaderTemplate`), the CPU leader is excluded from all TPU slice subgroups and the 8 worker Pods form two 4-host TPU slices (`TPU_WORKER_ID = 0..3` in each subgroup).

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: tpu-multislice
  annotations:
    leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology: cloud.google.com/gke-nodepool
spec:
  replicas: 1
  leaderWorkerTemplate:
    size: 9
    subGroupPolicy:
      subGroupPolicyType: LeaderExcluded
      subGroupSize: 4
    leaderTemplate:
      spec:
        containers:
          - name: coordinator
            image: rayproject/ray:latest
    workerTemplate:
      spec:
        nodeSelector:
          cloud.google.com/gke-tpu-accelerator: tpu-v5-lite-podslice
          cloud.google.com/gke-tpu-topology: 4x4
        containers:
          - name: worker
            image: vllm/vllm-tpu:latest
            resources:
              limits:
                google.com/tpu: "4"
```

## Multi-Container TPU Pods

- **Without `subGroupPolicy`:** If multiple containers in the same Pod request `google.com/tpu` (for example, running separate single-chip processes per container), LWS assigns sequential worker IDs (`podWorkerIndex * numContainers + containerIndex`) and sequential default ports (`8476 + containerIndex`, unless `TPU_PROCESS_PORT` is explicitly set on a container), listing the Pod's hostname once per TPU container in `TPU_WORKER_HOSTNAMES` and `TPU_PROCESS_ADDRESSES`. Leader and worker Pods that both request TPUs must define the same number of TPU containers.
- **With `subGroupPolicy`:** LWS injects `TPU_*` variables into the first container requesting `google.com/tpu` in each Pod.

## Using TPUs in `DisaggregatedSet`

Because each role in a [DisaggregatedSet](../../disaggregatedset/) is reconciled as a child `LeaderWorkerSet`, TPU environment variable injection works identically on `DisaggregatedSet` roles (such as separate TPU `prefill` and `decode` topologies with independent `size`, `subGroupPolicy`, and `cloud.google.com/gke-tpu-topology` selectors). Each role replica receives isolated `TPU_WORKER_HOSTNAMES` and `TPU_PROCESS_ADDRESSES` scoped to its own group or subgroup.
