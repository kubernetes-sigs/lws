# KEP-956: Pod Termination Policy API

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Fast group rebuild upon leader failure](#story-1-fast-group-rebuild-upon-leader-failure)
    - [Story 2: Fast scale-down and rolling updates](#story-2-fast-scale-down-and-rolling-updates)
    - [Story 3: Fast LeaderWorkerSet deletion](#story-3-fast-leaderworkerset-deletion)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
  - [Webhook Defaulting and Validation](#webhook-defaulting-and-validation)
  - [LeaderWorkerSet Controller Changes](#leaderworkerset-controller-changes)
  - [Pod Controller Changes](#pod-controller-changes)
  - [Rebuild Coordination and Race Conditions](#rebuild-coordination-and-race-conditions)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

This KEP introduces a `spec.podTerminationPolicy` field to the `LeaderWorkerSet` API with two values: `Default` and `Parallel`.

Under `Default`, existing behavior is preserved: when a leader pod is deleted, the Kubernetes garbage collector sequentially deletes the worker StatefulSet and worker pods only after the leader pod has completely terminated and exited etcd.

Under `Parallel`, when a leader pod is deleting (`DeletionTimestamp != nil`) or when the `LeaderWorkerSet` is being deleted, the controller proactively issues foreground deletion for the worker StatefulSet, terminating both the leader pod and worker pods concurrently. This significantly reduces group teardown and recreation latency, especially for distributed AI/ML workloads with long `terminationGracePeriodSeconds`.

## Motivation

In `LeaderWorkerSet`, the resource ownership hierarchy is structured as:
`LeaderWorkerSet -> Leader StatefulSet -> Leader Pod -> Worker StatefulSet -> Worker Pods`.

When a worker pod fails and `restartPolicy: RecreateGroupOnPodRestart` is used, the pod controller initiates foreground deletion on the leader pod, terminating both the leader pod and worker pods in parallel.

However, in several standard scenarios:
1. **Leader Pod Failure / Deletion**: When a leader pod is deleted or evicted, the worker StatefulSet is owned by the leader pod. Kubernetes background cascading deletion waits until the leader pod is completely gone before marking child resources (the worker StatefulSet and its pods) for deletion.
2. **Scale-Down and Rolling Updates**: When scaling down or performing rolling updates, leader pods are deleted first. The associated worker pods are deleted sequentially after their respective leader pod finishes its termination grace period.
3. **LeaderWorkerSet Deletion**: When an entire LWS is deleted, worker pods wait for the leader pods to finish shutting down before their deletion starts.

In large-scale AI/ML and distributed training/inference workloads, pods often configure significant `terminationGracePeriodSeconds` (e.g., 60–300 seconds) to flush model checkpoints, release GPU resources, or gracefully drain traffic. Sequential deletion means group teardown takes up to `2 * terminationGracePeriodSeconds` (e.g., 120s or more).

By allowing concurrent termination of leader and worker pods, group rebuild time and LWS teardown time can be reduced by ~50%.

### Goals

1. Provide an opt-in `spec.podTerminationPolicy` field in the `LeaderWorkerSet` specification.
2. Support `Default` (sequential cascading deletion) and `Parallel` (concurrent foreground deletion).
3. Ensure 100% backward compatibility for existing `LeaderWorkerSet` manifests and workloads.
4. Accelerate leader pod deletion, scale-down, rolling updates, and LWS deletion.
5. Prevent race conditions during group replacement by ensuring the old worker StatefulSet is completely deleted before a new replacement group's worker StatefulSet is created.

### Non-Goals

1. Changing container-level preStop hooks or overriding Kubernetes `terminationGracePeriodSeconds`.
2. Modifying pod termination behavior for other controllers outside of `LeaderWorkerSet`.

## Proposal

We propose adding `spec.podTerminationPolicy` with enum values `Default` and `Parallel`.
The field is defaulted to `Default` by both the CRD schema and the defaulting webhook.

When configured as `Parallel`:
1. The `LeaderWorkerSet` controller injects the annotation `leaderworkerset.sigs.k8s.io/pod-termination-policy: Parallel` into the leader pod template.
2. When a leader pod enters termination (`DeletionTimestamp != nil`), the pod controller detects the `Parallel` policy (via LWS spec or the leader pod annotation) and immediately deletes the associated worker StatefulSet with `metav1.DeletePropagationForeground`.
3. When the `LeaderWorkerSet` itself is deleted (`DeletionTimestamp != nil`), the `LeaderWorkerSet` controller immediately deletes all worker StatefulSets with `metav1.DeletePropagationForeground`.
4. If a replacement leader pod is created while the old worker StatefulSet is still terminating, the pod controller requeues reconciliation until the old worker StatefulSet is fully removed, ensuring clean state handoff.

### User Stories

#### Story 1: Fast group rebuild upon leader failure

A distributed training job runs with 1 leader and 7 workers, each having `terminationGracePeriodSeconds: 120`. When the leader fails or is preempted, under `Default`, the group rebuild takes over 240 seconds because workers only start terminating after the leader is completely removed. With `podTerminationPolicy: Parallel`, leader and workers terminate concurrently in 120 seconds, halving downtime.

#### Story 2: Fast scale-down and rolling updates

An inference service autoscales down replicas or applies a configuration rolling update. With `podTerminationPolicy: Parallel`, the workers of terminating groups begin termination simultaneously with the leader pod, freeing cluster resources and GPU nodes twice as fast.

#### Story 3: Fast LeaderWorkerSet deletion

An operator deletes a `LeaderWorkerSet`. With `podTerminationPolicy: Parallel`, all leader and worker pods across all replicas terminate concurrently rather than waiting for multi-tiered cascading GC.

### Notes/Constraints/Caveats

- When using `Parallel`, workers will receive termination signals (`SIGTERM`) at the same time as the leader pod. Workloads relying on the leader remaining alive while workers finish should use `Default`.
- The leader pod annotation `leaderworkerset.sigs.k8s.io/pod-termination-policy` ensures parallel termination functions correctly even if the parent `LeaderWorkerSet` resource has already been deleted.

### Risks and Mitigations

- **Risk**: A replacement leader pod might attempt to adopt or create a worker StatefulSet while the previous one is still terminating.
  - **Mitigation**: The pod controller checks if the existing worker StatefulSet has `DeletionTimestamp != nil`. If so, it requeues reconciliation with a short delay (1s) until the old worker StatefulSet is deleted.
- **Risk**: Backward compatibility impact.
  - **Mitigation**: The default value is `Default`, preserving identical existing behavior unless explicitly opted into `Parallel`.

## Design Details

### API

In `api/leaderworkerset/v1/leaderworkerset_types.go`:

```go
type PodTerminationPolicyType string

const (
    DefaultPodTerminationPolicy  PodTerminationPolicyType = "Default"
    ParallelPodTerminationPolicy PodTerminationPolicyType = "Parallel"
)

const (
    PodTerminationPolicyAnnotationKey = "leaderworkerset.sigs.k8s.io/pod-termination-policy"
)

type LeaderWorkerSetSpec struct {
    // ...
    // PodTerminationPolicy indicates the policy for terminating pods in a group.
    // Default: leader pod terminates first, followed sequentially by workers via garbage collection.
    // Parallel: leader and worker pods terminate concurrently.
    // Defaults to Default.
    // +kubebuilder:default=Default
    // +kubebuilder:validation:Enum=Default;Parallel
    // +optional
    PodTerminationPolicy PodTerminationPolicyType `json:"podTerminationPolicy,omitempty"`
}
```

### Webhook Defaulting and Validation

- **Defaulting**: If `spec.podTerminationPolicy` is empty, the mutating webhook defaults it to `DefaultPodTerminationPolicy`.
- **Validation**: The validating webhook verifies that `spec.podTerminationPolicy` is either `Default` or `Parallel`. Invalid values are rejected. Updates between valid values are supported.

### LeaderWorkerSet Controller Changes

1. **Pod Template Annotation**: In `buildLeaderPodTemplateApplyConfiguration`, the controller injects `leaderworkerset.sigs.k8s.io/pod-termination-policy` into the leader pod template with the value of `lws.Spec.PodTerminationPolicy`.
2. **Proactive LWS Deletion**: In `deleteWorkerStatefulSets`, when `lws.DeletionTimestamp != nil` and `lws.Spec.PodTerminationPolicy == Parallel`, the controller deletes all worker StatefulSets with `metav1.DeletePropagationForeground`.

### Pod Controller Changes

1. **Helper `isParallelPodTermination`**: Checks whether `lws.Spec.PodTerminationPolicy == Parallel` or the leader pod has `leaderworkerset.sigs.k8s.io/pod-termination-policy: Parallel`.
2. **Helper `deleteWorkerStatefulSetIfExists`**: Fetches the worker StatefulSet owned by the leader pod (validating UID) and deletes it with `metav1.DeletePropagationForeground`.
3. **Leader Deletion Handling**: When reconciling a leader pod with `DeletionTimestamp != nil`, if `isParallelPodTermination` returns true, `deleteWorkerStatefulSetIfExists` is called.

### Rebuild Coordination and Race Conditions

When group replacement occurs (e.g. `RecreateGroupOnPodRestart` or rolling updates), a new leader pod might be scheduled while the old worker StatefulSet is terminating.
In `pod_controller.go`:
```go
if workerSts.DeletionTimestamp != nil {
    log.V(2).Info("Worker statefulSet is terminating, requeue", "workerSts", workerSts.Name)
    return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
}
```
This guarantees that the replacement leader will wait for the old worker StatefulSet to be fully purged from etcd before creating a fresh worker StatefulSet.

### Test Plan

#### Unit tests
- Webhook defaulting and validation unit tests in `pkg/webhooks/leaderworkerset_webhook_test.go`.
- Controller apply configuration and proactive LWS deletion tests in `pkg/controllers/leaderworkerset_controller_test.go`.
- Pod controller parallel deletion and requeue tests in `pkg/controllers/pod_controller_test.go`.

#### Integration tests
- Defaulting and validation integration tests with envtest in `test/integration/webhooks/leaderworkerset_test.go`.
- Controller integration tests covering group lifecycle with `podTerminationPolicy: Parallel`.

#### e2e tests
- End-to-end test verifying concurrent termination of leader and worker pods upon leader pod deletion and LWS deletion.

### Graduation Criteria

- Alpha in current release with unit and integration test coverage.
- Graduate to Beta after community feedback and verification across production distributed workloads.

## Implementation History

- 2026-09-29: Initial KEP created for issue #956.

## Drawbacks

- Workers receive termination signals simultaneously with the leader, which may not be suitable for workloads where workers depend on an active leader during shutdown. For those workloads, `Default` should be used.

## Alternatives

- **Always parallel deletion**: Making parallel deletion the default without an opt-in field. This was rejected because it would break backward compatibility for workloads requiring graceful ordered shutdown.
- **Custom finalizers on leader pods**: Using finalizers to delay leader pod deletion until workers terminate. This would increase complexity and make teardown even slower rather than faster.
