# lws's helm chart

## Table of Contents

<!-- toc -->
- [Installation](#installation)
    - [Prerequisites](#prerequisites)
    - [Installing the chart](#installing-the-chart)
        - [Install chart using Helm v3.0+](#install-chart-using-helm-v30)
        - [Verify that controller pods are running properly.](#verify-that-controller-pods-are-running-properly)
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

### Configuration

The following table lists the configurable parameters of the LWS chart and their default values.

| Parameter                                  | Description                                    | Default                                             |
|--------------------------------------------|------------------------------------------------|-----------------------------------------------------|
| `nameOverride`                             | nameOverride                                   | ``                                                  |
| `fullnameOverride`                         | fullnameOverride                               | ``                                                  |
| `enablePrometheus`                         | enable Prometheus                              | `false`                                             |
| `enableCertManager`                        | enable CertManager                             | `false`                                             |
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