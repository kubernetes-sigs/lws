# Combined rollout: local executable investigation

Status: **bounded production implementation connected to `Reconcile`**. The
branch's ordering enum is removed. Scoped native AKS validation is recorded
below; this does not imply maintainer approval. This implements a narrower local #717 contract, not
the separate #1018 proposal or universal neutral-repair liveness.

## Implemented production scope

The experiment helper was promoted into
[combined_rollout.go](../pkg/controllers/combined_rollout.go), used by
[combined_rollout_controller.go](../pkg/controllers/combined_rollout_controller.go).
The retained tests exercise that same production planner. Its rules are:

- Persist initial replica baseline `B`, latest desired `D`, active generation,
  template revision, and outstanding ordinal/UID reservations in one JSON value.
  It does not derive the baseline from Ready count or the overwritten replicas
  annotation. Supersession retains `B`; explicit downscale uses `min(B,D)` for
  the floor.
- Resolve `U` by rounding down against `D`, and `S` by rounding up against `D`.
  Preserve the nonzero-desired rejection of jointly zero resolved budgets.
- Count complete Ready groups across revisions. Exclude terminating leaders,
  terminating workers, mismatched controller UIDs/revisions, missing workers,
  stale worker status and stale worker generations. Use each leader's stamped
  size, not the latest LWS size, when evaluating an old group.
- Exclude an authorized old group's credit immediately, even if a repeated
  observation still sees its old leader Ready. Release its reservation only
  when a different leader UID and its entire replacement group are Ready at the
  reserved LWS and native revisions, or after an acknowledged freeze withdraws
  an obsolete rollback/partition authorization. Retry deletion using the observed leader identity.
- Permit paid replacements only above `max(0,min(B,D)-U)`. Unavailable old
  groups can be repaired without spending another Ready group, provided the
  partition suffix can be authorized.
- Reserve every stale leader exposed by the partition, including those the
  native controller could delete independently. Skip already-updated Pending
  ordinals when selecting a lower explicit deletion.
- Freeze before introducing or superseding a template, then wait for native
  observedGeneration before publication. Wait again for observedGeneration and
  updateRevision before planning. The planner's `templateFrozen` test input is
  not itself an acknowledgement; production uses persisted phase barriers.
- Create desired additions without automatically adding surge on top of a
  scale-up. Keep operation state until desired partition-aware convergence and
  observed surge cleanup. Ordinary rollout/scaling without active state stays
  on the legacy path. Default U=1 can replace old groups before Pending additions
  complete, changing the previous default combined scale-first behavior.
- Bound reservations to 64 entries and state to 24 KiB. A full window blocks
  additional suffix exposure until replacements recover. The floor bounds
  controller-authorized disruption from observed health, not independent failures
  or strict update ordering. Baseline is the pre-operation non-surge target.

This is inspired by DaemonSet's distinction between unavailable old Pods that
can be repaired and available old Pods whose deletion consumes budget. Unlike
DaemonSet, an ordinal StatefulSet represents update eligibility with a suffix,
not an arbitrary set of independent targets.

## What the tests establish

[combined_rollout_planner_test.go](../pkg/controllers/combined_rollout_planner_test.go)
checks the 1-to-2 Pending-addition case, zero-budget waiting and Ready-addition
credit, 4-to-8 concurrency, percentage rounding, repeated observations/restarts,
initial unhealthy repair, whole-group readiness, supersession, downscale/zero,
partition protection, malformed state, and all 256 readiness masks under each
of four budgets for a 4-to-8 transition.

The `B=2,D=3,U=1 -> 0 -> 1` case selects old ordinal 0 despite updated Pending
ordinal 1 and Ready ordinal 2. A characterization of the native gate-disabled
descending loop stops at ordinal 1. Thus explicit deletion, rather than merely
lowering the partition, makes a real difference in the planner.

