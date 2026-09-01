# KEP-820: Bounded Group Recovery for LeaderWorkerSet

<!--
This KEP adds a per-replica restart budget to LeaderWorkerSet so repeated
group recreation stops at a user-selected limit while preserving the failed
Pods for inspection.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Stop a persistent recovery loop](#story-1-stop-a-persistent-recovery-loop)
    - [Story 2: Inspect and recover a retained group](#story-2-inspect-and-recover-a-retained-group)
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
  - [Delete the group after exhaustion](#delete-the-group-after-exhaustion)
  - [Use startup or readiness probes](#use-startup-or-readiness-probes)
  - [Use entrypoint wrappers or sidecars](#use-entrypoint-wrappers-or-sidecars)
<!-- /toc -->

## Summary

This KEP adds `leaderWorkerTemplate.maxGroupRestarts` to bound automatic group
recovery under `RecreateGroupOnPodRestart`. When a replica exhausts the budget,
LeaderWorkerSet (LWS) stops recreating that group, retains its Pods for
inspection, and reports `Degraded=True` without making the whole LWS terminal.

The budget applies to every failure handled by `RecreateGroupOnPodRestart`.
Init-container preflight checks are one use case, not a separate lifecycle or
status model.

## Motivation

`RecreateGroupOnPodRestart` currently has no upper bound. A persistent failure
can repeatedly delete and recreate an entire group, consume cluster and
control-plane resources, and discard the Pods that contain the most useful
failure state. Operators need a circuit breaker that stops this loop while
leaving healthy replicas and unrelated rollouts active.

### Goals

1. Stop LWS-initiated group recreation after a user-selected number of
   attempts.
2. Retain the current failed group for diagnosis after the budget is exhausted.
3. Report partial failure without making the whole LWS terminal.
4. Define how users inspect and recover a retained group.
5. Preserve existing behavior when `maxGroupRestarts` is unset.

### Non-Goals

1. Change kubelet container restart behavior.
2. Release resources automatically after the restart budget is exhausted.
3. Add preflight-specific phases, images, scripts, or environment variables.
4. Roll back or repair the workload automatically.

## Proposal

`maxGroupRestarts` is a circuit breaker for LWS-initiated `RecreateGroup`
actions. It does not make the LWS or replica a Kubernetes terminal object.

### User Stories

#### Story 1: Stop a persistent recovery loop

An operator runs a distributed preflight check in init containers. One replica
fails consistently, so LWS recreates that group until it consumes the configured
budget. LWS then retains the failed Pods and stops automatic group recreation.
The remaining replicas continue running.

#### Story 2: Inspect and recover a retained group

After the budget is exhausted, an operator inspects the retained Pods and their
logs. Once the underlying problem is fixed, the operator deletes the retained
leader Pod. LWS clears that revision and replica's count before deletion, then
the StatefulSet creates a replacement group with a fresh budget.

### Notes/Constraints/Caveats

- Suppression covers only the LWS `RecreateGroup` action. Kubelet may continue
  restarting containers in retained Pods.
- Retained Pods preserve logs and status but continue reserving scheduled
  resources, including GPUs.
- Pod status comes from Kubernetes and the workload. A failed init container
  will usually appear as `Init:Error` or `Init:CrashLoopBackOff`; LWS does not
  change it to `Completed`.
- Deleting retained Pods discards their local logs. Operators should collect
  diagnostics first.

### Risks and Mitigations

**Risk:** A small budget may stop recovery after a transient failure.

**Mitigation:** The field is optional. When unset, LWS keeps the current
unbounded recreation behavior. Users can also increase the limit at runtime.

**Risk:** Retained Pods keep their resource reservations.

**Mitigation:** The status condition identifies the stopped recovery loop. The
operator chooses when to collect diagnostics and delete the retained leader to
release and recreate the group.

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

`maxGroupRestarts` is valid only with `restartPolicy:
RecreateGroupOnPodRestart`. The validating webhook rejects other combinations.

No per-group phase or preflight-specific status is added. `Degraded=True` is an
LWS-level aggregate condition with reason `ReplicaRestartBudgetExceeded`.

### Controller behavior

For each failure handled by `RecreateGroupOnPodRestart`:

1. If the replica has remaining budget, consume one restart and request leader
   deletion. Existing group recreation then creates a replacement.
2. If the budget is exhausted, do not delete the leader. Mark automatic
   recovery as suppressed for the current replica and set `Degraded=True`.
3. Repeated Pod events while suppressed do not increase the count.

The count records budget consumed when LWS proceeds to leader deletion. A
suppressed attempt does not increment the count.

### User-visible behavior

| State or user action | LWS action | User-visible result |
|---|---|---|
| Budget remains | Delete the leader and recreate the group | `Progressing=True` while recovery is active |
| Budget exhausted | Retain the group and stop `RecreateGroup` | `Degraded=True`, reason `ReplicaRestartBudgetExceeded`; `Progressing=False` if nothing else is progressing |
| Increase `maxGroupRestarts` | Apply the larger limit on the next failure-triggered Pod reconcile | If budget is available, clear suppression, consume one restart, and recreate the group |
| Decrease `maxGroupRestarts` | Keep the current group unchanged until its next failure | The next failure uses the smaller limit; if the current count already meets it, retain the group immediately |
| Delete only retained workers | Allow their StatefulSet to replace them | Leader remains retained and the count does not change |
| Delete the retained leader | Clear that revision/replica count, then allow deletion | Recreate the whole group with a fresh budget |
| Delete all Pods in the retained group | Process the leader deletion as explicit recovery | Recreate the whole group with a fresh budget |
| Update the Pod template | Roll out a new revision | The new revision uses a fresh per-replica budget |

Editing `maxGroupRestarts` alone does not directly delete a retained Pod. A
larger limit resumes automatic recreation when the Pod controller next handles
a failure event. A smaller limit affects the next failure and does not disrupt a
currently healthy group.

For an LWS named `serving` with ten replicas where group 0 is retained after an
init failure, the main columns are expected to look like:

```text
$ kubectl get lws serving -o wide
NAME      READY   DESIRED   UP-TO-DATE   AGE
serving   9       10        10           1h

$ kubectl get pods
NAME            READY   STATUS                  RESTARTS
serving-0       0/1     Init:CrashLoopBackOff   7
serving-0-1     0/1     Init:CrashLoopBackOff   7
```

The exact Pod status and restart count depend on the workload and kubelet. The
corresponding LWS conditions, when no other rollout is active, are:

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
kubectl delete pod serving-0
```

Deleting the LWS object is not group recovery; it deletes the whole workload.

### Status and recovery

The retained leader carries an exhausted-state annotation and a cleanup
finalizer. The finalizer clears the current revision and replica count before
manual leader deletion completes. If the LWS itself no longer exists, the Pod
controller removes the finalizer without attempting counter cleanup.

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

- the user deletes the retained leader for recovery; or
- scale-down removes the replica ordinal.

Increasing `maxGroupRestarts` adds usable budget to the current count. Merely
becoming Ready does not reset the counter.

### Test Plan

[X] I/we understand the owners of the involved components may require updates
to existing tests to make this code solid enough prior to committing the
changes necessary to implement this enhancement.

#### Unit tests

- Unset, zero, and non-zero budgets.
- Exact count behavior with no increment on a suppressed attempt.
- Retention without leader deletion and idempotent repeated events.
- `Degraded` and `Progressing` condition transitions.
- Manual deletion, increased limits, new revisions, and scale-down cleanup.

#### Integration tests

- Webhook acceptance with `RecreateGroupOnPodRestart` and rejection with other
  restart policies.
- Spec updates that change restart policy only after clearing the limit.

#### e2e tests

- Allow one group recreation, retain the group after exhaustion, preserve the
  exact count, and recover after deleting the retained leader.

### Graduation Criteria

**Alpha:**

- Add the optional API field, webhook validation, restart accounting, retained
  group behavior, and `Degraded` condition.
- Cover exhaustion and manual recovery with unit, integration, and e2e tests.
- Document status, resource retention, and recovery commands.

**Beta:**

- Gather production feedback on restart limits and manual recovery.
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

## Drawbacks

1. The feature adds API, condition, annotation, and finalizer behavior that LWS
   must support over time.
2. Preserving the failed Pods also preserves their resource reservations.
3. Recovery is intentionally operator-driven after exhaustion.

## Alternatives

### Set `Failed=True` on the LWS

Rejected because LWS is a continuously reconciled, multi-replica workload. One
exhausted replica should not make the whole object terminal.

### Delete the group after exhaustion

Rejected because its owner would recreate it and restart the loop. Preventing
replacement would require a separate per-replica suspension design and would
discard the current logs.

### Use startup or readiness probes

Rejected because probes do not bound group-level recreation.

### Use entrypoint wrappers or sidecars

Rejected because they couple the policy to workload images and do not provide
an LWS-level recovery budget.
