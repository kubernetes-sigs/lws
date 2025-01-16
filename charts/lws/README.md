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

```bash
$ git clone git@github.com:kubernetes-sigs/lws.git
$ cd charts
$ helm install lws lws --create-namespace --namespace lws-system
```

##### Verify that controller pods are running properly.

```bash
$ kubectl get deploy -n lws-system
NAME                          READY   UP-TO-DATE   AVAILABLE   AGE
lws-system-controller-manager   1/1     1            1           14s
```

### Configuration

The following table lists the configurable parameters of the LWS chart and their default values.

| Parameter                                   | Description                                    | Default                              |
|---------------------------------------------|------------------------------------------------|--------------------------------------|
| `nameOverride`                              | nameOverride                                   | ``                                   |
| `fullnameOverride`                          | fullnameOverride                               | ``                                   |
| `enablePrometheus`                          | enable Prometheus                              | `false`                              |
| `enableCertManager`                         | enable CertManager                             | `false`                              |
| `imagePullSecrets`                          | Image pull secrets                             | `[]`                                 |
| `image.manager.repository`                  | Repository for manager image                   | `gcr.io/k8s-staging-lws/lws`         |
| `image.manager.tag`                         | Tag for manager image                          | `main`                               |
| `image.manager.pullPolicy`                  | Pull policy for manager image                  | `IfNotPresent`                       |
| `podAnnotations`                            | Annotations for pods                           | `{}`                                 |
| `podSecurityContext.runAsNonRoot`           | Run pod as non-root user                       | `true`                               |
| `securityContext.allowPrivilegeEscalation`  | Allow privilege escalation in security context | `false`                              |
| `securityContext.capabilities.drop`         | Drop all capabilities in security context      | `["ALL"]`                            |
| `service.type`                              | Type of lws controller service                 | `ClusterIP`                          |
| `service.port`                              | Lws controller service port                    | `9443`                               |
| `resources.requests.cpu`                    | CPU request for resources                      | `1`                                  |
| `resources.requests.memory`                 | Memory request for resources                   | `1Gi`                                |
| `nodeSelector`                              | Node selector                                  | `{}`                                 |
| `tolerations`                               | Tolerations                                    | `{}`                                 |
| `affinity`                                  | Affinity                                       | `{}`                                 |