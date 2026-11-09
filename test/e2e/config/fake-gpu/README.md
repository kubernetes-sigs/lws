# Fake GPU E2E

Run on CPU-only Kind nodes with Docker, the standard LWS e2e tools, and Helm:

```sh
make test-e2e-fake-gpu \
  KIND_CLUSTER_NAME=lws-fake-gpu-local \
  KUBECONFIG=/tmp/lws-fake-gpu-local.kubeconfig \
  ARTIFACTS=/tmp/lws-fake-gpu-local-artifacts
```

Use unique paths and cluster names for concurrent runs. Nodes must be able
to pull the backend and BusyBox images. Reports are saved to `ARTIFACTS`.

For `USE_EXISTING_CLUSTER=true`, use a dedicated kubeconfig for a disposable
cluster matching `kind.yaml`, with fake-gpu-operator `0.0.72` preinstalled
using `values.yaml`. Cleanup deletes only clusters created by the runner.

The nine `FakeGPU` tests cover workload deletion, scale-down, and rolling updates
for LWS Ordinal, LWS Hash, and DisaggregatedSet, including domain retention while
Pods drain and cleanup after they exit, as described in
[KEP-1259](../../../../keps/1259-auto-compute-domain/README.md).
They do not validate real CUDA, NVLink, or NVIDIA driver behavior.
Container selection and role opt-out are covered by integration tests.