[combined_rollout_api_test.go](../pkg/controllers/combined_rollout_api_test.go)
uses an existing local envtest API server to establish that:

1. SSA with `metadata.resourceVersion` atomically writes reservation and
   partition, and rejects a stale second plan even with force ownership.
2. Pod UID/resourceVersion preconditions reject stale deletion requests.
3. An unconditioned by-name request can delete a replacement Pod. This is an
   API fact, **not a reproduced StatefulSet race**: the native controller also
   serializes per-set syncs and recreates Pods itself.
4. `OnDelete` cannot retain `rollingUpdate.partition`: the API rejects that
   combination, accepting the strategy change only after removing that field.

Envtest here has no StatefulSet controller, scheduler, kubelet, or garbage
collector. Pod replacement is simulated explicitly. The native-loop and
creation-selection characterizations are not imported Kubernetes controller
tests and must not be reported as older-cluster E2E validation.

## Remaining neutral-repair limitation

There is a distinct obstruction beyond the updated-Pending-middle case:

| Ordinal | Revision | Whole group | Native leader readiness |
| --- | --- | --- | --- |
| 0 | old | Ready | Ready |
| 1 | old | unavailable | not Ready |
| 2 | old | Ready | Ready |
| 3 | new | workers unavailable | Ready |

For `B=3,D=4,U=1`, the floor is two and exactly two groups supply credit.
Repairing group 1 at the new template would be availability-neutral. However:

- Partition must be at most 1 for native recreation of ordinal 1 at the new
  template.
- That also exposes Ready old ordinal 2. The gate-disabled native controller
  passes Ready leader 3 and deletes ordinal 2, leaving only one Ready group.
  It does not know that group 3's workers are unavailable.
- Keeping partition 3 allows direct repair of ordinal 1 only at the **current,
  old template**. That can recover a transient failure, but cannot guarantee
  progress when the old template itself must change.
- Reserving ordinal 2 cannot invent the missing Ready credit. Requiring it to
  become affordable preserves safety but can leave neutral repair blocked.

`TestCombinedPlannerNeutralRepairBehindReadyOldSuffix` characterizes this
limitation. The test **passes by asserting conservative blocking**, not by
asserting the requested general neutral-repair liveness has been achieved.

The production suffix planner explicitly adopts this narrower local contract
and emits `CombinedRolloutBudgetBlocked`. It does not claim arbitrary
DaemonSet-style neutral repair. DaemonSet's per-node pairs differ from LWS group
identities; LWS retains mixed positive budgets and its existing rounding rules.

## Alternative examined: OnDelete with protected-revision admission

Plain OnDelete removes native rolling deletions and permits independent
budget-authorized leader deletion. It does not preserve old-version recreation
below the user's partition: native creation uses the updated template because
the native partition field is absent. This is covered by the API test and the
creation-selection characterization.

An admission-assisted alternative could restore the protected revision during
leader Pod **creation**, before existing Pod defaulting. This avoids racing an
LWS-created Pod against native creation, but it is a substantially larger
controller/admission protocol:

1. Persist the protected revision selection and retain its ControllerRevision
   before publishing the OnDelete strategy. A single latest-revision label is
   insufficient across supersession and partition changes.
2. Give the currently stateless `PodWebhook` an authenticated, uncached reader.
   Validate actual leader StatefulSet owner UID and ordinal; do not trust only
   caller-supplied LWS labels. Fail closed when required state/history is absent.
3. Restore the complete revision-specific leader template, not just its image
   or revision label. Preserve native Pod identity, ordinal PVC naming/bindings,
   hostname/subdomain, owner reference and retention behavior. Then perform
   existing LWS defaults, topology/TPU/PodGroup injection and environment setup
   using the restored size and metadata. The current webhook only decorates the
   template it receives; it cannot currently do this restoration.
4. Reconcile creation admission with state-generation changes. A request must
   not select a newer protected template from a stale state snapshot. Strategy
   entry/exit and restoration of RollingUpdate need explicit handoff barriers.
