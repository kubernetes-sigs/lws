
# KEP-820: Fail-Fast Restart Budget and Init-Phase DNS for LeaderWorkerSet

<!-- toc -->
- [Summary](#summary)
  - [Story](#story)
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

This KEP adds two capabilities to LeaderWorkerSet (LWS):

1. `leaderWorkerTemplate.maxGroupRestarts` plus a terminal `Failed` condition for bounded retries.
2. `networkConfig.publishNotReadyAddresses` to make the (previously hardcoded)
   publish-not-ready behavior an explicit, user-controllable knob, plus eager
   per-leader headless Service creation so peer FQDNs exist from the start of
   the init phase.

This proposal addresses two generic LWS gaps:
- group recreation can loop forever without a terminal failure boundary.
- peer DNS FQDN publication during the init-container phase depends on a
  per-replica Service that is created lazily (after the leader pod appears),
  so early init containers can hit NXDOMAIN races.
It applies to any workload with init-phase peer communication and to any repeated
group recreation caused by failures in init-containers or main containers.


### Story

When users run distributed pre-checks (for example NCCL tests) in LWS init-containers,
a failure can push the group into an infinite recreate loop in the `RecreateGroupOnPodRestart` path.
This KEP adds a fail-fast boundary after N group recreation attempts, and also makes
leader FQDN resolvable from the start of the init phase (eager per-leader Service
creation; not-ready address publishing stays on by default as it always has been).


## Goals

1. Provide a native fail-fast mechanism after N group recreation attempts,
   regardless of whether the trigger is init-container failure or main-container failure.
2. Enable init-containers to resolve `LWS_LEADER_ADDRESS` during init phase.
3. Keep the `maxGroupRestarts` feature backward compatible and opt-in (`nil` by default);
   `publishNotReadyAddresses` defaults to the historical behavior (publish, i.e. `true`).
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
| Init-container DNS publication is an implicit hardcoded behavior and per-replica Services are created lazily | `networkConfig.publishNotReadyAddresses` knob (default `true`, the historical behavior) + eager per-leader headless Service creation | Kubeflow Trainer PR #3417 discussion |
| Opt-in vs opt-out concern | `maxGroupRestarts` is opt-in (`nil` default); `publishNotReadyAddresses` keeps the legacy default | Maintainer feedback pattern in Trainer |
| "Why not startup/readiness probe?" | Keep as rejected alternative | Trainer discussion on probe semantics |
| Need more env vars? | No, reuse existing LWS envs | Existing LWS behavior |

User-visible behavior change:

1. Default behavior is unchanged.
   - If `maxGroupRestarts` is unset, group recreation remains unbounded.
   - If `publishNotReadyAddresses` is unset (`nil`), LWS-owned headless Services
     keep publishing not-ready addresses exactly as they always have (effectively `true`).
2. If users set `leaderWorkerTemplate.maxGroupRestarts: N`,
   LWS allows at most `N` group recreations in `RecreateGroupOnPodRestart` path.
3. After the limit is exceeded, LWS sets `Failed=True` and stops further group recreation.
4. `Failed` is a new LWS condition type added by this KEP.
   - Current built-in conditions are `Available`, `Progressing`, `UpdateInProgress`.
   - Controllers/clients that parse `status.conditions` should tolerate and handle the new type.
5. If users set `networkConfig.publishNotReadyAddresses: false`,
   LWS-owned headless Services stop publishing not-ready pod addresses, which
   disables init-phase DNS publication.
6. Under `UniquePerReplica`, the LWS reconciler now eagerly creates one headless
   Service per leader pod, so the leader's DNS records exist before any init
   container starts and no longer race with the pod reconciler's lazy creation.

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
    publishNotReadyAddresses: true # optional: true is the default
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
   - `spec.networkConfig.publishNotReadyAddresses` (*bool, optional, default true)
2. **Validation constraint**: `maxGroupRestarts` is only valid when `restartPolicy` is `RecreateGroupOnPodRestart`. The validating webhook rejects any LWS with `maxGroupRestarts` set but a different restart policy.
3. **Status semantic extension**:
   - no new status field is added;
   - `status.conditions[]` introduces a new condition type value: `Failed`.
4. **Compatibility**:
   - CRD schema shape for `status.conditions` stays the same (`[]metav1.Condition`);
   - `publishNotReadyAddresses` defaults to `true`, which preserves the historical
     behavior where LWS-owned headless Services always published not-ready addresses;
   - clients/controllers that switch on known condition types must handle unknown/new values safely.

```go
type NetworkConfig struct {
    // +kubebuilder:validation:Enum={Shared,UniquePerReplica}
    SubdomainPolicy *SubdomainPolicy `json:"subdomainPolicy"`
    // +optional
    // +kubebuilder:default=true
    PublishNotReadyAddresses *bool `json:"publishNotReadyAddresses,omitempty"`
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

1. **Webhook validation**:
   before creating or updating an LWS, the validating webhook checks that `maxGroupRestarts` is only set when `restartPolicy: RecreateGroupOnPodRestart`. If `maxGroupRestarts` is set with a different restart policy, the webhook denies the request with a clear error message.
2. Restart reconcile path:
   for `RecreateGroupOnPodRestart`, controller checks group restart budget before leader deletion.
3. In `RecreateGroupOnPodRestart` path, before deleting leader pod:
   - read `group-restart-count` annotation;
   - if `count >= maxGroupRestarts`, stop recreation and mark replica failed;
   - otherwise increment annotation and continue current recreation flow.
4. The counter is group-level, not init-only: any failure path that enters
   `RecreateGroupOnPodRestart` contributes to the same retry budget.
5. Service reconcile sets:
   `svc.Spec.PublishNotReadyAddresses = spec.networkConfig.publishNotReadyAddresses`
   (defaulting to `true` when unset) on both create and update paths.
6. Under `UniquePerReplica`, the LWS reconciler eagerly creates a per-leader
   headless Service (named after the leader pod, selector `{lws-name, group-index}`)
   for every existing leader pod, so the leader FQDN used by `LWS_LEADER_ADDRESS`
   exists before init containers run instead of being created lazily by the pod
   reconciler. Under `Shared`, the single shared Service is already created
   eagerly and keeps its current timing.
7. Counter persistence is annotation-based so controller restarts do not reset state.

### Status Semantics

1. `maxGroupRestarts` unset: current behavior (unbounded recreation).
2. `maxGroupRestarts: 0`: first group-level failure becomes terminal.
3. `maxGroupRestarts` requires `restartPolicy: RecreateGroupOnPodRestart` (enforced by webhook).
4. Any replica exceeding limit sets LWS condition `Failed=True`
   with reason `MaxGroupRestartsExceeded`.
5. Failed replica is excluded from available-ready accounting.

### Operational Notes

1. Restart counting:
   one "group restart" means one leader deletion in `RecreateGroupOnPodRestart` path.
2. Counter persistence:
   - the `group-restart-count` annotation on the leader pod is the source of truth
     the LWS-level reconciler reads to surface `Failed`;
   - the `group-restart-counts` annotation on the LWS object survives leader pod
     recreation, so a recreated leader inherits the group's counter and neither
     pod recreation nor controller restarts reset the budget.
3. After terminal failure:
   no further group recreation is attempted for the failed replica; pod is retained for debugging.
4. DNS behavior:
   with `publishNotReadyAddresses: false`, peer FQDN may not be resolvable in init phase.
   The default (`true`, including unset/`nil`) publishes not-ready addresses, matching
   the historical LWS behavior.
5. `startupPolicy: LeaderReady` interaction:
   worker groups are still created only after leader is ready; this proposal does not alter that gate.

## Risks and Mitigations

1. Strict `maxGroupRestarts` may fail transient issues.
   Mitigation: opt-in, user-controlled threshold.
2. Annotation increment and delete are not fully atomic.
   Mitigation: acceptable best-effort bound for fail-fast policy.
3. `publishNotReadyAddresses=true` exposes DNS records before readiness.
   Mitigation: this is the historical default behavior, so no change for existing
   users; users who want to disable it can set the field to `false` explicitly.

## Drawbacks

1. A strict restart budget can convert transient failures into terminal failure.
2. `publishNotReadyAddresses` publishes not-ready pod DNS records by default (as
   before); disabling it requires an explicit opt-out.
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
   - service construction propagates `publishNotReadyAddresses` (including the
     unset/`nil` -> `true` legacy default).
2. Integration:
   - bounded recreate behavior with `maxGroupRestarts`;
   - no regression when `maxGroupRestarts` is unset;
   - DNS publish behavior with `publishNotReadyAddresses` set and unset;
   - eager per-leader headless Service creation for both subdomain policies.
3. e2e:
   - init-container peer check succeeds with publish enabled;
   - `publishNotReadyAddresses` update propagation on Shared and UniquePerReplica Services;
   - repeated init failure reaches `Failed` after configured limit.

## Implementation History

- 2026-04-16: Initial draft.
- 2026-04-16: Simplified structure and clarified universal scope beyond preflight-only use.
- 2026-07: Implementation review feedback: `publishNotReadyAddresses` defaults to
  `true` (unset/`nil`) to preserve the historical hardcoded behavior instead of
  flipping existing Services to `false` on upgrade; LWS reconciler creates
  per-leader headless Services eagerly to close the init-phase DNS timing race.
