# DEPRECATED

The standalone `disaggregatedset` Helm chart is deprecated. The
DisaggregatedSet CRD, RBAC, and validating webhook are now shipped as part of
the unified `lws` Helm chart at
[`charts/lws`](../../../charts/lws).

Enable DisaggregatedSet on a fresh `lws` install:

```bash
helm install lws charts/lws \
  --namespace lws-system --create-namespace \
  --set enableDisaggregatedSet=true
```

This directory will be removed in a follow-up PR
(see [kubernetes-sigs/lws#789](https://github.com/kubernetes-sigs/lws/issues/789)).
