---
title: "DisaggregatedSet API (v1)"
linkTitle: "disaggregatedset.v1"
weight: 20
description: >
  API reference for the DisaggregatedSet custom resource (disaggregatedset.x-k8s.io/v1).
---

> **API Status:** `v1`

<!-- toc -->
- [DisaggregatedSet](#disaggregatedset)
- [DisaggregatedSetSpec](#disaggregatedsetspec)
- [DisaggregatedRoleSpec](#disaggregatedrolespec)
- [DisaggregatedSetStatus](#disaggregatedsetstatus)
- [RoleStatus](#rolestatus)
- [CRD Source](#crd-source)
<!-- /toc -->

## DisaggregatedSet

`DisaggregatedSet` is the top-level resource. It defines a multi-role inference deployment where each role is backed by an independent `LeaderWorkerSet`.

| Field | Type | Required | Description |
|---|---|---|---|
| `apiVersion` | string | Yes | `disaggregatedset.x-k8s.io/v1` |
| `kind` | string | Yes | `DisaggregatedSet` |
| `metadata` | [ObjectMeta](https://kubernetes.io/docs/reference/kubernetes-api/common-definitions/object-meta/) | Yes | Standard Kubernetes object metadata |
| `spec` | [DisaggregatedSetSpec](#disaggregatedsetspec) | Yes | Desired state of the DisaggregatedSet |
| `status` | [DisaggregatedSetStatus](#disaggregatedsetstatus) | No | Observed state (populated by the controller) |

## DisaggregatedSetSpec

| Field | Type | Required | Description |
|---|---|---|---|
| `roles` | `[]DisaggregatedRoleSpec` | Yes | List of roles (e.g., prefill, decode, encode). Each role creates one `LeaderWorkerSet`. Must contain at least two roles and at most ten roles. Role names must be unique within a `DisaggregatedSet`. |

## DisaggregatedRoleSpec

Defines a single inference role and the LWS template that backs it.

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | Yes | Unique name for this role. Used to construct the child LWS name: `<DisaggregatedSet-name>-<role-name>`. |
| `metadata` | [ObjectMeta](https://kubernetes.io/docs/reference/kubernetes-api/common-definitions/object-meta/) | No | Optional metadata (labels/annotations) to apply to the child `LeaderWorkerSet` created for this role. |
| `spec` | [LeaderWorkerSetSpec](/docs/reference/leaderworkerset.v1/#leaderworkerset-x-k8s-io-v1-LeaderWorkerSetSpec) | Yes | Defines the behavior of the child `LeaderWorkerSet`. Note: `rolloutStrategy.type` must be `RollingUpdate` (or empty) and `rolloutStrategy.rollingUpdateConfiguration.partition` must not be set. |

## DisaggregatedSetStatus

| Field | Type | Description |
|---|---|---|
| `conditions` | `[]metav1.Condition` | Standard Kubernetes conditions reflecting the overall health of the DisaggregatedSet (e.g., Available, Progressing, Degraded). |
| `roleStatuses` | `[]RoleStatus` | Per-role status, including the name of the role and its current ready, updated, and total replica counts. |

## RoleStatus

| Field | Type | Description |
|---|---|---|
| `name` | string | Name of the role (matches `spec.roles[].name`). |
| `replicas` | int32 | Total number of replicas (pod groups) for this role. |
| `readyReplicas` | int32 | Number of ready replicas for this role. |
| `updatedReplicas` | int32 | Number of replicas updated to the latest revision. |

## CRD Source

The full CRD YAML and Go type definitions are available in the repository:

- **CRD YAML:** [`config/crd/bases/`](https://github.com/kubernetes-sigs/lws/tree/main/config/crd/bases)
- **Go types:** [`api/disaggregatedset/v1/`](https://github.com/kubernetes-sigs/lws/tree/main/api/disaggregatedset/v1)
- **Design (KEP-766):** [`keps/766-DisaggregatedSet/`](https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet)

For labels and annotations used by the DisaggregatedSet controller, see the [Labels, Annotations, and Environment Variables](/docs/reference/labels-annotations-and-environment-variables/) reference page.