5. Keep protected history while it can be used by admission. In native OnDelete,
   `completeRollingUpdate` does not promote `currentRevision`, so that field
   alone is not an authoritative protected snapshot for later operations.
6. Integrate worker recreation, revision-specific size, foreground leader/worker
   cleanup, and stale UID handling. In the existing Pod controller, the early
   size-one check uses the live LWS size, before revision restoration; that also
   needs review for protected old groups with a different size.

This could remove the suffix limitation, but it is not a safe local substitution
of `OnDelete` for `RollingUpdate`. It needs creation-admission and native-controller
tests, history lifecycle changes, and a specified protected-revision policy. No
such admission-assisted production changes are included or required for this
bounded implementation.

## Implemented native handoff and remaining validation boundary

Production checks live LWS UID/generation and StatefulSet UID/resourceVersion
through the manager APIReader. Old-template freeze and new-template publication
are separate acknowledged phases. Operational state/partition writes use
optimistic-lock merge patches; template publication uses the complete intended
declarative configuration with resourceVersion-fenced SSA. Neither operation
claims unrelated observed fields. A later observation performs authorized direct
deletion with Pod UID/RV preconditions, normal grace, and foreground worker GC.
Every exposed stale leader is reserved, not merely the explicit deletion target.
No temporary OnDelete or new native feature gate is required.

Real Reconcile/envtest tests exercise phase persistence, acknowledgements, U=0
whole-Ready credit, 1->2 and 4->8 Pending additions, restart/payment retention,
stale LWS generations, supersession and malformed state. Native status and Pods
are explicitly simulated: these tests do not reproduce native controller races.
Separate native capacity-constrained AKS smoke tests are recorded below. They
do not establish older-cluster or gate-disabled compatibility.

The API enum, webhook restrictions, wrappers, generated client configuration,
both CRD schemas/Helm copies and API reference are removed/regenerated. Existing
unrelated line-ending changes are not reverted.

## Final local validation - 2026-09-09

- Full `make test`: **602 tests passed**.
- Focused controller/webhook/revision/StatefulSet utility run: **168 test/subtest passes**.
- Targeted rolling-update, scale, and surge integration: **23 passed, 0 failed**;
  30 other specs were outside the selection. This is not a full integration-suite pass.
- Go lint: **0 issues**.
- `git diff --check`: **passed** after restoring LF endings in the two generated
  API-reference pages. The DisaggregatedSet reference has no remaining diff.
- Scoped native AKS 1.35.7 smoke validation: **passed**, as detailed below.
  Earlier AKS validation of the ordering prototype is separate evidence and is
  not counted as validation of this new controller path.

Final regressions cover native-vs-LWS revision identity, worker native hashes,
downscale before reserved deletion, rollback and partition changes across
restarts, native revision promotion before cleanup, independent SSA metadata
ownership, and revision reuse while waiting for freeze acknowledgement.
Production now pins both native and LWS target revisions and uses optimistic
operational patches rather than applying the entire observed StatefulSet.
Earlier failed/timed-out runs below describe the investigation history, not
the final results.

## Native AKS validation - 2026-09-09

The budget-driven implementation was built from the uncommitted working tree
and deployed to a private AKS 1.35.7 cluster. Both controller replicas ran image
digest `sha256:0178dc26f47c80eabbfd6d2fa42ebadbecdad6e4b922144216ba4abcb43b2702`.
The 427-file source inventory was verified unchanged during validation; this
subsequent documentation update does not change the tested controller code.

Native scheduler, StatefulSet controller, kubelet and garbage collector were
used, without simulated status, manual worker deletion or finalizer removal.
Each fixture had size-two groups pinned through a node selector. Old Pods
requested 300m CPU each and new Pods 175m each. A test-only filler left 750m
available: the old 600m group and final 700m workload fit individually, but an
old group plus one new Pod requires 775m and cannot fit.

