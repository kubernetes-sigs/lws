# Recovery end-to-end test

Run the controller-restart recovery scenario on a dedicated Kind test cluster:

```shell
make test-e2e E2E_FOCUS='LeaderWorkerSet recovery'
```

This uses the existing E2E prerequisites: Go, Docker, and the tools installed by
the Makefile. It creates eight CPU-only pause Pods, with two LWS replica groups
of one leader and three workers each. No GPUs or model weights are required.
The standard E2E script creates and cleans up its Kind cluster and writes logs
and test reports to `ARTIFACTS`.

The Makefile uses the repository's default controller replica count and resource
requests. On a smaller VPS, prepare a dedicated test cluster with one controller
replica and resource requests that fit, then use the existing-cluster command
below. The test supports either one or multiple controller replicas.

To run against an already prepared test cluster with LWS installed:

```shell
bin/ginkgo -v --fail-on-empty --focus='LeaderWorkerSet recovery' ./test/e2e
```

The test uses the current kubeconfig, expects the `lws-controller-manager`
Deployment in `lws-system` (override the namespace with `LWS_NAMESPACE`), and
restarts **all** Pods belonging to that Deployment. Ginkgo runs the scenario
serially so the restart cannot overlap other E2E specs.

## Scenario and assertions

1. Wait for both replica groups to be running and ready; record their Pod and
   worker StatefulSet identities.
2. Add a test finalizer to group 0's leader, delete one worker, and wait until
   LWS starts foreground deletion of that leader and records one restart.
3. Restart every controller-manager Pod while the old leader is still held.
4. Wait for new controller processes to become ready, then remove the test
   finalizer and let recovery complete.
5. Check that every Pod in group 0 was replaced, every Pod in group 1 kept its
   UID, the replacement worker StatefulSet belongs to the new leader, and the
   workload has exactly the expected Pods and StatefulSets.
6. Check that the recovered Pods remain stable and the persisted restart count
   stays at one. A one-restart budget exposes accidental replay as exhaustion.

The finalizer makes the interruption point deterministic. Cleanup releases it
even if an assertion fails. This tests real Kubernetes controllers, garbage
collection, admission webhooks, and LWS process restart; it measures neither
model correctness nor GPU inference performance.
