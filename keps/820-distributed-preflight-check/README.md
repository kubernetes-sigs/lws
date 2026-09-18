# KEP-820: Bounded Group Recovery for LeaderWorkerSet

<!--
This KEP adds a per-replica restart budget to LeaderWorkerSet so repeated
group recreation stops at a user-selected limit while preserving terminating
Pod API objects for inspection and recovery coordination.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Stop a persistent recovery loop](#story-1-stop-a-persistent-recovery-loop)
    - [Story 2: Inspect and recover an exhausted group](#story-2-inspect-and-recover-an-exhausted-group)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
  - [Controller behavior](#controller-behavior)
  - [User-visible behavior](#user-visible-behavior)
  - [Status and recovery](#status-and-recovery)
  - [Counter lifetime](#counter-lifetime)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Set <code>Failed=True</code> on the LWS](#set--on-the-lws)
  - [Retain a running group after exhaustion](#retain-a-running-group-after-exhaustion)
  - [Use startup or readiness probes](#use-startup-or-readiness-probes)
  - [Use entrypoint wrappers or sidecars](#use-entrypoint-wrappers-or-sidecars)
<!-- /toc -->

## Summary

This KEP adds `leaderWorkerTemplate.maxGroupRestarts` to bound automatic group
recovery under `RecreateGroupOnPodRestart` and `RecreateGroupAfterStart`. When a
replica exhausts the budget, LeaderWorkerSet (LWS) stops automatic recovery,
terminates that group so its scheduled resources can be released, retains the
terminating Pod API objects with finalizers, and reports `Degraded=True`
without making the whole LWS terminal.

The retained objects preserve failure status and coordinate explicit recovery;
they do not guarantee that kubelet or CRI logs remain available. Workloads that
need complete diagnostics must export logs to an external logging system.

The budget applies to every failure for which either policy would recreate the
group. Init-container preflight checks are one use case, not a separate
lifecycle or status model.

## Motivation

The group-recreating restart policies currently have no upper bound. A
persistent failure can repeatedly delete and recreate an entire group, consume
cluster and control-plane resources, and discard the Pods that contain the most
useful failure state. Operators need a circuit breaker that stops this loop
while leaving healthy replicas and unrelated rollouts active.

### Goals

1. Stop LWS-initiated group recreation after a user-selected number of
   attempts.
2. Terminate the exhausted group so its scheduled resources, including GPUs,
   are released after its Pods become terminal.
3. Retain the terminating Pod API objects for status inspection and explicit
   recovery coordination.
4. Report partial failure without making the whole LWS terminal.
5. Define how users inspect and recover an exhausted group.
6. Preserve existing behavior when `maxGroupRestarts` is unset.
7. Apply the same budget semantics to both group-recreating restart policies.

### Non-Goals

1. Change kubelet container restart behavior.
2. Guarantee retention of native kubelet or CRI logs after Pod termination.
3. Add preflight-specific phases, images, scripts, or environment variables.
4. Roll back or repair the workload automatically.
5. Track restart budgets across the unstable identities used by
   `groupIdentity: Hash`.

## Proposal

`maxGroupRestarts` is a circuit breaker for LWS-initiated `RecreateGroup`
actions. Its exhaustion behavior is fixed: terminate the group, release its
scheduled resources after Pods become terminal, retain the Pod API objects with
finalizers when they can still receive them, and report `Degraded=True`. This KEP does not add a configurable
`restartBudgetExhaustionPolicy` such as `Retain` or `Terminate`. It does not
make the LWS or replica a Kubernetes terminal object.

### User Stories

#### Story 1: Stop a persistent recovery loop

An operator runs a distributed preflight check in init containers. One replica
fails consistently, so LWS recreates that group until it consumes the configured
budget. LWS then terminates the exhausted group, adds cleanup finalizers to the
leader and worker Pods that can still receive them, and stops automatic group
recovery. Once those Pods reach terminal state, their scheduled resources are
released while the retained API objects remain available for inspection. The
remaining replicas continue running.

#### Story 2: Inspect and recover an exhausted group

After the budget is exhausted, an operator inspects the retained Pod objects and
external logs. Once the underlying problem is fixed, the operator adds the
recovery annotation to the retained leader Pod. LWS clears that revision and
replica's count, removes the cleanup finalizers from the leader and worker Pods,
and lets the StatefulSet create a replacement group with a fresh budget.

### Notes/Constraints/Caveats

- Suppression covers only the LWS `RecreateGroup` action. At exhaustion, LWS
  starts deletion of the complete group and kubelet stops its containers; the
  finalizers retain the leader and worker Pod API objects while deletion is in
  progress.
- Once a retained Pod reaches `Succeeded` or `Failed`, its CPU, memory, and
  device-plugin resources are no longer counted for scheduling. Finalizers
  retain API objects, not running containers or resource reservations.
- This resource-release behavior relies on the Kubernetes 1.27+ Pod deletion
  flow, which transitions deleted Pods to a terminal phase before removing
  their API objects.
- A Pod that was already deleting before budget exhaustion cannot gain a new
  finalizer. LWS retains the remaining group Pod objects and still terminates
  the complete group.
- Native `kubectl logs` is best effort and may be unavailable as soon as the
  container runtime removes the terminated container. Operators should export
  diagnostics to an external logging system before relying on this lifecycle.
- Pod status comes from Kubernetes and the workload. A failed init container
  will usually appear as `Init:Error` or `Init:CrashLoopBackOff`; LWS does not
  change it to `Completed`.
- LWS deletion, scale-down, and rollout are teardown/lifecycle operations, not
  explicit recovery. They remove budget cleanup finalizers as appropriate without
  clearing counters as a recovery side effect or starting a new group.

### Risks and Mitigations

**Risk:** A small budget may stop recovery after a transient failure.

**Mitigation:** The field is optional. When unset, LWS keeps the current
unbounded recreation behavior. Before a group is exhausted, users can adjust
the limit at runtime.

**Risk:** Native logs may be unavailable after the group is terminated.

**Mitigation:** The status condition identifies the stopped recovery loop, and
external logging is the diagnostics contract. Finalizers preserve Pod API
objects and status while kubelet terminates containers and releases scheduling
resources.

**Risk:** Concurrent failures could corrupt or lose restart counts.

**Mitigation:** Counts have one LWS-level source of truth and are keyed by Pod
template revision and replica ordinal. Suppressed attempts are idempotent and
do not increment the count.

## Design Details

### API

This KEP adds one optional spec field and one condition type:

```go
type LeaderWorkerTemplate struct {
    // maxGroupRestarts is the maximum number of LWS-initiated group
    // recreations allowed for a replica before automatic recovery is
    // suppressed. When unset, group recreation remains unbounded.
    // +optional
    // +kubebuilder:validation:Minimum=0
    MaxGroupRestarts *int32 `json:"maxGroupRestarts,omitempty"`
}

const (
    LeaderWorkerSetDegraded LeaderWorkerSetConditionType = "Degraded"
)
```

The user-facing recovery signal is the leader Pod annotation
`leaderworkerset.sigs.k8s.io/recover=true`. Other values are ignored. The
controller uses `leaderworkerset.sigs.k8s.io/group-restart-budget-cleanup` as
an internal Pod finalizer while an exhausted group is retained.

`maxGroupRestarts` is valid with `restartPolicy: RecreateGroupOnPodRestart` or
`RecreateGroupAfterStart`. The validating webhook rejects other restart
policies. It also rejects `groupIdentity: Hash` because a recreated Hash group
receives a new identity and cannot use an ordinal-based restart counter safely.

No per-group phase or preflight-specific status is added. `Degraded=True` is an
LWS-level aggregate condition with reason `ReplicaRestartBudgetExceeded`.

### Controller behavior

For each failure for which `RecreateGroupOnPodRestart` or
`RecreateGroupAfterStart` would recreate the group:

1. If the replica has remaining budget, consume one restart and request leader
   deletion. Existing group recreation then creates a replacement.
2. If the budget is exhausted, mark the group as exhausted, add the cleanup
   finalizer to leader and worker Pods that can still receive it, and initiate deletion of the group.
   Set `Degraded=True` and do not allow the owner chain to create a replacement
   while the exhausted group's Pod objects are retained.
3. Repeated Pod events while the group is terminating do not increase the count,
   clear the counter, or create a replacement.
4. When the LWS is being deleted, or a scale-down/rollout removes the group,
   remove budget cleanup finalizers as part of teardown without treating it as
   explicit recovery.
5. When the retained leader has
   `leaderworkerset.sigs.k8s.io/recover=true`, clear the current
   revision/replica count, remove the cleanup finalizers from the group Pods,
   and allow normal StatefulSet replacement.

A recovery annotation that predates exhaustion is cleared when the exhausted
state is recorded; recovery must be requested after the group is exhausted.

The count records budget consumed when LWS proceeds to leader deletion. An
exhaustion/termination event does not increment the count.

### User-visible behavior

| State or user action | LWS action | User-visible result |
|---|---|---|
| Budget remains | Delete the leader and recreate the group | `Progressing=True` while recovery is active |
| Budget exhausted | Terminate the group, retain Pod API objects that can receive cleanup finalizers, and stop `RecreateGroup` | `Degraded=True`, reason `ReplicaRestartBudgetExceeded`; resources are released after Pods become terminal; `Progressing=False` if nothing else is progressing |
| Change `maxGroupRestarts` on an exhausted group | Keep the terminating group unchanged | Only the explicit recovery annotation resumes recovery |
| Increase `maxGroupRestarts` before exhaustion | Use the larger limit for the next failure | The current count is preserved |
| Decrease `maxGroupRestarts` | Keep the current group unchanged until its next failure | The next failure uses the smaller limit; if the current count already meets it, terminate and retain the group immediately |
| Delete only terminating workers | Do not treat the deletion as recovery; reconcile the group teardown | The retained leader and count remain unchanged |
| Add `leaderworkerset.sigs.k8s.io/recover=true` to the retained leader | Clear that revision/replica count and remove group cleanup finalizers | Recreate the whole group with a fresh budget |
| Delete all Pods in the exhausted group | Continue teardown; do not infer recovery from deletion | No replacement is created until explicit recovery or normal lifecycle cleanup |
| Delete the LWS | Remove budget cleanup finalizers as teardown proceeds | No counter reset or replacement group is triggered by recovery logic |
| Scale down the group or select it for replacement during a rollout | Remove budget cleanup finalizers and obsolete lifecycle state | The removed replica/revision is not recovered or recreated; its counter is cleaned up as lifecycle bookkeeping |
| Update the Pod template | Roll out a new revision | The new revision uses a fresh per-replica budget |

Editing or unsetting `maxGroupRestarts` does not resume an exhausted group. This
avoids making an unrelated Pod update an implicit recovery trigger. Before
exhaustion, a changed limit applies to the next failure and does not disrupt a
currently healthy group.

For an LWS named `serving` with ten replicas where group 0 is exhausted after an
init failure, the main columns are expected to look like while finalizers retain
the terminating Pod objects:

```text
$ kubectl get lws serving -o wide
NAME      READY   DESIRED   UP-TO-DATE   AGE
serving   9       10        10           1h

$ kubectl get pods
NAME            READY   STATUS   RESTARTS
serving-0       0/1     Error    1
serving-0-1     0/1     Error    1
```

The exact Pod status, phase, and restart count depend on the workload and
kubelet. After containers terminate, the Pod objects may show `Failed` or
`Succeeded` while their finalizers keep them in the API. The corresponding LWS
conditions, when no other rollout is active, are:

```text
Available=False
Progressing=False       Reason=ReplicaRestartBudgetExceeded
UpdateInProgress=False
Degraded=True           Reason=ReplicaRestartBudgetExceeded
```

`ReadyReplicas` continues to report ready groups. If another replica or rollout
is making progress, `Progressing=True` and `Degraded=True` can coexist.

The explicit recovery command for group 0 is:

```bash
kubectl annotate pod serving-0 \
  leaderworkerset.sigs.k8s.io/recover=true --overwrite
```

Deleting the LWS object is not group recovery; it deletes the whole workload.

### Status and recovery

The LWS carries the restart-count map. The exhausted leader carries the
exhausted-state marker, and the leader and worker Pods that can receive them
carry cleanup finalizers.
LWS initiates deletion of the group when the budget is exhausted, and the
finalizers keep the Pod API objects available while containers terminate. A
deletion timestamp on its own is not recovery: LWS must see
`leaderworkerset.sigs.k8s.io/recover=true` on the retained leader before
clearing the current revision and replica count and removing the group
finalizers.

Deleting the LWS is workload teardown, not group recovery. Once the LWS has a
deletion timestamp, the Pod controller removes budget cleanup finalizers from
the leader and worker Pods without changing restart counts or issuing another
`RecreateGroup`. Scale-down and rollout cleanup follow the same rule for groups
that are no longer part of the desired workload.

Readiness alone does not clear suppression or reset the budget. This prevents a
workload that briefly becomes ready before failing again from bypassing the
limit.

### Counter lifetime

The LWS annotation stores a JSON map whose keys use
`<revision>/<groupIndex>` and whose values are non-negative restart counts. The
counter survives controller restarts and LWS-initiated group recreation. Counts
from older Pod-template revisions are retained while a rollout is active and
ignored for recovery decisions. Once the rollout completes, LWS removes those
obsolete revision keys. LWS clears a count when:

- the retained leader receives the explicit recovery annotation; or
- scale-down or rollout cleanup removes the replica/revision from the desired
  workload.

Controller-initiated deletion at exhaustion does not clear an exhausted marker
or reset a counter. Changing `maxGroupRestarts` or merely becoming Ready does
not reset the counter either. LWS deletion removes finalizers for teardown but
does not clear counters as a recovery side effect.

### Test Plan

[X] I/we understand the owners of the involved components may require updates
to existing tests to make this code solid enough prior to committing the
changes necessary to implement this enhancement.

#### Unit tests

- Unset, zero, and non-zero budgets.
- Exact count behavior with no increment on a suppressed attempt.
- Exhaustion initiates group deletion, adds finalizers to leader and workers,
  and is idempotent across repeated events.
- `Degraded` and `Progressing` condition transitions.
- Recovery annotation, limit changes, new revisions, and scale-down cleanup.
- LWS deletion releases leader and worker Pod finalizers without treating
  teardown as recovery.

#### Integration tests

- Webhook acceptance with both group-recreating restart policies and rejection
  with other restart policies.
- Webhook rejection of `groupIdentity: Hash` with `maxGroupRestarts`.
- The controller behavior table above, including exhaustion-triggered group
  deletion, explicit annotation recovery, and LWS deletion.
- Spec updates that change restart policy only after clearing the limit.

#### e2e tests

- Allow one group recreation, terminate and retain the Pod API objects after
  exhaustion, observe terminal Pod state, preserve the exact count, and recover
  after adding the annotation to the retained leader.

### Graduation Criteria

**Alpha:**

- Add the optional API field, webhook validation, restart accounting,
  exhaustion-triggered termination/finalizer behavior, and `Degraded`
  condition.
- Cover exhaustion and explicit recovery with unit, integration, and e2e tests.
- Document status, resource release, API-object retention, and recovery
  commands.

**Beta:**

- Gather production feedback on restart limits and explicit recovery.
- Add metrics for suppressed group recreation if operators need alerting beyond
  status conditions and events.

**Stable:**

- No unresolved data-loss or controller-liveness issues in restart accounting
  and recovery.
- User documentation reflects operational experience from beta.

## Implementation History

- 2026-06-02: Initial draft.
- 2026-08: Clarified that the restart budget applies beyond preflight checks.
- 2026-08: Replaced whole-LWS terminal failure with bounded per-replica
  recovery, aggregate degradation, and explicit recovery semantics.
- 2026-08: Split init-phase DNS changes into separate work so this KEP covers
  only bounded group recovery.
- 2026-09: Changed exhaustion handling to terminate the group, retain terminal
  Pod objects with finalizers, release resources, and recover by annotation.

## Drawbacks

1. The feature adds API, condition, annotation, and finalizer behavior that LWS
   must support over time.
2. Native Pod logs may disappear when the terminated container is removed;
   external logging is required for durable diagnostics.
3. Recovery is intentionally operator-driven after exhaustion.

## Alternatives

### Set `Failed=True` on the LWS

Rejected because LWS is a continuously reconciled, multi-replica workload. One
exhausted replica should not make the whole object terminal.

### Retain a running group after exhaustion

Rejected because it keeps containers and device resources active after the
budget is exhausted. The selected design terminates the group and uses Pod
finalizers only to retain API objects and coordinate explicit recovery.

Native logs are not the retention contract; workloads that need durable
diagnostics must export them to an external logging system.

### Use startup or readiness probes

Rejected because probes do not bound group-level recreation.

### Use entrypoint wrappers or sidecars

Rejected because they couple the policy to workload images and do not provide
an LWS-level recovery budget.
