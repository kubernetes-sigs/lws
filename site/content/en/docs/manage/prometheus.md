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
      cert:
        secret:
          name: lws-metrics-server-cert
          key: tls.crt
      keySecret:
        name: lws-metrics-server-cert
        key: tls.key
```

The secrets must reference the cert manager generated secrets.