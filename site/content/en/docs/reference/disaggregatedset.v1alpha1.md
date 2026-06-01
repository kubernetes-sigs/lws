---
title: "DisaggregatedSet API (v1alpha1)"
linkTitle: "disaggregatedset.v1alpha1"
weight: 20
description: >
  API reference for the DisaggregatedSet custom resource (disaggregatedset.x-k8s.io/v1alpha1).
---

> **API Status:** `v1alpha1` — This API is in alpha and subject to change without notice.
> Pin your controller to a specific release version in production environments.

<!-- toc -->
- [DisaggregatedSet](#disaggregatedset)
- [DisaggregatedSetSpec](#disaggregatedsetspec)
- [RoleSpec](#rolespec)
- [RolloutStrategy](#rolloutstrategydisagg)
- [RollingUpdateConfiguration](#rollingupdateconfigurationdisagg)
- [DisaggregatedSetStatus](#disaggregatedsetstatus)
- [CRD Source](#crd-source)
<!-- /toc -->

## DisaggregatedSet

`DisaggregatedSet` is the top-level resource. It defines a multi-role inference deployment where
each role is backed by an independent `LeaderWorkerSet`.

| Field | Type | Required | Description |
|---|---|---|---|
| `apiVersion` | string | Yes | `disaggregatedset.x-k8s.io/v1alpha1` |
| `kind` | string | Yes | `DisaggregatedSet` |
| `metadata` | [ObjectMeta](https://kubernetes.io/docs/reference/kubernetes-api/common-definitions/object-meta/) | Yes | Standard Kubernetes object metadata |
| `spec` | [DisaggregatedSetSpec](#disaggregatedsetspec) | Yes | Desired state of the DisaggregatedSet |
| `status` | [DisaggregatedSetStatus](#disaggregatedsetstatus) | No | Observed state (populated by the controller) |

## DisaggregatedSetSpec

| Field | Type | Required | Description |
|---|---|---|---|
| `roles` | [][RoleSpec](#rolespec) | Yes | List of roles (e.g., prefill, decode, encode). Each role creates one `LeaderWorkerSet`. Must contain at least one role. Role names must be unique within a `DisaggregatedSet`. |

## RoleSpec

Defines a single inference role and the `LeaderWorkerSet` that backs it.

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | Yes | Unique name for this role. Used to construct the child LWS name: `<DisaggregatedSet-name>-<role-name>`. |
| `replicas` | int32 | No | Number of LWS replicas (pod groups) for this role. Defaults to `1`. |
| `rolloutStrategy` | [RolloutStrategy](#rolloutstrategydisagg) | No | Rolling update policy for this role. Independent of other roles. |
| `leaderWorkerTemplate` | [LeaderWorkerTemplate](https://github.com/kubernetes-sigs/lws/blob/main/api/leaderworkerset/v1/leaderworkerset_types.go) | Yes | Pod template for leader and worker containers, identical to the LWS `leaderWorkerTemplate` field. |

## RolloutStrategy (DisaggregatedSet) {#rolloutstrategydisagg}

Each role has an independent rollout strategy. A rolling update on the `decode` role will not
affect pods in the `prefill` role.

| Field | Type | Required | Description |
|---|---|---|---|
| `rollingUpdateConfiguration` | [RollingUpdateConfiguration](#rollingupdateconfigurationdisagg) | No | Configures maxSurge and maxUnavailable for this role. |

## RollingUpdateConfiguration (DisaggregatedSet) {#rollingupdateconfigurationdisagg}

| Field | Type | Default | Description |
|---|---|---|---|
| `maxUnavailable` | int32 | `1` | Maximum number of LWS replicas that can be unavailable during a rolling update. Cannot be `0` if `maxSurge` is also `0`. |
| `maxSurge` | int32 | `0` | Maximum number of extra LWS replicas that can exist during a rolling update. |

## DisaggregatedSetStatus

| Field | Type | Description |
|---|---|---|
| `conditions` | []metav1.Condition | Standard Kubernetes conditions reflecting the overall health of the DisaggregatedSet. |
| `roleStatuses` | [][RoleStatus] | Per-role status, including the name of the child LWS and its current ready/total replica counts. |

## CRD Source

The full CRD YAML and Go type definitions are available in the repository:

- **CRD YAML:** [`disaggregatedset/config/crd/bases/`](https://github.com/kubernetes-sigs/lws/tree/main/disaggregatedset/config/crd/bases)
- **Go types:** [`disaggregatedset/api/v1alpha1/`](https://github.com/kubernetes-sigs/lws/tree/main/disaggregatedset/api/v1alpha1)
- **Design (KEP-766):** [`keps/766-DisaggregatedSet/`](https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet)

For labels and annotations used by the DisaggregatedSet controller, see the
[Labels, Annotations, and Environment Variables](/docs/reference/labels-annotations-and-environment-variables/)
reference page — note that child LWS resources created by DisaggregatedSet inherit all standard LWS labels.
