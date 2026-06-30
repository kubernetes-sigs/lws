# lws's helm chart

## Table of Contents

<!-- toc -->
- [Installation](#installation)
    - [Prerequisites](#prerequisites)
    - [Installing the chart](#installing-the-chart)
        - [Install chart using Helm v3.0+](#install-chart-using-helm-v30)
        - [Verify that controller pods are running properly.](#verify-that-controller-pods-are-running-properly)
    - [Upgrading the chart](#upgrading-the-chart)
    - [Configuration](#configuration)
<!-- /toc -->

### Installation

Quick start instructions for the setup and configuration of lws using Helm.

#### Prerequisites

- [Helm](https://helm.sh/docs/intro/quickstart/#install-helm)
- (Optional) [Cert-manager](https://cert-manager.io/docs/installation/)

#### Installing the chart

##### Install chart using Helm v3.0+

You can install the chart using one of the following methods:

**From source**:

```bash
git clone git@github.com:kubernetes-sigs/lws.git
cd charts
helm install lws lws --create-namespace --namespace lws-system
```

**From the OCI registry**:

Alternatively, you can use the charts available at `oci://registry.k8s.io/lws/charts/lws`. For more details, refer to the [Helm chart installation documentation](https://lws.sigs.k8s.io/docs/installation/#install-by-helm).

##### Verify that controller pods are running properly.

```bash
kubectl get deploy -n lws-system
NAME                          READY   UP-TO-DATE   AVAILABLE   AGE
lws-system-controller-manager   1/1     1            1           14s
```

##### Cert Manager

LWS has support for third-party certificates.
One can enable this by setting `enableCertManager` to true.
This will use certManager to generate a secret, inject the CABundles and set up the tls.

Check out the [site](https://lws.sigs.k8s.io/docs/manage/cert_manager/)
for more information on installing cert manager with our Helm chart.

##### Prometheus

LWS supports prometheus metrics.
Check out the [site](https://lws.sigs.k8s.io/docs/manage/prometheus/)
for more information on installing LWS with metrics using our Helm chart.

##### DisaggregatedSet

The DisaggregatedSet CRD and the manager permissions required by its bundled
controller are always installed with the chart. To also install the
editor/viewer/admin ClusterRoles and validating webhook, set
`enableDisaggregatedSet` to `true`.

### Upgrading the chart

The chart ships its CRDs under the special `crds/` directory. Helm installs the
files in that directory only during the initial `helm install`; it never updates
or deletes them on `helm upgrade` (see the
[Helm documentation](https://helm.sh/docs/chart_best_practices/custom_resource_definitions/)).

This has two consequences when upgrading an existing release:

- CRD schema changes (new fields, printer columns, conversion settings, etc.) do
  **not** reach the cluster through `helm upgrade`.
- A newly added CRD (for example `DisaggregatedSet`) is **not** installed by
  `helm upgrade`.

Apply the CRDs explicitly before upgrading so the cluster always has the schema
the new chart version expects. `--server-side` lets the apply coexist with the
ownership Helm records for the resource:

```bash
kubectl apply --server-side --force-conflicts \
  -f charts/lws/crds/leaderworkerset.x-k8s.io_leaderworkersets.yaml \
  -f charts/lws/crds/disaggregatedset.x-k8s.io_disaggregatedsets.yaml
helm upgrade lws charts/lws --namespace lws-system
```

> **Note**: neither `helm upgrade` nor `helm uninstall` + `helm install` will
> update CRD schemas — Helm never modifies CRDs placed in the `crds/` directory
> after the initial install, and does not delete them on `helm uninstall` either.
> Always reconcile CRD schemas explicitly with the `kubectl apply` step above
> before upgrading.

### Configuration

The following table lists the configurable parameters of the LWS chart and their default values.

| Parameter                                  | Description                                    | Default                                             |
|--------------------------------------------|------------------------------------------------|-----------------------------------------------------|
| `nameOverride`                             | nameOverride                                   | ``                                                  |
| `fullnameOverride`                         | fullnameOverride                               | ``                                                  |
| `enablePrometheus`                         | enable Prometheus                              | `false`                                             |
| `enableCertManager`                        | enable CertManager                             | `false`                                             |
| `enableDisaggregatedSet`                   | install DisaggregatedSet editor/viewer/admin ClusterRoles and validating webhook (the CRD, bundled controller, and its required RBAC rules are always installed) | `false` |
| `imagePullSecrets`                         | Image pull secrets                             | `[]`                                                |
| `image.manager.repository`                 | Repository for manager image                   | `us-central1-docker.pkg.dev/k8s-staging-images/lws` |
| `image.manager.tag`                        | Tag for manager image                          | `main`                                              |
| `image.manager.pullPolicy`                 | Pull policy for manager image                  | `IfNotPresent`                                      |
| `podAnnotations`                           | Annotations for pods                           | `{}`                                                |
| `podSecurityContext.runAsNonRoot`          | Run pod as non-root user                       | `true`                                              |
| `securityContext.allowPrivilegeEscalation` | Allow privilege escalation in security context | `false`                                             |
| `securityContext.capabilities.drop`        | Drop all capabilities in security context      | `["ALL"]`                                           |
| `service.type`                             | Type of lws controller service                 | `ClusterIP`                                         |
| `service.port`                             | Lws controller service port                    | `9443`                                              |
| `resources.requests.cpu`                   | CPU request for resources                      | `1`                                                 |
| `resources.requests.memory`                | Memory request for resources                   | `1Gi`                                               |
| `nodeSelector`                             | Node selector                                  | `{}`                                                |
| `tolerations`                              | Tolerations                                    | `{}`                                                |
| `affinity`                                 | Affinity                                       | `{}`                                                |
| `gangSchedulingManagement`                 | Configuration for gang scheduling.             | `{}`                                                |
