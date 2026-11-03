---
title: "Configure Prometheus"
date: 2025-04-28
weight: 2
description: >
  Prometheus Support
---


This page shows how you configure LWS to use prometheus metrics.

## Before you begin

Make sure you the following conditions are set:

- A Kubernetes cluster is running.
- The kubectl command-line tool has communication with your cluster.
- Prometheus is [installed](https://prometheus-operator.dev/docs/getting-started/installation/)
- Cert Manager can be optionally [installed](https://cert-manager.io/docs/installation/)

LWS supports either Kustomize or installation via a Helm chart.

### Kustomize Installation

1. Enable `prometheus` in `config/default/kustomization.yaml` and uncomment all sections with 'PROMETHEUS'.

#### Kustomize Prometheus with certificates

If you want to enable TLS verification for the metrics endpoint, follow the directions below.

1. Set `internalCertManagement.enable` to `false` in the LWS configuration.
2. Comment out the `internalcert` folder in `config/default/kustomization.yaml`.
3. Enable `cert-manager` in `config/default/kustomization.yaml` and uncomment all sections with 'CERTMANAGER'.
4. To enable secure metrics with TLS protection, uncomment all sections with 'PROMETHEUS-WITH-CERTS'.

### Helm Installation

#### Prometheus installation

LWS can also supports helm deployment for Prometheus.

1. Set `enablePrometheus` in your values.yaml file to true.

#### Helm Prometheus with certificates

If you want to secure the metrics endpoints with external certificates:

1. Set `internalCertManagement.enable` to `false` in the LWS configuration.
2. Set both `enableCertManager` and `enablePrometheus` to true.
3. Provide values for the tlsConfig, see the example below:

An example for your tlsConfig in the helm chart could be as follows:

```yaml
...
metrics:
  prometheusNamespace: monitoring
# tls configs for serviceMonitor
  serviceMonitor:
    tlsConfig:
      serverName: lws-controller-manager-metrics-service.lws-system.svc
      ca:
        secret:
          name: lws-metrics-server-cert
          key: ca.crt
```

The secrets must reference the cert manager generated secrets.

## LWS metrics

Besides the [controller-runtime metrics](https://book.kubebuilder.io/reference/metrics-reference), the LWS controller manager serves these counters on the same endpoint:

| Metric | Labels | Description |
|---|---|---|
| `lws_group_recreations_total` | `namespace`, `name`, `reason` | Groups the pod controller recreated by deleting their leader pod. `reason` is `PodRestart` when the restart policy recreated the group after a container of the group restarted or a pod of the group was deleted, and `UpgradeSplit` when an LWS upgrade split the group across PodGroups. |
| `lws_group_restart_budget_exhausted_total` | `namespace`, `name` | Groups terminated because they used up `maxGroupRestarts`. Such a group stays down until it is recovered explicitly. |
| `lws_group_restart_backoffs_total` | `namespace`, `name` | Group recreations delayed by `restartBackoff`. |

`namespace` and `name` identify the LeaderWorkerSet. Its series are removed when the LeaderWorkerSet is deleted.

With the ServiceMonitor that the Kustomize and Helm installations above create, Prometheus sets `namespace` to the namespace of the scraped LWS manager and keeps the LeaderWorkerSet's namespace as `exported_namespace`, unless the ServiceMonitor sets `honorLabels: true`.
