---
title: "Configure the controller manager"
date: 2026-09-28
weight: 1
description: >
  Configure the LeaderWorkerSet controller manager with the stable configuration API.
---

The LeaderWorkerSet (LWS) controller manager reads its component configuration
from the file passed to the `--config` flag. New configuration files should use
the stable `config.lws.x-k8s.io/v1` API.

## Configuration example

The following configuration enables leader election and internal certificate
management. Fields that are not specified use their default values.

```yaml
apiVersion: config.lws.x-k8s.io/v1
kind: Configuration
leaderElection:
  leaderElect: true
internalCertManagement:
  enable: true
```

When installing from source or with Kustomize, edit
[`config/manager/controller_manager_config.yaml`](https://github.com/kubernetes-sigs/lws/blob/main/config/manager/controller_manager_config.yaml)
before building or applying the manifests. The generated controller Deployment
mounts this configuration and passes its path with `--config`.

The Helm chart also renders a `v1` controller configuration. Use chart values to
configure the settings exposed by the chart, including certificate management,
feature gates, and gang scheduling. See the
[`values.yaml`](https://github.com/kubernetes-sigs/lws/blob/main/charts/lws/values.yaml)
file and the [Helm chart configuration](https://github.com/kubernetes-sigs/lws/tree/main/charts/lws#configuration)
for the available values.

## DisaggregatedSet External role names

A scaler sets the number of groups in an External DisaggregatedSet role, so
admission cannot know how long the group index in its generated names will get.
`disaggregatedSetExternalGroupIndexDigits` sets how many characters new External
roles reserve for it, from 1 to 10, and defaults to 8. With the default
`groupIdentity`, one slice, and a DisaggregatedSet name and role name counted
together:

| Digits | Longest combined name | Groups before new Pods fail |
| ------ | --------------------- | --------------------------- |
| 1      | 38 characters         | 10                          |
| 6      | 33 characters         | 1 million                   |
| 8      | 31 characters         | 100 million                 |
| 10     | 29 characters         | No limit                    |

These group counts apply to names at the longest combined length; shorter names
can reach more groups. Past that count, Pods of new groups cannot be created
because their `controller-revision-hash` label would exceed 63 characters.

```yaml
apiVersion: config.lws.x-k8s.io/v1
kind: Configuration
disaggregatedSetExternalGroupIndexDigits: 6
```

## Migrate from `v1alpha1`

Existing `v1alpha1` configuration files continue to work and are converted to
`v1` when the controller starts. Loading one emits a deprecation warning because
`v1alpha1` will be removed in a future release.

To migrate, change the `apiVersion`; the field names remain unchanged:

```diff
-apiVersion: config.lws.x-k8s.io/v1alpha1
+apiVersion: config.lws.x-k8s.io/v1
 kind: Configuration
```

Redeploy the controller after updating the configuration. For a Kustomize
installation, rebuild and apply the manifests. For a Helm installation, upgrade
to a chart that renders the `v1` configuration.
