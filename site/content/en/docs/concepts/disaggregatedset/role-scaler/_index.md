---
title: "Independent Role Autoscaling"
linkTitle: "Independent Role Autoscaling"
weight: 40
description: >
  Autoscaling individual DisaggregatedSet roles with HorizontalPodAutoscaler (HPA) and KEDA via DisaggregatedSetRoleScaler.
---

In disaggregated serving architectures, different roles experience fundamentally different bottlenecks:
- **Prefill:** Compute-bound and sensitive to request queue depth and prompt token volume.
- **Decode:** Memory-bandwidth-bound and sensitive to active batch size and KV-cache capacity.

To maximize accelerator utilization and cost efficiency, each role must be able to autoscale independently.

However, because child LeaderWorkerSets include dynamic revision hashes in their names (`<ds>-<slice>-<rev>-<role>`), pointing a standard Kubernetes `HorizontalPodAutoscaler` (HPA) directly at an underlying LWS breaks whenever a rolling update occurs.

**`DisaggregatedSetRoleScaler`** (`disaggregatedset.x-k8s.io/v1`, introduced in [KEP-849](https://github.com/kubernetes-sigs/lws/tree/main/keps/849-DisaggregatedSet-HPA)) solves this by exposing a stable `/scale` subresource target per role that persists across arbitrary rollouts.

---

## How It Works

```
┌─────────────────────────────────────────────────────────────┐
│             HPA / KEDA ScaledObject / Custom Scaler         │
│          (Target: kind=DisaggregatedSetRoleScaler)          │
└──────────────────────────────┬──────────────────────────────┘
                               │  /scale subresource
                               ▼
┌─────────────────────────────────────────────────────────────┐
│  DisaggregatedSetRoleScaler: "<ds-name>-<role-name>"        │
│  (Controller-managed, stable name across all revisions)     │
└──────────────────────────────┬──────────────────────────────┘
                               │  Reconciles desired replicas
                               ▼
┌─────────────────────────────────────────────────────────────┐
│                    DisaggregatedSet Controller              │
│       (Manages child LWS lifecycle, rollouts, and drain)    │
└──────────────────────────────┬──────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────┐
│  Child LeaderWorkerSet: "<ds-name>-0-<revision>-<role-name>"│
└─────────────────────────────────────────────────────────────┘
```

1. **Automatic Lifecycle:** When you set `scaling.mode: External` on a role, the DisaggregatedSet controller automatically creates and maintains a `DisaggregatedSetRoleScaler` named `<disaggregatedset-name>-<role-name>`. You do not need to author this resource manually.
2. **Stable Target:** The scaler's name remains constant regardless of how many rolling updates or revision changes occur.
3. **Reconciled Replicas:** The external autoscaler (HPA or KEDA) writes the desired replica count to the scaler's `/scale` endpoint. The DisaggregatedSet controller reads this value and scales the corresponding child LeaderWorkerSet accordingly.

---

## Configuration Example

### 1. Enable External Scaling on the Role

In your `DisaggregatedSet` manifest, set `scaling.mode: External` on the role(s) you wish to autoscale:

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: llm-serving
spec:
  roles:
  - name: prefill
    scaling:
      mode: External
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

### 2. Create an HPA Targeting the Role Scaler

Target the automatically created `DisaggregatedSetRoleScaler` by name (`<ds-name>-<role-name>`):

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: prefill-hpa
spec:
  scaleTargetRef:
    apiVersion: disaggregatedset.x-k8s.io/v1
    kind: DisaggregatedSetRoleScaler
    name: llm-serving-prefill
  minReplicas: 2
  maxReplicas: 10
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 80
```

### 3. (Optional) KEDA ScaledObject Example

For event-driven autoscaling based on Prometheus metrics (such as vLLM queue depth or TTFT):

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: prefill-keda-scaler
spec:
  scaleTargetRef:
    apiVersion: disaggregatedset.x-k8s.io/v1
    kind: DisaggregatedSetRoleScaler
    name: llm-serving-prefill
  minReplicaCount: 2
  maxReplicaCount: 20
  triggers:
  - type: prometheus
    metadata:
      serverAddress: http://prometheus-k8s.monitoring.svc:9090
      metricName: vllm_num_requests_waiting
      query: sum(vllm_num_requests_waiting{model="meta-llama/Llama-3-70b"})
      threshold: "10"
```

---

## Key Benefits

- **Independent Scaling Curves:** Scale prefill aggressively based on request surges while keeping decode capacity stable or scaling on memory pressure.
- **Rollout Resilience:** Active autoscalers remain attached and functional during rolling updates without reconfiguration.
- **Declarative & Low-Touch:** Zero custom CR authoring required — simply enable `mode: External` and point your autoscaler at the deterministic scaler name.
