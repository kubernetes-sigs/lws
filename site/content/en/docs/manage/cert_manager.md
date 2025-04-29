---
title: "Configure external cert-manager"
date: 2025-04-28
weight: 1
description: >
  Cert Manager Support
---

This page shows how you can a third party certificate authority solution like
Cert Manager.

## Before you begin

Make sure you the following conditions are set:

- A Kubernetes cluster is running.
- The kubectl command-line tool has communication with your cluster.
- Cert Manager is [installed](https://cert-manager.io/docs/installation/)

LWS supports either Kustomize or installation via a Helm chart.

### Internal Certificate management

In all cases, LWS's internal certificate management must be turned off
if one wants to use CertManager.

### Kustomize Installation

1. Set `internalCertManagement.enable` to `false` in the LWS configuration.
2. Comment out the `../internalcert` folder in `config/default/kustomization.yaml`.
3. Uncomment `../certmanager` folder in `config/default/kustomization.yaml`.
4. Enable `cert-manager` in `config/default/kustomization.yaml` and uncomment all sections with 'CERTMANAGER'.
5. Apply these configurations to your cluster with ``kubectl apply --server-side -k config/default``.

### Helm Installation

LWS can also support optional helm values for Cert Manager enablement.

1. Disable `internalCertManager` in the LWS configuration.
2. set `enableCertManager` in your values.yaml file to true.