# KEP-820: Bounded Group Recovery for LeaderWorkerSet

<!-- toc -->
- [Summary](#summary)
- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User-visible behavior](#user-visible-behavior)
- [Design Details](#design-details)
  - [API](#api)
  - [Controller behavior](#controller-behavior)
  - [Status and recovery](#status-and-recovery)
  - [Counter lifetime](#counter-lifetime)
- [Risks and Drawbacks](#risks-and-drawbacks)
- [Alternatives](#alternatives)
- [Test Plan](#test-plan)
- [Implementation History](#implementation-history)
<!-- /toc -->

## Summary

This KEP adds `leaderWorkerTemplate.maxGroupRestarts` to bound automatic group
recovery under `RecreateGroupOnPodRestart`.

The restart budget applies to any failure that enters the
`RecreateGroupOnPodRestart` path. Init-container preflight checks are one use
case, not a separate lifecycle or status model.

## Goals

1. Stop unbounded LWS-initiated group recreation after a user-selected number
   of attempts.
2. Retain the current failed group for diagnosis after the budget is exhausted.
3. Report partial failure without making the whole LWS terminal.
4. Define clear user actions for recovery.
5. Preserve existing behavior when `maxGroupRestarts` is unset.

## Non-Goals

1. Change kubelet container restart behavior.
2. Release resources automatically after the restart budget is exhausted.
3. Add preflight-specific phases, images, scripts, or environment variables.
4. Roll back or repair the workload automatically.

## Proposal

`maxGroupRestarts` is a circuit breaker for LWS-initiated `RecreateGroup`
actions. It does not make the LWS or replica a Kubernetes terminal object.

### User-visible behavior

| State or user action | LWS action | Status and recovery |
|---|---|---|
| Budget remains | Delete the leader and recreate the group | `Progressing=True` while recovery is active |
| Budget exhausted | Stop LWS-initiated recreation and retain the current group | `Degraded=True`, reason `ReplicaRestartBudgetExceeded`; `Progressing=False` if nothing else is progressing |
| User deletes the retained leader | Clear that revision/replica count and create a replacement group | Logs from the deleted Pods are lost; `Degraded` clears after the replacement appears |
| User updates the Pod template | Roll out a new revision with a fresh per-replica budget | Normal rollout conditions apply |
| User increases `maxGroupRestarts` | Resume automatic recovery with the additional budget | The retained group may be recreated on the next reconcile |

`ReadyReplicas` continues to report the number of ready groups. `Degraded` is
orthogonal to rollout progress. If the retained replica is the only unfinished
work, LWS reports `Progressing=False`; another rollout can make
`Progressing=True` and `Degraded=True` coexist.

Example:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: serving
spec:
  leaderWorkerTemplate:
    restartPolicy: RecreateGroupOnPodRestart
    maxGroupRestarts: 3
```

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
2. If the budget is exhausted, do not delete the leader. Record that automatic
   recovery is suppressed for the current replica and set `Degraded=True`.
3. Repeated Pod events while suppressed are idempotent and do not increase the
   restart count.

The count records budget consumed when LWS proceeds to leader deletion.
Exhaustion is tracked separately, so a suppressed attempt does not increment
the count.

Suppression applies only to the LWS `RecreateGroup` action. Kubelet may continue
to restart containers, and StatefulSet, rollout, scale, eviction, and deletion
behavior remain active.

### Status and recovery

`Degraded=True` means at least one desired replica is currently retained after
exhausting its automatic recovery budget. It can coexist with
`Progressing=True` when another replica or rollout is making progress.

If no other replica or rollout is making progress, LWS sets
`Progressing=False` with reason `ReplicaRestartBudgetExceeded`. If other work is
still progressing, both `Progressing=True` and `Degraded=True` are valid.

Readiness alone does not clear the retained state or reset the budget. This
prevents a workload that briefly becomes ready before failing again from
bypassing the limit.

Deleting the retained leader is an explicit recovery action. A cleanup
finalizer clears that replica's suppressed state and count before deletion, and
the replacement starts with a fresh budget. A new Pod-template revision also
starts with a fresh budget.

### Counter lifetime

The counter is scoped to a replica ordinal and Pod-template revision. It
survives controller restarts and LWS-initiated group recreation. Counts from an
older Pod-template revision are ignored. LWS clears a count when:

- the user deletes the retained replica for recovery;
- scale-down removes the replica ordinal.

Increasing `maxGroupRestarts` adds usable budget to the current count. Merely
becoming Ready does not reset the counter.

## Risks and Drawbacks

1. A small restart budget may stop recovery after a transient failure. The
   field is opt-in and user controlled.
2. Retained Pods preserve the current diagnostic context but continue to reserve
   scheduled resources, including GPUs. Logs remain subject to kubelet and
   container-runtime retention.
3. Deleting the retained group releases its resources and logs. Users should
   collect diagnostics before recovery.
4. Counter and suppression updates must tolerate controller restarts and
   concurrent failures in different replicas.

## Alternatives

1. **Set `Failed=True` on the LWS.** Rejected because LWS is a continuously
   reconciled, multi-replica workload. One exhausted replica must not make the
   whole object terminal.
2. **Delete the group after exhaustion.** Rejected because its owner would
   recreate it and restart the loop. Preventing replacement would require a
   separate per-replica suspension design and would discard the current logs.
3. **Use startup or readiness probes.** Rejected because probes do not bound
   group-level recreation.
4. **Use entrypoint wrappers or sidecars.** Rejected because they couple the
   policy to workload images and do not provide an LWS-level recovery budget.

## Test Plan

1. Unit and integration tests cover unset, zero, and non-zero budgets; exact
   recreate counts; suppression without leader deletion; and idempotent events.
2. Status tests cover partial degradation, all replicas degraded, concurrent
   progress, and recovery to `Degraded=False`.
3. Recovery tests cover manual deletion, a new Pod-template revision, increasing
   the limit, scale-down cleanup, and controller restart.
4. An end-to-end test verifies that a repeatedly failing init container exhausts
   the budget, retains its group, and exposes the expected condition.

## Implementation History

- 2026-04-16: Initial draft.
- 2026-04-16: Clarified that the restart budget applies beyond preflight checks.
- 2026-08: Replaced whole-LWS terminal failure with bounded per-replica recovery,
  aggregate degradation, and explicit recovery semantics.
- 2026-08: Split init-phase DNS changes into separate work so this KEP covers
  only bounded group recovery.
