# DisaggregatedSet Operator

A Kubernetes operator for managing disaggregated inference deployments. DisaggregatedSet orchestrates multiple [LeaderWorkerSets](https://github.com/kubernetes-sigs/lws) with coordinated lifecycle management, specifically designed for disaggregated LLM inference architectures.

## Overview

Disaggregated serving separates the phases of LLM inference—such as **prefill** (processing the input prompt) and **decode** (generating output tokens)—onto different infrastructure. This optimization leverages the different computational characteristics of each phase. Serving frameworks like [vLLM](https://github.com/vllm-project/vllm) and [SGLang](https://github.com/sgl-project/sglang) support this architecture.

DisaggregatedSet simplifies deploying these workloads by:

- **Unified Management**: Manage multiple roles (prefill, decode, etc.) as a single resource
- **Coordinated Rolling Updates**: N-dimensional rollout algorithm updates all roles in lockstep
- **Automatic Service Discovery**: Headless service auto-created for each role
- **Flexible Role Configuration**: Support for 2-10 roles with arbitrary names
- **Stateless Operator**: Safe to restart at any point during operations

## Features

### N-Dimensional Rolling Updates

Disaggregated deployments often use specific role ratios (e.g., 5P2D means 5 prefill and 2 decode replicas). During rolling updates, the operator maintains this ratio as closely as possible using a **linear interpolation algorithm**.

**How it works:**

1. **Compute total steps** based on the largest role: `totalSteps = max(role[i].replicas for all i)`
2. **Linear interpolation** determines target replicas at each step:
   - New replicas: `ceil(step * target / totalSteps)` — scales up from 0 to target
   - Old replicas: `source - floor(step * source / totalSteps)` — scales down from source to 0
3. **N-dimensional coordination** keeps roles in sync:
   - Scale-up uses `min(role[i].step)` — all roles progress together
   - Scale-down uses `max(role[i].step)` — all roles drain together
4. **Decoupled steps**: each reconciliation changes EITHER old OR new replicas, not both

**Key properties:**

- **Ratio preservation**: A 5P2D deployment stays approximately 5:2 throughout the rollout
- **Scale-up before scale-down**: New replicas are created before old ones are removed (ensures capacity)
- **Per-role surge constraints**: Respects `maxSurge` and `maxUnavailable` independently per role
- **Coordinated drain**: If any role reaches 0 replicas, all roles are forced to 0 (prevents orphaned workloads)
- **Stability check**: Waits for `replicas == readyReplicas` before proceeding to next step

Visualize the rollout plan with the `plan-steps` CLI:

```bash
go run ./hack/plan-steps \
  -source '{"prefill": 5, "decode": 2}' \
  -target '{"prefill": 5, "decode": 2}' \
  -surge '{"prefill": 2, "decode": 2}' \
  -unavailable '{"prefill": 1, "decode": 1}'
```

```
Roles: [decode prefill]
Source: decode=2, prefill=5
Target: decode=2, prefill=5
Config: decode(surge=2, unavail=1), prefill(surge=2, unavail=1)

┌──────┬────────────┬─────────────┬────────────┬─────────────┬───────┬───────────────────────────────┐
│ STEP │ OLD DECODE │ OLD PREFILL │ NEW DECODE │ NEW PREFILL │ TOTAL │            ACTION             │
├──────┼────────────┼─────────────┼────────────┼─────────────┼───────┼───────────────────────────────┤
│ 0    │ 2          │ 5           │ 0          │ 0           │ 7     │ initial                       │
│ 1    │ 2          │ 5           │ 1          │ 2           │ 10    │ new decode +1, new prefill +2 │
│ 2    │ 2          │ 4           │ 1          │ 2           │ 9     │ old prefill -1                │
│ 3    │ 2          │ 3           │ 1          │ 2           │ 8     │ old prefill -1                │
│ 4    │ 2          │ 3           │ 2          │ 4           │ 11    │ new decode +1, new prefill +2 │
│ 5    │ 1          │ 2           │ 2          │ 4           │ 9     │ old decode -1, old prefill -1 │
│ 6    │ 1          │ 2           │ 2          │ 5           │ 10    │ new prefill +1                │
│ 7    │ 0          │ 0           │ 2          │ 5           │ 7     │ old decode -1, old prefill -2 │
└──────┴────────────┴─────────────┴────────────┴─────────────┴───────┴───────────────────────────────┘
```

### Automatic Service Discovery

A headless service is automatically created for each role, enabling direct pod-to-pod communication. Services are named `{disaggregatedset-name}-{role-name}` (e.g., `my-llm-prefill`, `my-llm-decode`).

## Installation

### Prerequisites

- Kubernetes v1.27+
- [LeaderWorkerSet](https://github.com/kubernetes-sigs/lws) CRD installed

Install LWS first:
```bash
kubectl apply --server-side -f https://github.com/kubernetes-sigs/lws/releases/latest/download/manifests.yaml
```

### Using Helm

```bash
helm install disaggregatedset ./charts/disaggregatedset
```

### Using YAML Manifest

```bash
kubectl apply -f https://raw.githubusercontent.com/<org>/disaggregatedset/<tag>/dist/install.yaml
```

## Usage

### Minimal Example

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1alpha1
kind: DisaggregatedSet
metadata:
  name: my-llm
spec:
  roles:
  - name: prefill
    replicas: 3
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        spec:
          containers:
          - name: inference
            image: my-registry/vllm:latest
  - name: decode
    replicas: 3
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        spec:
          containers:
          - name: inference
            image: my-registry/vllm:latest
```

### Full Example (with Rollout Configuration)

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1alpha1
kind: DisaggregatedSet
metadata:
  name: my-llm
spec:
  roles:
  - name: prefill
    replicas: 5
    rolloutStrategy:
      rollingUpdateConfiguration:
        maxSurge: 2
        maxUnavailable: 1
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        spec:
          containers:
          - name: inference
            image: my-registry/vllm:latest
            resources:
              limits:
                nvidia.com/gpu: 8
  - name: decode
    replicas: 2
    rolloutStrategy:
      rollingUpdateConfiguration:
        maxSurge: 1
        maxUnavailable: 1
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        spec:
          containers:
          - name: inference
            image: my-registry/vllm:latest
            resources:
              limits:
                nvidia.com/gpu: 2
```

See [`config/samples/`](config/samples/) for more examples.

### Key Spec Fields

| Field | Description |
|-------|-------------|
| `spec.roles` | Array of role configurations (minimum 2, maximum 10) |
| `spec.roles[].name` | Unique name for the role (e.g., "prefill", "decode") |
| `spec.roles[].replicas` | Number of leader-worker groups |
| `spec.roles[].leaderWorkerTemplate` | Pod template (inherited from LWS) |
| `spec.roles[].rolloutStrategy` | Rolling update configuration |
| `spec.roles[].networkConfig` | Network configuration (inherited from LWS) |
| `spec.roles[].startupPolicy` | Startup policy for pods (inherited from LWS) |
| `spec.roles[].metadata` | Labels/annotations for the LWS CR (for Kueue, exclusive-topology) |

### Validation Rules

The API enforces these validation rules:
- **Minimum 2 roles** required (maximum 10)
- **Role names must be unique**
- **Replicas must be consistent**: Either all roles have 0 replicas, or all have >0 replicas

### Metadata Field (Kueue/Topology Integration)

The `metadata` field allows setting labels/annotations on the generated LWS CRs:

```yaml
spec:
  roles:
  - name: prefill
    replicas: 2
    metadata:
      labels:
        kueue.x-k8s.io/queue-name: gpu-queue
        leaderworkerset.sigs.k8s.io/exclusive-topology: cloud.provider.com/topology-block
    leaderWorkerTemplate:
      # ...
```

### Status Fields

| Field | Description |
|-------|-------------|
| `status.roleStatuses[].name` | Role name |
| `status.roleStatuses[].replicas` | Total replicas for the role |
| `status.roleStatuses[].readyReplicas` | Ready replicas |
| `status.roleStatuses[].updatedReplicas` | Replicas at current revision |
| `status.conditions` | Standard Kubernetes conditions (Available, Progressing, Degraded) |

### Labels and Revision Hash

The operator applies these labels to managed resources:

| Label | Description |
|-------|-------------|
| `disaggregatedset.x-k8s.io/name` | DisaggregatedSet name |
| `disaggregatedset.x-k8s.io/role` | Role name (e.g., `prefill`, `decode`) |
| `disaggregatedset.x-k8s.io/revision` | Revision hash for rollout tracking |

**Revision hash**: The revision is a truncated SHA256 hash computed from **all** role `leaderWorkerTemplate` fields (serialized as JSON). This means:
- Changing any role's template triggers a new revision
- All roles always roll out together with the same revision
- LeaderWorkerSets are named `{name}-{revision}-{role}` (e.g., `my-llm-a1b2c3d4-prefill`)

## Architecture

```
DisaggregatedSet
       │
       ├── LeaderWorkerSet (prefill)
       │       └── Pods (prefill-0, prefill-1, ...)
       │
       ├── LeaderWorkerSet (decode)
       │       └── Pods (decode-0, decode-1, ...)
       │
       ├── Service (prefill) ── headless, auto-created
       └── Service (decode)  ── headless, auto-created
```

The operator:
1. Computes a revision hash from all role pod templates (SHA256 of JSON-serialized leaderWorkerTemplates)
2. Creates/updates LeaderWorkerSets for each role
3. Coordinates rolling updates using the N-dimensional linear interpolation algorithm
4. Creates headless Services for each role
5. Cleans up old resources when fully drained

## Development

### Build and Run

```bash
make build              # Build manager binary
make run                # Run operator locally
make install            # Install CRDs into cluster
make deploy IMG=<image> # Deploy to cluster
```

### Testing

```bash
make test               # Run unit tests
make test-e2e           # Run e2e tests (requires Kind)
```

### Code Generation

```bash
make manifests          # Generate CRDs, RBAC
make generate           # Generate DeepCopy methods
```

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.
