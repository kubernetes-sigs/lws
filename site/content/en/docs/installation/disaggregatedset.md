---
title: "Installing the DisaggregatedSet Controller"
linkTitle: "DisaggregatedSet Controller"
weight: 20
description: >
  How to install the DisaggregatedSet controller after LWS is already running.
---

<!-- toc -->
- [Prerequisites](#prerequisites)
- [Install a Released Version](#install-a-released-version)
  - [By kubectl](#by-kubectl)
  - [By Helm](#by-helm)
  - [Verify Installation](#verify-installation)
  - [Uninstall](#uninstall)
- [Install the Latest Development Version](#install-the-latest-development-version)
- [Namespace and RBAC Notes](#namespace-and-rbac-notes)
<!-- /toc -->

## Prerequisites

DisaggregatedSet is built on top of [LeaderWorkerSet (LWS)](https://github.com/kubernetes-sigs/lws).
**LWS must be installed and running before you install the DisaggregatedSet controller.**

> **Important:** Install LWS first, then DisaggregatedSet. Installing them in reverse order
> will cause the DisaggregatedSet controller to fail on startup because its webhooks depend on
> LWS CRDs being present.

Verify LWS is running before proceeding:

```shell
kubectl wait deploy/lws-controller-manager -n lws-system \
  --for=condition=available --timeout=5m
```

You should also have:
- A Kubernetes cluster version >= 1.26
- `kubectl` configured to talk to your cluster
- (Optional) [Helm](https://helm.sh/) >= 3.x for Helm-based installation

## Install a Released Version

### By kubectl

```shell
VERSION=v0.8.0
kubectl apply --server-side \
  -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/disaggregatedset-manifests.yaml
```

### By Helm

```shell
CHART_VERSION=0.8.0
helm install disaggregatedset oci://registry.k8s.io/lws/charts/disaggregatedset \
  --version=$CHART_VERSION \
  --namespace disaggregatedset-system \
  --create-namespace \
  --wait --timeout 300s
```

### Verify Installation

Wait for the controller to become available:

```shell
kubectl wait deploy/disaggregatedset-controller-manager \
  -n disaggregatedset-system \
  --for=condition=available --timeout=5m
```

Confirm the CRD is registered:

```shell
kubectl get crd disaggregatedsets.disaggregatedset.x-k8s.io
```

You should see output similar to:

```
NAME                                             CREATED AT
disaggregatedsets.disaggregatedset.x-k8s.io     2025-01-01T00:00:00Z
```

List both controllers to confirm both are running side by side:

```shell
kubectl get pods -n lws-system
kubectl get pods -n disaggregatedset-system
```

### Uninstall

To remove the DisaggregatedSet controller:

```shell
# kubectl
VERSION=v0.8.0
kubectl delete \
  -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/disaggregatedset-manifests.yaml

# Helm
helm uninstall disaggregatedset --namespace disaggregatedset-system
```

> **Note:** Uninstalling the controller does **not** automatically delete existing `DisaggregatedSet`
> objects. You should delete those first to ensure child LWS resources are cleaned up properly.

## Install the Latest Development Version

To install the latest development version directly from the `main` branch:

```shell
kubectl apply --server-side \
  -k github.com/kubernetes-sigs/lws/disaggregatedset/config/default?ref=main
```

The controller runs in the `disaggregatedset-system` namespace.

To uninstall:

```shell
kubectl delete -k github.com/kubernetes-sigs/lws/disaggregatedset/config/default
```

## Namespace and RBAC Notes

| Controller | Namespace | Watches |
|---|---|---|
| LWS | `lws-system` | `LeaderWorkerSet` in all namespaces |
| DisaggregatedSet | `disaggregatedset-system` | `DisaggregatedSet` in all namespaces |

Both controllers are cluster-scoped — they can manage workloads in any namespace. The
`DisaggregatedSet` controller creates `LeaderWorkerSet` resources in the **same namespace** as the
`DisaggregatedSet` object, so LWS must have permission to reconcile those.

No additional RBAC configuration is needed if you install LWS and DisaggregatedSet using the
provided manifests or Helm charts.
