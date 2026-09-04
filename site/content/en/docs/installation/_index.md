---
title: "Installation"
linkTitle: "Installation"
weight: 2
description: >
  Installing LWS to a Kubernetes Cluster
---

<!-- toc -->
- [Before you begin](#before-you-begin)
- [Install a released version](#install-a-released-version)
  - [Uninstall](#uninstall)
- [Install the latest development version](#install-the-latest-development-version)
  - [Uninstall](#uninstall-1)
- [Build and install from source](#build-and-install-from-source)
  - [Uninstall](#uninstall-2)
- [Install in a different namespace](#install-in-a-different-namespace)
- [Optional: Use cert manager instead of internal cert](#optional-use-cert-manager-instead-of-internal-cert)
- [Install with Helm chart](#install-with-helm-chart)
- [DisaggregatedSet](#disaggregatedset)

<!-- /toc -->


## Before you begin

Make sure the following conditions are met:

- A Kubernetes cluster with version >= 1.33 is **Required** (lws supports the latest 3 Kubernetes minor versions, currently 1.33-1.36). Learn how to [install the Kubernetes tools](https://kubernetes.io/docs/tasks/tools/).
    - Rolling update with max unavailable Pods, you must enable the [MaxUnavailableStatefulSet][max_unavailable] feature gate, which is still in alpha since Kubernetes v1.24, see discussion [here][max_unavailable_enhancement]. Or lws will roll out the pods one by one.
- Your cluster has at least 1 node with 1+ CPUs and 1G of memory available for the LeaderWorkerSet controller manager Deployment to run on. **NOTE: On some cloud providers, the default node machine type will not have sufficient resources to run the LeaderWorkerSet controller manager and all the required kube-system pods, so you'll need to use a larger
machine type for your nodes.**
- The kubectl command-line tool has communication with your cluster.

## Install a released version

### Install by kubectl

To install a released version of LeaderWorkerSet in your cluster, run the following command:


```shell
VERSION=v0.10.0
kubectl apply --server-side -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/manifests.yaml
```

To wait for LeaderWorkerSet to be fully available, run:

```shell
kubectl wait deploy/lws-controller-manager -n lws-system --for=condition=available --timeout=5m
```

### Install by Helm

To install a released version of lws in your cluster by [Helm](https://helm.sh/), run the following command:

```shell
CHART_VERSION=0.10.0
helm install lws oci://registry.k8s.io/lws/charts/lws \
  --version=$CHART_VERSION \
  --namespace lws-system \
  --create-namespace \
  --wait --timeout 300s
```

You can also use the following command:

```shell
VERSION=v0.10.0
helm install lws https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/lws-chart-$VERSION.tgz \
  --namespace lws-system \
  --create-namespace \
  --wait --timeout 300s
```

### Upgrade by Helm

Helm only installs the chart's CRDs during the initial `helm install`. It does
not update or delete CRDs on `helm upgrade` (see the
[Helm documentation](https://helm.sh/docs/chart_best_practices/custom_resource_definitions/)),
so CRD schema changes and newly added CRDs do not reach the cluster through
`helm upgrade` alone.

Apply the CRDs explicitly before upgrading, then upgrade the release in place:

```shell
CHART_VERSION=0.10.0
helm pull oci://registry.k8s.io/lws/charts/lws --version=$CHART_VERSION --untar
kubectl apply --server-side --force-conflicts -f lws/crds
helm upgrade lws oci://registry.k8s.io/lws/charts/lws \
  --version=$CHART_VERSION \
  --namespace lws-system \
  --wait --timeout 300s
```

{{% alert title="Note" color="info" %}}
`helm upgrade` does not update CRD schemas — Helm never modifies CRDs placed
in the `crds/` directory after the initial `helm install`, and does not delete
them on `helm uninstall` either. Always reconcile CRD schemas explicitly with
the `kubectl apply` step above before upgrading the chart.
{{% /alert %}}

#### Upgrading from v0.7.0 or earlier

Chart versions up to v0.7.0 rendered the `LeaderWorkerSet` CRD from
`templates/crds/`, so the CRD is part of the Helm release manifest. Starting
with v0.8.0 the CRD ships from the special `crds/` directory and is no longer
part of the release. Without preparation, the first `helm upgrade` across that
boundary treats the CRD as removed from the release and deletes it — cascading
to the deletion of every `LeaderWorkerSet` in the cluster (see
[#880](https://github.com/kubernetes-sigs/lws/issues/880)).

Before the first upgrade from v0.7.0 or earlier, run this one-time step so Helm
keeps the CRD when it leaves the release:

```shell
kubectl annotate crd leaderworkersets.leaderworkerset.x-k8s.io \
  helm.sh/resource-policy=keep --overwrite
```

Then follow the regular upgrade flow above (apply the CRDs, then
`helm upgrade`). Subsequent upgrades no longer need the annotation step.

### Uninstall

To uninstall a released version of LeaderWorkerSet from your cluster, run the following command:

```shell
VERSION=v0.10.0
kubectl delete -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/manifests.yaml
```

To uninstall a released version of LeaderWorkerSet from your cluster by Helm, run the following command:

```shell
helm uninstall lws --namespace lws-system
```

## Install the latest development version

To install the latest development version of LeaderWorkerSet in your cluster, run the
following command:

```shell
kubectl apply --server-side -k github.com/kubernetes-sigs/lws/config/default?ref=main
```

The controller runs in the `lws-system` namespace.

### Uninstall

To uninstall LeaderWorkerSet, run the following command:

```shell
kubectl delete -k github.com/kubernetes-sigs/lws/config/default
```

## Build and install from source

To build LeaderWorkerSet from source and install LeaderWorkerSet in your cluster, run the following
commands:

```sh
git clone https://github.com/kubernetes-sigs/lws.git
cd lws
IMAGE_REGISTRY=<registry>/<project> make image-push deploy
```

### Uninstall

To uninstall LeaderWorkerSet, run the following command:

```sh
make undeploy
```

## Install in a different namespace

To install the leaderWorkerSet controller in a different namespace rather than `lws-system`, you should first:
```sh
git clone https://github.com/kubernetes-sigs/lws.git
cd lws
```
Then change the [kustomization.yaml](https://github.com/kubernetes-sigs/lws/blob/main/config/default/kustomization.yaml) _namespace_ field as:
```yaml
namespace: <your-namespace>
```

## Optional: Use cert manager instead of internal cert
The webhooks use an internal certificate by default. However, if you wish to use cert-manager (which
supports cert rotation), instead of internal cert, follow the [cert manage guide](/docs/manage/cert_manager).

## Install with Helm chart

Please refer to the release page for [helm charts][helm_charts].

## DisaggregatedSet

Starting from v0.9.0, DisaggregatedSet is bundled with the LWS controller manager.

For kubectl and Kustomize installs, the standard v0.9.0+ manifests include the DisaggregatedSet
CRD, controller permissions, and validating webhook. No separate DisaggregatedSet installation
step is required.

For Helm installs, the DisaggregatedSet CRD and controller permissions are installed by default.
The optional validating webhook and user-facing editor/viewer/admin ClusterRoles can be enabled
by passing `--set enableDisaggregatedSet=true` to the Helm install command:

```shell
CHART_VERSION=0.10.0
helm install lws oci://registry.k8s.io/lws/charts/lws \
  --version=$CHART_VERSION \
  --namespace lws-system \
  --create-namespace \
  --set enableDisaggregatedSet=true \
  --wait --timeout 300s
```

### Verify Installation

1. Wait for the controller manager to become available:

```shell
kubectl wait deploy/lws-controller-manager -n lws-system \
  --for=condition=available --timeout=5m
```

2. Confirm the DisaggregatedSet CRD is registered:

```shell
kubectl get crd disaggregatedsets.disaggregatedset.x-k8s.io
```

3. (Helm with webhooks enabled) Confirm the validating webhook configuration:

```shell
kubectl get validatingwebhookconfiguration lws-validating-webhook-configuration \
  -o yaml | grep disaggregatedsets
```

### Upgrade from an older version

Helm does not automatically install newly added CRDs during `helm upgrade`. If you are upgrading
from a version older than v0.9.0, manually apply the CRD first:

```shell
kubectl apply --server-side \
  -f https://raw.githubusercontent.com/kubernetes-sigs/lws/main/charts/lws/crds/disaggregatedset.x-k8s.io_disaggregatedsets.yaml

helm upgrade lws oci://registry.k8s.io/lws/charts/lws \
  --namespace lws-system \
  --set enableDisaggregatedSet=true
```

[feature_gate]: https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/
[start_ordinal]: https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#start-ordinal
[max_unavailable]: https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#maximum-unavailable-pods
[max_unavailable_enhancement]: https://github.com/kubernetes/enhancements/issues/961
[helm_charts]: https://github.com/kubernetes-sigs/lws/releases