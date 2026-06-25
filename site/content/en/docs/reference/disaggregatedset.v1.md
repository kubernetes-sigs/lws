---
title: "DisaggregatedSet API Reference"
linkTitle: "DisaggregatedSet API"
weight: 20
description: >
  API reference for the DisaggregatedSet v1 resource (disaggregatedset.x-k8s.io/v1).
---

`DisaggregatedSet` is a cluster-scoped Custom Resource that orchestrates multiple
`LeaderWorkerSet` resources as a single logical unit for disaggregated inference workloads.

- **API Group:** `disaggregatedset.x-k8s.io`
- **Version:** `v1`
- **Kind:** `DisaggregatedSet`
- **Scope:** Namespaced
- **CRD source:** [disaggregatedset.x-k8s.io_disaggregatedsets.yaml](https://github.com/kubernetes-sigs/lws/blob/main/config/crd/bases/disaggregatedset.x-k8s.io_disaggregatedsets.yaml)

---

## DisaggregatedSet

| Field | Type | Required | Description |
|---|---|---|---|
| `apiVersion` | `string` | Yes | `disaggregatedset.x-k8s.io/v1` |
| `kind` | `string` | Yes | `DisaggregatedSet` |
| `metadata` | `ObjectMeta` | No | Standard Kubernetes object metadata. |
| `spec` | `DisaggregatedSetSpec` | Yes | Desired state of the `DisaggregatedSet`. |
| `status` | `DisaggregatedSetStatus` | No | Observed state, populated by the controller. |

---

## DisaggregatedSetSpec

| Field | Type | Required | Description |
|---|---|---|---|
| `roles` | `[]DisaggregatedRoleSpec` | Yes | List of roles (minimum 2, maximum 10). Role names must be unique. Either all roles have `replicas > 0` or all have `replicas == 0`; mixing is not allowed. |

---

## DisaggregatedRoleSpec

Each entry in `spec.roles` configures one `LeaderWorkerSet` managed by the `DisaggregatedSet`.

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | `string` | Yes | Unique identifier for the role. Must match `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` (1–63 characters). |
| `metadata` | `ObjectMeta` | No | Labels and annotations propagated to the managed `LeaderWorkerSet` object. Useful for Kueue (`kueue.x-k8s.io/queue-name`) and topology annotations. |
| `spec` | `LeaderWorkerSetSpec` | No | Full LWS spec for this role. The `rolloutStrategy` field is honored for per-role surge settings; `RolloutStrategy.Type` must be `RollingUpdate` (or empty). `RolloutStrategy.RollingUpdateConfiguration.Partition` must **not** be set — `DisaggregatedSet` manages rollouts across all roles. |

> **Note**: `DisaggregatedRoleSpec` embeds `leaderworkerset.LeaderWorkerSetTemplateSpec` inline. See the [LeaderWorkerSet API Reference](./leaderworkerset.v1) for the full field list.

---

## DisaggregatedSetStatus

| Field | Type | Description |
|---|---|---|
| `roleStatuses` | `[]RoleStatus` | Per-role observed state. Order matches `spec.roles`. |
| `conditions` | `[]metav1.Condition` | Standard conditions reflecting overall state. |

### Condition Types

| Type | Meaning |
|---|---|
| `Available` | All roles are fully functional and `readyReplicas == replicas` for each role. |
| `Progressing` | The `DisaggregatedSet` is being created or a rolling update is in progress. |
| `Degraded` | The `DisaggregatedSet` failed to reach or maintain its desired state. |

---

## RoleStatus

| Field | Type | Description |
|---|---|---|
| `name` | `string` | Role name (matches `spec.roles[].name`). |
| `replicas` | `int32` | Total number of LWS replicas for this role. |
| `readyReplicas` | `int32` | Number of replicas that are `Ready`. |
| `updatedReplicas` | `int32` | Number of replicas updated to the latest revision. |

---

## Labels and Annotations

### Labels (applied to managed LeaderWorkerSets and their pods)

| Key | Description |
|---|---|
| `disaggregatedset.x-k8s.io/name` | Name of the parent `DisaggregatedSet`. |
| `disaggregatedset.x-k8s.io/role` | Name of the role (e.g. `prefill`, `decode`). |
| `disaggregatedset.x-k8s.io/revision` | Truncated hash of all role templates for this revision, enabling revision-aware traffic routing. |

### Annotations (applied to managed LeaderWorkerSets)

| Key | Description |
|---|---|
| `disaggregatedset.x-k8s.io/initial-replicas` | Starting replica count recorded at the beginning of a rolling update. Used internally by the rolling update planner; do not edit manually. |

---

## Headless Services

For each role and revision, the controller automatically creates a headless `Service` named:

```
{disaggregatedset-name}-{revision-hash}-{role-name}-prv
```

These services enable revision-aware load balancers (such as [llm-d](https://github.com/llm-d/llm-d))
to count pods per revision across all role pools and distribute traffic proportionally during rolling updates.
Services are owned by the `DisaggregatedSet` and garbage-collected when the parent resource is deleted.
