# DisaggregatedSet Operator

A Kubernetes operator for managing disaggregated inference deployments. DisaggregatedSet orchestrates multiple [LeaderWorkerSets](https://github.com/kubernetes-sigs/lws) with coordinated lifecycle management, specifically designed for disaggregated LLM inference architectures.

## Overview

Disaggregated serving separates the two phases of LLM inference—**prefill** (processing the input prompt) and **decode** (generating output tokens)—onto different infrastructure. This optimization leverages the different computational characteristics of each phase. Serving frameworks like [vLLM](https://github.com/vllm-project/vllm) and [SGLang](https://github.com/sgl-project/sglang) support this architecture.

DisaggregatedSet simplifies deploying these workloads by:

- **Unified Management**: Manage prefill and decode LeaderWorkerSets as a single resource
- **Coordinated Rolling Updates**: Two-dimensional rollout algorithm updates both sides in lockstep
- **Service Orchestration**: Automatically create Services when both sides are ready
- **Stateless Operator**: Safe to restart at any point during operations

## Features

### Two-Dimensional Rolling Updates

Disaggregated deployments often use specific prefill-to-decode ratios (e.g., 3P6D means 3 prefill and 6 decode replicas). During rolling updates, the operator maintains this ratio as closely as possible using a **linear interpolation algorithm**.

**How it works:**

1. **Compute total steps** based on the larger side: `totalSteps = max(prefillReplicas, decodeReplicas)`
2. **Linear interpolation** determines target replicas at each step:
   - New replicas: `ceil(step * target / totalSteps)` — scales up from 0 to target
   - Old replicas: `source - floor(step * source / totalSteps)` — scales down from source to 0
3. **Two-dimensional coordination** keeps sides in sync:
   - Scale-up uses `min(prefillStep, decodeStep)` — both sides progress together
   - Scale-down uses `max(prefillStep, decodeStep)` — both sides drain together
4. **Decoupled steps**: each reconciliation changes EITHER old OR new replicas, not both

**Key properties:**

- **Ratio preservation**: A 3P6D deployment stays approximately 1:2 throughout the rollout
- **Scale-up before scale-down**: New replicas are created before old ones are removed (ensures capacity)
- **Per-side surge constraints**: Respects `maxSurge` and `maxUnavailable` independently per side
- **Coordinated drain**: If either side reaches 0 replicas, both sides are forced to 0 (prevents orphaned single-side workloads)
- **Stability check**: Waits for `replicas == readyReplicas` before proceeding to next step

Visualize the rollout plan with the `plan-steps` CLI:

```bash
go run ./cmd/plan-steps \
  --source-prefill 3 --source-decode 3 \
  --target-prefill 3 --target-decode 3 \
  --prefill-surge 1 --decode-surge 1
```

```
Source: prefill=3, decode=3
Target: prefill=3, decode=3
Config: prefill(surge=1, unavail=0), decode(surge=1, unavail=0)

┌──────┬─────────────┬────────────┬─────────────┬────────────┬───────┬───────────────────────────────┐
│ STEP │ OLD PREFILL │ OLD DECODE │ NEW PREFILL │ NEW DECODE │ TOTAL │            ACTION             │
├──────┼─────────────┼────────────┼─────────────┼────────────┼───────┼───────────────────────────────┤
│ 0    │ 3           │ 3          │ 0           │ 0          │ 6     │ initial                       │
│ 1    │ 3           │ 3          │ 1           │ 1          │ 8     │ new prefill +1, new decode +1 │
│ 2    │ 2           │ 2          │ 1           │ 1          │ 6     │ old prefill -1, old decode -1 │
│ 3    │ 2           │ 2          │ 2           │ 2          │ 8     │ new prefill +1, new decode +1 │
│ 4    │ 1           │ 1          │ 2           │ 2          │ 6     │ old prefill -1, old decode -1 │
│ 5    │ 1           │ 1          │ 3           │ 3          │ 8     │ new prefill +1, new decode +1 │
│ 6    │ 0           │ 0          │ 3           │ 3          │ 6     │ old prefill -1, old decode -1 │
└──────┴─────────────┴────────────┴─────────────┴────────────┴───────┴───────────────────────────────┘
```

### Service Orchestration

Services are automatically created for each side when **both** prefill and decode are ready. This ensures traffic only flows to fully operational deployments.

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
  prefill:
    replicas: 3
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        spec:
          containers:
          - name: prefill
            image: my-registry/vllm-prefill:latest
  decode:
    replicas: 3
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        spec:
          containers:
          - name: decode
            image: my-registry/vllm-decode:latest
```

### Full Example (with Services and Rollout Configuration)

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1alpha1
kind: DisaggregatedSet
metadata:
  name: my-llm
spec:
  prefill:
    replicas: 3
    rolloutStrategy:
      maxSurge: 1
      maxUnavailable: 0
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        spec:
          containers:
          - name: prefill
            image: my-registry/vllm-prefill:latest
            resources:
              limits:
                nvidia.com/gpu: 1
  decode:
    replicas: 3
    rolloutStrategy:
      maxSurge: 1
      maxUnavailable: 0
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        spec:
          containers:
          - name: decode
            image: my-registry/vllm-decode:latest
            resources:
              limits:
                nvidia.com/gpu: 1
```

See [`config/samples/`](config/samples/) for more examples.

### Key Spec Fields

| Field | Description |
|-------|-------------|
| `spec.prefill` | Configuration for prefill side |
| `spec.decode` | Configuration for decode side |
| `spec.{side}.replicas` | Number of leader-worker groups |
| `spec.{side}.leaderWorkerTemplate` | Pod template (same as LWS) |
| `spec.{side}.rolloutStrategy` | Rolling update configuration |

### Labels and Revision Hash

The operator applies these labels to managed resources:

| Label | Description |
|-------|-------------|
| `disaggregatedset.x-k8s.io/name` | DisaggregatedSet name |
| `disaggregatedset.x-k8s.io/side` | `prefill` or `decode` |
| `disaggregatedset.x-k8s.io/revision` | Revision hash for rollout tracking |

**Revision hash**: The revision is a truncated SHA256 hash computed from **both** the prefill and decode `leaderWorkerTemplate` fields (serialized as JSON). This means:
- Changing either prefill OR decode template triggers a new revision
- Both sides always roll out together with the same revision
- LeaderWorkerSets are named `{name}-{revision}-{side}` (e.g., `my-llm-a1b2c3d4-prefill`)

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
       ├── Service (prefill) ── created when ready
       └── Service (decode)  ── created when ready
```

The operator:
1. Computes a revision hash from both pod templates (SHA256 of JSON-serialized leaderWorkerTemplates)
2. Creates/updates LeaderWorkerSets for each side
3. Coordinates rolling updates using the two-dimensional linear interpolation algorithm
4. Creates Services when both sides reach ready state
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
