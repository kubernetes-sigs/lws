# Installation

## Before you begin

Make sure the following conditions are met:

- A Kubernetes cluster with version 1.26 or newer is running. Learn how to [install the Kubernetes tools](https://kubernetes.io/docs/tasks/tools/). For any clusters with version lower than 1.27, we need to enable the [feature gate][feature_gate] for `Start Ordinal`, see https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#start-ordinal
- Your cluster has at least 1 node with 1+ CPUs and 1G of memory available for the LeaderWorkerSet controller manager Deployment to run on. **NOTE: On some cloud providers, the default node machine type will not have sufficient resources to run the LeaderWorkerSet controller manager and all the required kube-system pods, so you'll need to use a larger
machine type for your nodes.**
- The kubectl command-line tool has communication with your cluster.

[feature_gate]: https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/

# Install a released version

To install a released version of LeaderWorkerSet in your cluster, run the following command:

```shell
VERSION=v0.5.1
kubectl apply --server-side -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/manifests.yaml
```

## Uninstall

To uninstall a released version of LeaderWorkerSet from your cluster, run the following command:

```shell
VERSION=v0.5.1
kubectl delete -f https://github.com/kubernetes-sigs/lws/releases/download/$VERSION/manifests.yaml
```

# Install the latest development version

To install the latest development version of LeaderWorkerSet in your cluster, run the
following command:

```shell
kubectl apply --server-side -k github.com/kubernetes-sigs/lws/config/default?ref=main
```

The controller runs in the `lws-system` namespace.

## Uninstall

To uninstall LeaderWorkerSet, run the following command:

```shell
kubectl delete -k github.com/kubernetes-sigs/lws/config/default
```

# Build and install from source

To build LeaderWorkerSet from source and install LeaderWorkerSet in your cluster, run the following
commands:

```sh
git clone https://github.com/kubernetes-sigs/lws.git
cd lws
IMAGE_REGISTRY=<registry>/<project> make image-push deploy
```

## Uninstall

To uninstall LeaderWorkerSet, run the following command:

```sh
make undeploy
```

# Install in a different namespace

To install the leaderWorkerSet controller in a different namespace rather than `lws-system`, you should first:
```sh
git clone https://github.com/kubernetes-sigs/lws.git
cd lws
```
Then change the [kustomization.yaml](../../config/default/kustomization.yaml) _namespace_ field as:
```yaml
namespace: <your-namespace>
```

# Optional: Use cert manager instead of internal cert
The webhooks use an internal certificate by default. However, if you wish to use cert-manager (which
supports cert rotation), instead of internal cert, you can by performing the following steps.

First, install cert-manager on your cluster by running the following command:

```shell
VERSION=v1.11.0
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/$VERSION/cert-manager.yaml
```

Next, in the file ``lws/config/default/kustomization.yaml`` replace ``../internalcert`` with
``../certmanager`` then uncomment all the lines beginning with ``[CERTMANAGER]``.

Finally, install the cert manager follwing the link: https://cert-manager.io/docs/installation/#default-static-install
and apply these configurations to your cluster with ``kubectl apply --server-side -k config/default``.

# Install with helm chart
See [lws/charts](../../charts/lws/README.md)
