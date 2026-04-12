---
title: "Installing DisaggregatedSet"
linkTitle: "DisaggregatedSet"
weight: 20
description: >
  How to install and enable DisaggregatedSet.
---

<!-- toc -->
- [Overview](#overview)
- [Installation Options](#installation-options)
  - [By kubectl (Kustomize)](#by-kubectl-kustomize)
  - [By Helm](#by-helm)
- [Verify Installation](#verify-installation)
- [Upgrade Notes](#upgrade-notes)
- [Uninstall](#uninstall)
<!-- /toc -->

## Overview

Starting from version 0.8.0, **DisaggregatedSet** is bundled directly into the core LeaderWorkerSet (LWS) controller manager. You do not need to deploy a separate controller manager deployment or manage a separate namespace.

## Installation Options

### By kubectl (Kustomize)

When you deploy LeaderWorkerSet using the standard Kustomize defaults or released manifests, the DisaggregatedSet CustomResourceDefinition (CRD) and manager permissions are automatically installed.

To install the latest release version of LWS (which includes DisaggregatedSet):

```shell
VERSION=v0.8.0
kubectl apply --server-side -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/manifests.yaml
```

To install the latest development version from the `main` branch:

```shell
kubectl apply --server-side -k github.com/kubernetes-sigs/lws/config/default?ref=main
```

### By Helm

The `charts/lws` Helm chart always installs the DisaggregatedSet CRD and controller permissions. 

To also deploy the user-facing editor, viewer, and admin `ClusterRoles` as well as the validating admission webhook for DisaggregatedSet, set the `enableDisaggregatedSet` parameter to `true`:

```shell
CHART_VERSION=0.8.0
helm install lws oci://registry.k8s.io/lws/charts/lws \
  --version=$CHART_VERSION \
  --namespace lws-system \
  --create-namespace \
  --set enableDisaggregatedSet=true \
  --wait --timeout 300s
```

## Verify Installation

1. Wait for the `lws-controller-manager` deployment to become available:

```shell
kubectl wait deploy/lws-controller-manager -n lws-system \
  --for=condition=available --timeout=5m
```

2. Confirm the DisaggregatedSet CRD is registered:

```shell
kubectl get crd disaggregatedsets.disaggregatedset.x-k8s.io
```

You should see output similar to:

```
NAME                                          CREATED AT
disaggregatedsets.disaggregatedset.x-k8s.io   2026-06-09T00:00:00Z
```

3. (If using Helm with webhooks enabled) Confirm the validating webhook configuration matches:

```shell
kubectl get validatingwebhookconfiguration validating-webhook-configuration -o yaml | grep disaggregatedsets
```

## Upgrade Notes

Helm installs CRDs during `helm install`, but does not automatically install newly added CRDs during `helm upgrade`. If you are upgrading an existing LWS Helm release from a version older than v0.8.0 to a version that includes DisaggregatedSet, you must manually apply the CRD first:

```shell
kubectl apply --server-side \
  -f https://raw.githubusercontent.com/kubernetes-sigs/lws/main/charts/lws/crds/disaggregatedset.x-k8s.io_disaggregatedsets.yaml

helm upgrade lws oci://registry.k8s.io/lws/charts/lws \
  --namespace lws-system \
  --set enableDisaggregatedSet=true
```

## Uninstall

To uninstall both LWS and DisaggregatedSet:

- **By kubectl**:
  ```shell
  VERSION=v0.8.0
  kubectl delete -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/manifests.yaml
  ```
- **By Helm**:
  ```shell
  helm uninstall lws --namespace lws-system
  ```