- **U1/S0, replicas 1 to 2:** a pre-mutation Pod watch captured the additional
  leader Pending with `Insufficient cpu` before the original leader's
  termination update. Native worker GC followed. The paid reservation remained
  while the replacement leader was Ready but its worker was deliberately unready.
- **U0/S1, replicas 1 to 2:** both original Pod UIDs remained Ready and unchanged
  during bounded 15-second observations of Pending additions and, after releasing
  the test filler, an additional Ready leader with an unready worker. Making the
  additional worker Ready supplied whole-group credit and enabled replacement.
- **Both finals:** four target-revision Ready Pods with correct ownership and
  native worker revisions; native leader revision promotion, observed generation,
  combined-state removal and retained RollingUpdate were verified. Both scenario
  namespaces and their fillers were cleaned up.
- **Admission:** server dry-run accepted U1/S0 and U0/S1, rejected jointly zero
  budgets, and rejected removed `updateOrder` under strict validation.

The completed smoke run passed 73 as-run assertions and 34 independent evidence
checks. Raw Pod, StatefulSet, LWS and Event watches were established before
mutation; timestamps, resource versions, UIDs and final snapshots were retained
in local validation artifacts. An earlier attempt with broken watch setup is
not counted as a passing transition test.

**Extended restart/partition validation remains incomplete.** A B2/D3/U1/S0
restart attempt successfully replaced both controller Pods, but the test harness
asserted the new leader-election lease holder before lease handoff completed.
Subsequent read-only observations verified new leadership, retained baseline and
reservation, and protected old-group UIDs. These checks do not constitute a
complete scenario: final convergence was not executed and the partition scenario
was not run. Harness failures are not established controller defects. Full E2E,
stock-controller A/B, and older gate-disabled native validation are not claimed.

## Previous experiment validation (2026-09-09, not production validation)

- Existing Ubuntu-22.04 Go 1.26.0; existing envtest assets Kubernetes 1.36.0.
- Focused `TestCombined` run: PASS, including the real API precondition tests
  and the two limitation characterizations; package time 11.592 seconds.
- Existing controller, webhook, revision utility, and StatefulSet utility unit
  packages: PASS in the regression run.
- Full controller/webhook integration attempt with a four-minute Go timeout:
  webhook package PASS (35.592 seconds); controller package did not complete
  before the timeout (240.194 seconds). The captured stack was in the existing
  `ExpectWorkerStatefulSetsNotCreated` assertion. This is **not a full integration
  pass**, and no root cause or regression attribution is asserted.
- The initial dependency download failed because WSL could not resolve the Go
  proxy. The missing YAML module was downloaded through normal Windows HTTPS
  into the existing Go download cache; Go then verified/used it successfully.
  No checksum/TLS bypass, dependency-file change, or network configuration change
  was made.

## Pinned reference code

- [DaemonSet rollout selection, Kubernetes v1.35.0](https://github.com/kubernetes/kubernetes/blob/v1.35.0/pkg/controller/daemon/update.go)
- [DaemonSet surge KEP 1591](https://github.com/kubernetes/enhancements/tree/master/keps/sig-apps/1591-daemonset-surge)
- [StatefulSet update loop and generation status](https://github.com/kubernetes/kubernetes/blob/v1.35.0/pkg/controller/statefulset/stateful_set_control.go)
- [StatefulSet per-key serialized workqueue](https://github.com/kubernetes/kubernetes/blob/v1.35.0/pkg/controller/statefulset/stateful_set.go)
- [Pod deletion request options](https://github.com/kubernetes/kubernetes/blob/v1.35.0/pkg/controller/statefulset/stateful_pod_control.go)
- [Versioned Pod creation and revision promotion](https://github.com/kubernetes/kubernetes/blob/v1.35.0/pkg/controller/statefulset/stateful_set_utils.go)