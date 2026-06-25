---
title: "Installing DisaggregatedSet"
linkTitle: "DisaggregatedSet"
weight: 50
description: >
  How to verify that DisaggregatedSet is available after installing LWS.
---

`DisaggregatedSet` is **bundled with the LWS controller manager** (`lws-controller-manager`) as of v0.9.0.
No separate installation step is required: deploying LWS via `kubectl` or Helm automatically installs
the `DisaggregatedSet` CRD and starts the `DisaggregatedSet` controller alongside the LWS controller
in the `lws-system` namespace.

## Before You Begin

Ensure you have already [installed LWS](../) and that the controller manager is ready:

```shell
kubectl wait deploy/lws-controller-manager -n lws-system \
  --for=condition=available --timeout=5m
```

## Verify DisaggregatedSet Is Ready

### Check the CRD

Confirm the `DisaggregatedSet` CRD is registered in the cluster:

```shell
kubectl get crd disaggregatedsets.disaggregatedset.x-k8s.io
```

Expected output:

```
NAME                                          CREATED AT
disaggregatedsets.disaggregatedset.x-k8s.io  2025-...
```

### Check the Controller

The `DisaggregatedSet` controller runs inside the same `lws-controller-manager` pod as the LWS
controller. Verify it is running:

```shell
kubectl get pods -n lws-system
```

Expected output:

```
NAME                                   READY   STATUS    RESTARTS   AGE
lws-controller-manager-...             2/2     Running   0          2m
```

You can also confirm the controller registered successfully by checking the manager logs:

```shell
kubectl logs -n lws-system deploy/lws-controller-manager \
  -c manager | grep -i disaggregatedset
```

## Install a Specific Version

If you need a specific LWS release, follow the [standard installation instructions](../) and
substitute the desired version. The `DisaggregatedSet` CRD and controller are included in all
release manifests from v0.9.0 onwards.

```shell
VERSION=v0.9.0
kubectl apply --server-side \
  -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/manifests.yaml
```

## Uninstall

`DisaggregatedSet` resources are removed as part of the standard LWS uninstall procedure.
Before uninstalling, delete any `DisaggregatedSet` objects to ensure their owned
`LeaderWorkerSet` resources and headless `Services` are garbage-collected cleanly:

```shell
kubectl delete disaggregatedsets --all --all-namespaces
```

Then proceed with the [LWS uninstall steps](../#uninstall).
