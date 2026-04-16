
# KEP-NNN: Fail-Fast Restart Budget and Init-Phase DNS for LeaderWorkerSet

<!-- toc -->
- [Summary](#summary)
- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Proposal](#proposal)
- [Why No Failed Before](#why-no-failed-before)
- [Design Details](#design-details)
  - [API](#api)
  - [Controller Behavior](#controller-behavior)
  - [Status Semantics](#status-semantics)
  - [Operational Notes](#operational-notes)
- [Risks and Mitigations](#risks-and-mitigations)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Test Plan](#test-plan)
- [Implementation History](#implementation-history)
<!-- /toc -->

## Summary

This KEP adds two opt-in capabilities to LeaderWorkerSet (LWS):

1. `leaderWorkerTemplate.maxGroupRestarts` plus a terminal `Failed` condition for bounded retries.
2. `networkConfig.publishNotReadyAddresses` to make peer FQDN resolvable in init phase (secondary to fail-fast).

This proposal addresses two generic LWS gaps:
- group recreation can loop forever without a terminal failure boundary.
- peer DNS FQDN may be unavailable during init-container phase unless explicitly published.
It applies to any workload with init-phase peer communication and to any repeated
group recreation caused by failures in init-containers or main containers.


### Story

When users run distributed pre-checks (for example NCCL tests) in LWS init-containers,
a failure can push the group into an infinite recreate loop in the `RecreateGroupOnPodRestart` path.
This KEP adds a fail-fast boundary after N group recreation attempts, and also ensures
leader FQDN can be resolved during init phase when explicitly enabled.


## Goals

1. Provide a native fail-fast mechanism after N group recreation attempts,
   regardless of whether the trigger is init-container failure or main-container failure.
2. Enable init-containers to resolve `LWS_LEADER_ADDRESS` during init phase.
3. Keep both features backward compatible and opt-in (`false`/`nil` by default).
4. Reuse existing env vars (`LWS_LEADER_ADDRESS`, `LWS_GROUP_SIZE`, `LWS_WORKER_INDEX`).

## Non-Goals

1. Add new env vars for this feature.
2. Ship built-in preflight images/scripts.
3. Change container-level restart semantics.
4. Add automatic remediation after failure.

## Proposal

Decision matrix:

| Problem | Solution | Origin |
|---|---|---|
| Infinite recreate loop, no terminal state (including main-container repeated failure) | `maxGroupRestarts` + `Failed` condition | LWS requirement |
| Init-container cannot resolve leader FQDN | `networkConfig.publishNotReadyAddresses: true` | Kubeflow Trainer PR #3417 discussion |
| Opt-in vs opt-out concern | Both fields are opt-in (`nil/false` default) | Maintainer feedback pattern in Trainer |
| "Why not startup/readiness probe?" | Keep as rejected alternative | Trainer discussion on probe semantics |
| Need more env vars? | No, reuse existing LWS envs | Existing LWS behavior |

User-visible behavior change:

1. Default behavior is unchanged.
   - If `maxGroupRestarts` is unset, group recreation remains unbounded.
   - If `publishNotReadyAddresses` is unset/false, init-phase DNS behavior stays as today.
2. If users set `leaderWorkerTemplate.maxGroupRestarts: N`,
   LWS allows at most `N` group recreations in `RecreateGroupOnPodRestart` path.
3. After the limit is exceeded, LWS sets `Failed=True` and stops further group recreation.
4. `Failed` is a new LWS condition type added by this KEP.
   - Current built-in conditions are `Available`, `Progressing`, `UpdateInProgress`.
   - Controllers/clients that parse `status.conditions` should tolerate and handle the new type.
5. If users set `networkConfig.publishNotReadyAddresses: true`,
   peer FQDN can be resolved during init phase.

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
  networkConfig:
    subdomainPolicy: UniquePerReplica
    publishNotReadyAddresses: true
```

## Why No Failed Before

Historically, LWS was designed with a "keep reconciling" model:

1. Built-in conditions (`Available`, `Progressing`, `UpdateInProgress`) describe
   availability/progress/rollout, not terminal failure.
2. For restart paths, previous behavior assumed continuous self-healing was preferred
   over introducing a terminal state at LWS level.
3. There was no user-configured retry budget in API, so introducing `Failed` would
   have been ambiguous ("failed after how many attempts?").

This KEP changes that precondition by adding an explicit budget
(`maxGroupRestarts`). With a clear threshold, `Failed` now has deterministic and
user-controlled semantics, and avoids unbounded recreate loops.

## Design Details

### API

API changes in this KEP:

1. **Spec field additions (new user knobs)**:
   - `spec.leaderWorkerTemplate.maxGroupRestarts` (*int32, optional, minimum 0)
   - `spec.networkConfig.publishNotReadyAddresses` (bool, default false)
2. **Status semantic extension**:
   - no new status field is added;
   - `status.conditions[]` introduces a new condition type value: `Failed`.
3. **Compatibility**:
   - CRD schema shape for `status.conditions` stays the same (`[]metav1.Condition`);
   - clients/controllers that switch on known condition types must handle unknown/new values safely.

```go
type NetworkConfig struct {
    // +kubebuilder:validation:Enum={Shared,UniquePerReplica}
    SubdomainPolicy *SubdomainPolicy `json:"subdomainPolicy"`
    // +optional
    // +kubebuilder:default=false
    PublishNotReadyAddresses bool `json:"publishNotReadyAddresses,omitempty"`
}

type LeaderWorkerTemplate struct {
    // +optional
    // +kubebuilder:validation:Minimum=0
    MaxGroupRestarts *int32 `json:"maxGroupRestarts,omitempty"`
}

const (
    LeaderWorkerSetFailed LeaderWorkerSetConditionType = "Failed"
)

const (
    GroupRestartCountAnnotationKey = "leaderworkerset.sigs.k8s.io/group-restart-count"
)
```

### Controller Behavior

1. Restart reconcile path:
   for `RecreateGroupOnPodRestart`, controller checks group restart budget before leader deletion.
2. In `RecreateGroupOnPodRestart` path, before deleting leader pod:
   - read `group-restart-count` annotation;
   - if `count >= maxGroupRestarts`, stop recreation and mark replica failed;
   - otherwise increment annotation and continue current recreation flow.
3. The counter is group-level, not init-only: any failure path that enters
   `RecreateGroupOnPodRestart` contributes to the same retry budget.
4. Service reconcile sets:
   `svc.Spec.PublishNotReadyAddresses = spec.networkConfig.publishNotReadyAddresses`.
5. Counter persistence is annotation-based so controller restarts do not reset state.

### Status Semantics

1. `maxGroupRestarts` unset: current behavior (unbounded recreation).
2. `maxGroupRestarts: 0`: first group-level failure becomes terminal.
3. `maxGroupRestarts` is effective only for `restartPolicy: RecreateGroupOnPodRestart`.
4. Any replica exceeding limit sets LWS condition `Failed=True`
   with reason `MaxGroupRestartsExceeded`.
5. Failed replica is excluded from available-ready accounting.

### Operational Notes

1. Restart counting:
   one "group restart" means one leader deletion in `RecreateGroupOnPodRestart` path.
2. Counter persistence:
   `group-restart-count` annotation is used so controller restart does not reset budget.
3. After terminal failure:
   no further group recreation is attempted for the failed replica; pod is retained for debugging.
4. DNS behavior:
   with `publishNotReadyAddresses: false`, peer FQDN may not be resolvable in init phase.
5. `startupPolicy: LeaderReady` interaction:
   worker groups are still created only after leader is ready; this proposal does not alter that gate.

## Risks and Mitigations

1. Strict `maxGroupRestarts` may fail transient issues.
   Mitigation: opt-in, user-controlled threshold.
2. Annotation increment and delete are not fully atomic.
   Mitigation: acceptable best-effort bound for fail-fast policy.
3. `publishNotReadyAddresses=true` exposes DNS records before readiness.
   Mitigation: opt-in, LWS-owned scoped headless Service.

## Drawbacks

1. A strict restart budget can convert transient failures into terminal failure.
2. `publishNotReadyAddresses=true` publishes not-ready pod DNS records by design.
3. Keeping failed pods for debugging may hold cluster resources until user cleanup.

## Alternatives

1. Startup/readiness probes: rejected.
   Probes run with main process and do not provide strict pre-main gating; they also
   do not solve init-phase DNS publish timing without `publishNotReadyAddresses`.
2. Entrypoint wrapper script: rejected due to image/command coupling.
3. Sidecar-based checks: rejected due to lifecycle mismatch for one-shot gate.
4. User manually patches Service: rejected because controller reconciliation overwrites it.

## Test Plan

1. Unit:
   - restart counter read/increment logic;
   - threshold transition to `Failed`.
   - service construction propagates `publishNotReadyAddresses`.
2. Integration:
   - bounded recreate behavior with `maxGroupRestarts`;
   - no regression when `maxGroupRestarts` is unset;
   - DNS publish behavior with `publishNotReadyAddresses=true`.
3. e2e:
   - init-container peer check succeeds with publish enabled;
   - repeated init failure reaches `Failed` after configured limit.

## Implementation History

- 2026-04-16: Initial draft.
- 2026-04-16: Simplified structure and clarified universal scope beyond preflight-only use.
