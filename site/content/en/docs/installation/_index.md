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
- [Enable native gang scheduling](#enable-native-gang-scheduling)
  - [Configure Kubernetes](#configure-kubernetes)
  - [Configure LeaderWorkerSet](#configure-leaderworkerset)
  - [Verify the prerequisites](#verify-the-prerequisites)
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

- A Kubernetes cluster with version >= 1.34 is **required** (officially supported and tested on Kubernetes 1.34 through 1.37), or it may behave unexpectedly. Learn how to [install the Kubernetes tools](https://kubernetes.io/docs/tasks/tools/).
    - On Kubernetes 1.34, enable the [MaxUnavailableStatefulSet][max_unavailable] feature gate for rolling updates with max unavailable Pods. It is enabled by default in Kubernetes 1.35 and later; see discussion [here][max_unavailable_enhancement].
- Your cluster has at least 1 node with 1+ CPUs and 1G of memory available for the LeaderWorkerSet controller manager Deployment to run on. **NOTE: On some cloud providers, the default node machine type will not have sufficient resources to run the LeaderWorkerSet controller manager and all the required kube-system pods, so you'll need to use a larger
machine type for your nodes.**
- The kubectl command-line tool has communication with your cluster.

## Install a released version

### Install by kubectl

To install a released version of LeaderWorkerSet in your cluster, run the following command:


```shell
VERSION=v0.11.1
kubectl apply --server-side -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/manifests.yaml
```

To wait for LeaderWorkerSet to be fully available, run:

```shell
kubectl wait deploy/lws-controller-manager -n lws-system --for=condition=available --timeout=5m
```

### Install by Helm

To install a released version of lws in your cluster by [Helm](https://helm.sh/), run the following command:

```shell
CHART_VERSION=0.11.1
helm install lws oci://registry.k8s.io/lws/charts/lws \
  --version=$CHART_VERSION \
  --namespace lws-system \
  --create-namespace \
  --wait --timeout 300s
```

You can also use the following command:

```shell
VERSION=v0.11.1
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
CHART_VERSION=0.11.1
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
VERSION=v0.11.1
kubectl delete -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/manifests.yaml
```

To uninstall a released version of LeaderWorkerSet from your cluster by Helm, run the following command:

```shell
helm uninstall lws --namespace lws-system
```

## Enable native gang scheduling

Native gang scheduling is available in LWS v0.11.0 and later. It is supported
on Kubernetes 1.37, where LWS creates the
`scheduling.k8s.io/v1beta1` `Workload` and `PodGroup` resources introduced in
that Kubernetes release. Earlier supported Kubernetes minors do not provide
these API versions.

The LWS integration is alpha, and its required switches are disabled by
default in both Kubernetes and LWS. Configure the cluster and the LWS
controller before creating a LeaderWorkerSet with `spec.scheduling`.

### Configure Kubernetes

Enable the `GenericWorkload` feature gate on all three control-plane
components:

- `kube-apiserver`
- `kube-controller-manager`
- `kube-scheduler`

Also enable the scheduling API on kube-apiserver with
`--runtime-config=scheduling.k8s.io/v1beta1=true`. How these settings are
configured depends on your Kubernetes provider. For example, a Kind cluster
configuration contains:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  GenericWorkload: true
runtimeConfig:
  scheduling.k8s.io/v1beta1: true
```

### Configure LeaderWorkerSet

Enable the LWS `WorkloadAwareScheduling` feature gate and select the native
Kubernetes scheduler provider. With Helm, add these values to the install or
upgrade command shown above:

```shell
--set featureGates.WorkloadAwareScheduling=true \
--set gangSchedulingManagement.schedulerProvider=kubernetes
```

For a configuration-file installation, edit the `lws-manager-config`
ConfigMap in the controller namespace and add the equivalent settings to the
controller manager configuration:

```yaml
featureGates:
  WorkloadAwareScheduling: true
gangSchedulingManagement:
  schedulerProvider: kubernetes
```

Restart the `lws-controller-manager` Deployment after changing an existing
configuration ConfigMap:

```shell
kubectl rollout restart deployment/lws-controller-manager -n lws-system
kubectl rollout status deployment/lws-controller-manager -n lws-system --timeout=5m
```

### Verify the prerequisites

Confirm that Kubernetes serves both required APIs:

```shell
kubectl api-resources --api-group=scheduling.k8s.io | grep -E 'workloads|podgroups'
```

The output must include `workloads` and `podgroups` at version `v1beta1`.
Then follow the [native gang scheduling quickstart](../examples/leaderworkerset/gang-scheduling/).

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
CHART_VERSION=0.11.1
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

#### Migrate pre-slices DisaggregatedSets before v1.0.0

DisaggregatedSet first shipped in v0.9.0, before the `slices` feature. If a
DisaggregatedSet was originally created by v0.9.x and has not completed a rollout while
running v0.10.x or v0.11.x, do not upgrade directly to v1.0.0. First install either
v0.10.x or v0.11.x, then trigger and complete one template rollout for every affected
DisaggregatedSet. For example, changing a container image in a role's pod template
triggers a rollout.
This replaces the generated LeaderWorkerSets and pods with objects that carry
slice-aware names and labels. If the DisaggregatedSet has already completed such a
rollout, no additional migration is required.

Before upgrading to v1.0.0, verify that no pre-slices LeaderWorkerSets remain. The
following command must produce no output:

```shell
kubectl get leaderworkersets -A \
  -l 'disaggregatedset.x-k8s.io/name,!disaggregatedset.x-k8s.io/slice'
```

`helm upgrade` does not install newly added CRDs. DisaggregatedSet ships two:
`disaggregatedsets` since v0.9.0 and `disaggregatedsetrolescalers` since v0.10.0. Apply only
`disaggregatedsets` and the controller cannot create the `DisaggregatedSetRoleScaler` a role with
`scaling.mode: External` depends on, so External scaling never takes effect.

Use the [Upgrade by Helm](#upgrade-by-helm) steps, which apply every CRD in the chart, with the
DisaggregatedSet flag added:

```shell
CHART_VERSION=0.11.1
helm pull oci://registry.k8s.io/lws/charts/lws --version=$CHART_VERSION --untar
kubectl apply --server-side --force-conflicts -f lws/crds
helm upgrade lws oci://registry.k8s.io/lws/charts/lws \
  --version=$CHART_VERSION \
  --namespace lws-system \
  --set enableDisaggregatedSet=true \
  --wait --timeout 300s
```

[feature_gate]: https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/
[start_ordinal]: https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#start-ordinal
[max_unavailable]: https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#maximum-unavailable-pods
[max_unavailable_enhancement]: https://github.com/kubernetes/enhancements/issues/961
[helm_charts]: https://github.com/kubernetes-sigs/lws/releases
