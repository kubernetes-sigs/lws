# KEP-849: DisaggregatedSet HPA support per role

<!--
This KEP proposes adding per-role HorizontalPodAutoscaler (HPA) support to
DisaggregatedSet through a new DisaggregatedSetRoleScaler CRD that exposes the
/scale subresource. The DisaggregatedSet controller auto-creates one scaler
per role that sets scaling.mode: External; users don't author the scaler.
KEDA, HPA, or any /scale-aware controller then drives replica counts for
individual roles (e.g., "prefill" or "decode") independently.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: KEDA scales prefill on queue depth](#story-1-keda-scales-prefill-on-queue-depth)
    - [Story 2: Rolling update while HPA is active](#story-2-rolling-update-while-hpa-is-active)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [DisaggregatedSetRoleScaler API](#disaggregatedsetrolescaler-api)
  - [Changes to DisaggregatedRoleSpec](#changes-to-disaggregatedrolespec)
  - [Replica Resolution: DS reads replicas from HPA](#replica-resolution-ds-reads-replicas-from-hpa)
  - [Controller Wiring](#controller-wiring)
  - [Scale Subresource Status Fields](#scale-subresource-status-fields)
  - [Rolling Update Interaction](#rolling-update-interaction)
  - [Interaction with DisaggregatedSet Slices](#interaction-with-disaggregatedset-slices)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Point HPA directly at the underlying LeaderWorkerSet](#alternative-1-point-hpa-directly-at-the-underlying-leaderworkerset)
  - [Alternative 2: Expose /scale directly on DisaggregatedSet](#alternative-2-expose-scale-directly-on-disaggregatedset)
  - [Alternative 3: Embed scaling config inline in DisaggregatedRoleSpec](#alternative-3-embed-scaling-config-inline-in-disaggregatedrolespec)
  - [Alternative 4: Require the user to author the scaler CR](#alternative-4-require-the-user-to-author-the-scaler-cr)
<!-- /toc -->

## Summary

This KEP proposes adding per-role autoscaling to `DisaggregatedSet` by introducing a new lightweight CRD, `DisaggregatedSetRoleScaler`, that exposes the `/scale` subresource. When a user opts a role into external scaling via `scaling.mode: External`, the `DisaggregatedSet` controller **automatically creates and manages** a `DisaggregatedSetRoleScaler` for that role. The scaler is a per-role `/scale` target that an HPA, KEDA `ScaledObject`, or any custom `/scale`-aware controller can drive; the DisaggregatedSet controller reads its `spec.replicas` on every reconcile and drives the underlying LeaderWorkerSet.

The feature is fully opt-in and per-role: roles without `scaling.mode: External` behave exactly as they do today.

Users author two objects: the `DisaggregatedSet` and their HPA/KEDA target. The scaler CR itself is controller-managed — created and cleaned up by the DisaggregatedSet controller, in the same way the DisaggregatedSet already creates a `LeaderWorkerSet` per role.

## Motivation

`DisaggregatedSet` orchestrates multiple LeaderWorkerSets (LWS) for disaggregated inference workloads. Under a rolling update, the controller creates a new LWS per role with a revision-hashed name (e.g., `myds-0-6ad7c921-prefill` — `<ds>-<slice>-<revision>-<role>` per [KEP-846](/keps/846-disaggregatedset-slices)) and progressively drains the old-revision LWS.

Autoscaling a role today is only possible by pointing an HPA at the underlying LWS (see the [LWS HPA example](https://lws.sigs.k8s.io/docs/examples/hpa/)). This breaks for `DisaggregatedSet` because:

1. **LWS names change on every rollout.** The revision hash is part of the name, so an HPA created for `myds-0-6ad7c921-prefill` becomes an orphan the moment a rolling update produces `myds-0-7bf3d1a2-prefill`. Users would have to recreate the HPA on every deploy.
2. **DisaggregatedSet has no stable per-role scale target.** The parent CR aggregates multiple roles, so a single `/scale` subresource on `DisaggregatedSet` cannot express "scale prefill from 5 to 8 without touching decode".
3. **Disaggregated workloads scale per role.** Prefill and decode have different bottlenecks (compute-bound vs memory-bandwidth-bound), so operators need to autoscale each role independently. The DisaggregatedSet slices field ([KEP-846](/keps/846-disaggregatedset-slices)) replicates the whole role topology as a unit — it solves a different problem (adding entire copies of the deployment) and does not give per-role control within a single copy.
4. **DisaggregatedSet already owns per-role replica counts.** The controller reads `spec.roles[i].spec.replicas` and drives each LeaderWorkerSet's replica count on every reconcile. Adding an autoscaling entry point at the DisaggregatedSet layer keeps that responsibility in one place; delegating it out (e.g. asking users to bypass DisaggregatedSet and drive LWSes directly) would fragment ownership of the replica field between two controllers.

The upstream issue [#849](https://github.com/kubernetes-sigs/lws/issues/849) captures this.

### Goals

1. **Per-role autoscaling.** Allow HPA, KEDA, or any `/scale`-aware controller to drive replicas for a single role of a `DisaggregatedSet` without touching other roles.
2. **Stable scale target across rollouts.** The scaler CR name is stable and independent of the revision hash, so an HPA/KEDA object created once continues to work across arbitrary numbers of rolling updates.
3. **Opt-in, per-role, backward compatible.** DisaggregatedSets without any scaler behave identically to today. A DS can mix roles: some scaler-controlled, some inline `replicas`.
4. **Well-defined rolling-update behavior.** The interaction between an active autoscaler and an in-progress rolling update is specified, not left implicit.

### Non-Goals

1. **A built-in autoscaler.** This KEP does not add any autoscaling logic to LWS itself. The scaler CR is a delegation target only — replicas are set by an external controller (HPA/KEDA/custom) via `/scale`.
2. **Cross-role coordination of autoscaling decisions.** The scaler for `prefill` and the scaler for `decode` are independent. Coordinated scaling (e.g., "always keep decode at 2x prefill") is out of scope and can be built externally.
3. **Autoscaling metrics or recommendations.** How the HPA computes its target replica count (which metrics, which thresholds) is entirely the user's responsibility.
4. **VPA integration.** Vertical scaling (changing container resources) is out of scope; only replica count is delegated.
5. **Multi-slice DisaggregatedSets (alpha).** Interaction with [KEP-846](/keps/846-disaggregatedset-slices) `spec.slices > 1` is deferred to a follow-up KEP. Alpha only supports single-slice DisaggregatedSets. See [Interaction with DisaggregatedSet Slices](#interaction-with-disaggregatedset-slices).

## Proposal

We introduce a new CRD, `DisaggregatedSetRoleScaler`, in the `disaggregatedset.x-k8s.io/v1` API group. It exposes the `/scale` subresource so any autoscaler can drive it.

Instances of `DisaggregatedSetRoleScaler` are **created by the DisaggregatedSet controller**, one per role that has `scaling.mode: External`. Their names are deterministic — `<disaggregatedset>-<role>` — so users know in advance what to point their HPA/KEDA object at. The scaler is owned by the DisaggregatedSet, so garbage collection follows the standard Kubernetes cascade (see [Design Details](#disaggregatedsetrolescaler-api) for the exact ownerRef fields).

To opt into external scaling, the user:

1. Sets `scaling.mode: External` on the role inside the `DisaggregatedSet` spec.
2. Creates an HPA (or KEDA `ScaledObject`) whose `scaleTargetRef` names the deterministic scaler (`<ds>-<role>`).

That's it. The scaler CR appears on the next DS reconcile — the user does not author it. The controller seeds `spec.replicas` at creation time so vanilla HPA can attach: a fresh role is seeded at `1` (HPA parks in `ScalingDisabled` when it reads `current=0` from `/scale`, so seeding at 0 would deadlock the bootstrap), and a role transitioning from `Static` to `External` is seeded at its current LWS replica count so the running fleet is not drained. Autoscalers that support scale-from-zero (KEDA, or HPA with the `HPAScaleToZero` feature gate) can still take the role down to 0 after attach.

The DisaggregatedSet controller reads the desired replica count for each External role from its scaler's `spec.replicas` on every reconcile and writes back `scaler.status.replicas` and `scaler.status.selector` so the HPA loop closes.

### User Stories

#### Story 1: KEDA scales prefill on queue depth

An SRE runs a disaggregated vLLM deployment. Prefill is bottlenecked on request queue depth exposed via a Prometheus metric; decode is bottlenecked on GPU memory bandwidth. They want KEDA to scale prefill from 2 to 20 based on queue depth, and keep decode at a static 4.

They apply two objects. The `DisaggregatedSet` marks `prefill` as `External`; the DS controller creates a scaler named `my-llm-serving-prefill` on the next reconcile. KEDA's `ScaledObject` targets that (deterministic) scaler name and provides `minReplicaCount: 2`, which KEDA writes on its first tick to bootstrap the role from zero.

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: my-llm-serving
spec:
  roles:
    - name: prefill
      scaling:
        mode: External           # controller auto-creates my-llm-serving-prefill
      spec:
        leaderWorkerTemplate: {...}
    - name: decode
      spec:
        replicas: 4              # static
        leaderWorkerTemplate: {...}
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: my-llm-prefill
spec:
  scaleTargetRef:
    apiVersion: disaggregatedset.x-k8s.io/v1
    kind: DisaggregatedSetRoleScaler
    name: my-llm-serving-prefill   # controller-generated name: "<ds>-<role>"
  minReplicaCount: 2
  maxReplicaCount: 20
  triggers:
    - type: prometheus
      metadata:
        query: vllm_queue_depth{role="prefill"}
        threshold: "10"
```

The same layout works with vanilla HPA v2 — just swap the `ScaledObject` for a `HorizontalPodAutoscaler` targeting `my-llm-serving-prefill`. The controller seeds a fresh scaler at `spec.replicas: 1` (see the seeding note in the previous section), so HPA's first `/scale` read returns a non-zero current and HPA leaves `ScalingDisabled=False`.

#### Story 2: Rolling update while HPA is active

An SRE pushes a new container image. A rolling update starts. Meanwhile, request load spikes and the HPA wants to grow the new revision from 5 to 8. The desired behavior:

- The old-revision LWS keeps its snapshotted initial replica count and continues to drain on the existing schedule.
- The new revision's target is bumped from 5 to 8 in-flight; the planner picks up the new target on the next stability window and continues rolling.
- The HPA is never confused by intermediate revision transitions because the scaler's `status.selector` is rewritten to point at the current new-revision LWS.

### Risks and Mitigations

**Risk**: HPA and rolling update race conditions cause thrashing or stuck rollouts.

**Mitigation**: The scaler drives only the *new-revision target*. Old-revision replicas continue to use the existing snapshot-and-drain mechanism (unchanged). A guard in the planner prevents the new-revision target from shrinking below its current in-flight value mid-rollout, which would otherwise force a scale-down/scale-up flip.

**Risk**: Users configure inline `replicas` and set `scaling.mode: External` on the same role, causing confusion about which wins.

**Mitigation**: An explicit `scaling.mode` enum on the role makes intent visible in the DS spec. The webhook emits an admission warning when an External role sets `spec.replicas > 1` (values 0 and 1 are indistinguishable after CRD defaulting: `LeaderWorkerSetSpec.Replicas` carries `+kubebuilder:default=1` and defaulting runs before CEL, so a CEL rejection of `spec.replicas > 0` would fire on every External role). The all-or-nothing CEL rule on `DisaggregatedSetSpec` is scoped to non-External roles.

**Risk**: The autoscaler is not yet applied (or has never written) when the DS is applied, or a role is flipped from `Static` to `External` while serving traffic — in production this must not silently drain the role to 0.

**Mitigation**: The controller **never scales an existing LeaderWorkerSet down as a side effect of switching to `External`**. Concretely, when the controller creates the scaler for an External role it seeds `spec.replicas` based on the role's LWS state:
- If the role's LWS **does not exist yet** (fresh DisaggregatedSet, or newly added role), the scaler is seeded at `1` and the LWS is created at `1` replica. Seeding at `1` (rather than `0`) is what lets vanilla HPA attach — HPA parks in `ScalingDisabled` if it reads `current=0` from `/scale`, regardless of `minReplicas`, unless the `HPAScaleToZero` feature gate is enabled.
- If the role's LWS **already exists** (e.g. a role that was `Static` with 5 replicas is switched to `External`), the scaler is seeded at the current replica count and the controller holds the LWS there. The rolling-update planner behaves the same way: it clamps `targetNew` to the current in-flight value rather than shrinking.

Once the autoscaler attaches, HPA and KEDA both write `minReplicas` / `minReplicaCount` unconditionally when the target is below the floor, and the LWS scales to that value on the next reconcile. Autoscalers that support scale-from-zero (KEDA, or HPA with `HPAScaleToZero`) can take the role down to 0 after attach. Custom autoscalers that only observe per-pod metrics and lack a min-replicas floor must issue a one-time `kubectl scale` to bootstrap.

**Risk**: A user deletes the auto-created scaler expecting it to stay gone.

**Mitigation**: The controller recreates it on the next reconcile. To truly opt out of external scaling, the user flips the role's `scaling.mode` back to `Static` (or removes `scaling` entirely); the controller then deletes the scaler in the same pass.

**Risk**: A user hand-creates a `DisaggregatedSetRoleScaler` at the controller-managed name (`<ds>-<role>`), racing the controller.

**Mitigation**: the controller detects the missing or incorrect ownerRef, refuses to adopt or overwrite the object, and emits a warning event on the DisaggregatedSet naming the offending object.

## Design Details

### DisaggregatedSetRoleScaler API

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:shortName=dsrs

// DisaggregatedSetRoleScaler exposes the /scale subresource for a single
// role of a DisaggregatedSet. Instances are created by the DisaggregatedSet
// controller for every role that opts into external scaling via
// scaling.mode: External. Instance names follow the pattern
// "<disaggregatedset>-<role>", and each instance carries a controller
// owner reference back to the DisaggregatedSet.
//
// The DisaggregatedSet controller reads spec.replicas on every reconcile
// and drives the role's LeaderWorkerSet accordingly. External autoscalers
// (HPA, KEDA, or any /scale-aware controller) write spec.replicas via the
// /scale subresource.
type DisaggregatedSetRoleScaler struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitzero"`

    Spec   DisaggregatedSetRoleScalerSpec   `json:"spec"`
    Status DisaggregatedSetRoleScalerStatus `json:"status,omitzero"`
}

type DisaggregatedSetRoleScalerSpec struct {
    // Replicas is the desired replica count for the associated role.
    // Written by an external autoscaler via the /scale subresource.
    // Read by the DisaggregatedSet controller.
    //
    // The DisaggregatedSet + role this scaler drives is derived from the
    // scaler's controller ownerReference (kind: DisaggregatedSet) and the
    // scaler's disaggregatedset.x-k8s.io/role label. Users do not name the
    // target explicitly; the scaler's name — "<disaggregatedset>-<role>" —
    // is authoritative.
    // +optional
    // +kubebuilder:validation:Minimum=0
    Replicas *int32 `json:"replicas,omitempty"`
}

type DisaggregatedSetRoleScalerStatus struct {
    // Replicas is the observed pod count for this role, aggregated
    // across all revisions currently present (new + any draining old).
    // Read by the /scale subresource — HPA uses it as the "current"
    // replica count when computing desired replicas from metrics.
    // +optional
    Replicas int32 `json:"replicas,omitempty"`

    // Selector is a label selector (in string form) matching all pods
    // for this role, across all revisions. Format:
    //   disaggregatedset.x-k8s.io/name=<ds>,disaggregatedset.x-k8s.io/role=<role>
    // Used by HPA to compute per-pod metrics. Aggregate (revision-
    // agnostic) so HPA observes the actual serving fleet during a
    // rolling update rather than only the new-revision pods. Stable
    // across rollouts — no rewrite needed.
    // +optional
    Selector string `json:"selector,omitempty"`

    // ObservedGeneration is the .metadata.generation the status reflects.
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // Conditions expose scaler-level state. Standard types:
    //   - Ready: True when the scaler is bound to a live DS+role and its
    //     status fields reflect the current observed LeaderWorkerSet.
    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

**Naming**: instances are always named `<disaggregatedset>-<role>`. The DisaggregatedSet webhook rejects any create/update that would make this exceed the Kubernetes 253-character object-name limit for a role with `scaling.mode: External`; the user must shorten the DisaggregatedSet name or the role name. Since role names are already capped at 63 characters, this leaves 189 characters for the DisaggregatedSet name — enough for any realistic naming scheme. Rejecting at admission keeps the derivation from `(ds, role)` a stable, deterministic transformation without any fallback shape.

**Owner reference**: the scaler carries `Controller=true, BlockOwnerDeletion=true` ownerRef to the DisaggregatedSet, so the scaler is garbage-collected via the standard Kubernetes ownership chain when the DisaggregatedSet is deleted.

**Labels applied by the controller**:
- `disaggregatedset.x-k8s.io/name`: the parent DisaggregatedSet name.
- `disaggregatedset.x-k8s.io/role`: the role name (also used by the controller to identify which role the scaler drives).

### Changes to DisaggregatedRoleSpec

The existing `DisaggregatedRoleSpec` gains one optional field:

```go
type DisaggregatedRoleSpec struct {
    Name string `json:"name"`

    // Scaling configures how replicas are determined for this role.
    // Omit for inline Static scaling (default; today's behavior).
    // +optional
    Scaling *RoleScaling `json:"scaling,omitempty"`

    leaderworkerset.LeaderWorkerSetTemplateSpec `json:",inline"`
}

type RoleScaling struct {
    // Mode controls the source of the replica count for this role.
    //   - Static (default): use the inline .spec.replicas value.
    //   - External: the DisaggregatedSet controller creates a
    //     DisaggregatedSetRoleScaler named "<disaggregatedset>-<role>"
    //     whose /scale subresource an external autoscaler drives.
    //     .spec.replicas on the role is ignored regardless of value.
    //     Values > 1 trigger an admission warning to nudge users who
    //     may believe the number takes effect (1 is the field default
    //     and indistinguishable from unset after CRD defaulting).
    // +kubebuilder:validation:Enum=Static;External
    // +kubebuilder:default=Static
    // +optional
    Mode RoleScalingMode `json:"mode,omitempty"`
}

type RoleScalingMode string

const (
    RoleScalingStatic   RoleScalingMode = "Static"
    RoleScalingExternal RoleScalingMode = "External"
)
```

The `Scaling` field is a sub-struct rather than a bare enum to leave room for future per-role scaling policies without a v2 API bump — following the Kubernetes convention that policy fields (like `Deployment.spec.strategy`, `StatefulSet.spec.updateStrategy`, `LeaderWorkerSet.spec.rolloutStrategy`) live in sub-structs even when they start with a single field.

Backward compatibility: an omitted `scaling` block stays nil in storage (the `+kubebuilder:default=Static` marker on `Mode` only defaults the field when the sub-struct is present, not by materializing the sub-struct itself). The controller treats `Scaling == nil` and `Scaling.Mode == Static` as behaviorally identical — both read from the inline `spec.replicas`. Existing DisaggregatedSet objects therefore round-trip through the new schema unchanged and behave exactly as they did before.

**Existing CEL rule change**: today's rule enforces that replicas is either zero for all roles or non-zero for all roles. It must become scaling-mode-aware — External roles are exempt from the all-or-nothing rule because their effective replicas live outside the DS spec:

```go
// +kubebuilder:validation:XValidation:rule="self.roles.filter(r, !has(r.scaling) || r.scaling.mode != 'External').all(r, !has(r.spec.replicas) || r.spec.replicas == 0) || self.roles.filter(r, !has(r.scaling) || r.scaling.mode != 'External').all(r, has(r.spec.replicas) && r.spec.replicas > 0)"
```

No per-role CEL rule is added to reject `spec.replicas > 0` on External roles. The `LeaderWorkerSetSpec.Replicas` field carries `+kubebuilder:default=1`, which is inherited through the inlined `LeaderWorkerSetTemplateSpec`; API-server defaulting runs before CEL, so a role with `scaling.mode: External` and no explicit replicas always has `spec.replicas == 1` at validation time. A rule that forbids `replicas > 0` would therefore reject every External role. Instead, the field is documented as ignored regardless of value, and the DisaggregatedSet webhook emits an admission warning for explicit values `> 1` to catch the user who genuinely believes the number matters.

### Replica Resolution: DS reads replicas from HPA

**This is the load-bearing change of this KEP.** The `DisaggregatedSet` controller no longer treats `.spec.roles[].spec.replicas` as the sole source of truth. For roles with `scaling.mode: External`, it delegates the replica count to the HPA (or KEDA / any `/scale` writer) via the auto-created scaler CR.

The resolution rule for a role's desired replica count is:

- **`scaling.mode == External`** → read `scaler.spec.replicas` from the DisaggregatedSetRoleScaler named `<ds>-<role>` (deterministic). If the scaler does not exist yet, the controller creates it in the same reconcile pass. If `spec.replicas` is unset (no autoscaler has written), treat the role as "not ready" and surface a `WaitingForScaler` condition on the DS.
- **`scaling.mode == Static` (or `scaling` unset)** → read `.spec.replicas` from the role, defaulting to `1` if unset. This is today's behavior, preserved unchanged.

At the top of each reconcile pass, the controller ensures a scaler exists for every External role (create-if-missing), then reads their current `spec.replicas` into a map keyed by role name. The same map feeds the rolling-update planner so that scaler-driven values are consistent across scale-up, scale-down, and stability checks within one pass.

### Controller Wiring

The DS controller `Owns` the `DisaggregatedSetRoleScaler` type — same pattern as it already owns `LeaderWorkerSet`. Any change to a scaler (including autoscaler writes to `spec.replicas` via `/scale`) enqueues the parent DisaggregatedSet for reconciliation. No cross-CRD dependency on `autoscaling/v2` is needed.

Write path end-to-end:

```
Autoscaler writes /scale → scaler.spec.replicas updates → Owns() event fires → DS reconciles → LWS scaled
```

Reconcile latency for a scaler-triggered event is a single controller hop.

### Scale Subresource Status Fields

The DS controller writes back to each scaler's status at the end of every reconcile:

- `status.replicas`: the observed replica count for this role — LWS groups (== leader pods), aggregated across all revisions currently present (new + any draining old). HPA reads this as the "current" count. Same unit as `spec.replicas` (what HPA writes), so its ratio math stays consistent.
- `status.selector`: a stable selector matching one pod per LWS group (the leader), across all revisions — `disaggregatedset.x-k8s.io/name=<ds>,disaggregatedset.x-k8s.io/role=<role>,leaderworkerset.sigs.k8s.io/worker-index=0`. Leader-only because HPA's per-pod-metric averaging divides the metric sum by the count of matching pods, and that divisor has to equal `status.replicas` (group count) for the ratio math to work when `leaderWorkerTemplate.size > 1`. Users typically want to scale on the leader's signal anyway (leader handles ingress, workers are downstream compute). Because the label set on leader pods stays the same across revisions, this value is written once at scaler creation and never rewritten during a rollout.
- `status.observedGeneration`: `scaler.Generation`.
- `status.conditions`: `Ready` — True when the scaler is bound to a live DS + role and `status.replicas` reflects the observed leader count. False during transient reconcile errors.

### Rolling Update Interaction

`scaler.spec.replicas` is the target for the role's post-rollout steady state. The DS controller feeds it as the new-revision LeaderWorkerSet's target; old revisions continue to drain on the schedule the planner set at rollout start (the pre-existing `initial-replicas` annotation mechanism), independent of the scaler.

Because `status.selector` is leader-only and aggregate across revisions, HPA sees the serving fleet's leaders during a rolling update and its math stays self-consistent — the count HPA divides its metric by (`status.replicas`, LWS groups) matches the number of pods its selector matches (one leader per group), and the value it writes (`spec.replicas`, LWS groups) becomes the new-revision target as the old revision drains to zero.

One safety guard: between planner iterations, the new-revision target is never allowed to fall below the current new-revision replica count. If an HPA scale-down arrives mid-rollout, its requested value is floored to what's already in flight — the new-revision fleet stops growing but does not shrink. Once the rollout completes, the guard releases and the target tracks the scaler exactly.

**Future work:** the current design keeps HPA writes and the old-revision drain schedule fully independent — safe, but not the tightest possible feedback loop. A follow-up (implementation PR or KEP) could couple them:

- An HPA scale-down mid-rollout could accelerate the old-revision drain, converging on the smaller target faster.
- An HPA scale-up in response to a sudden load spike could accelerate new-revision growth (raising `maxSurge` for that step, for instance) to absorb the spike sooner.

Both are potentially destabilising if not carefully bounded — they interact with the planner's surge/unavailable budgets and could produce oscillation. They're deliberately out of scope for the initial implementation; alpha ships with the simple "old drains independently" behavior above.

### Interaction with DisaggregatedSet Slices

[KEP-846](/keps/846-disaggregatedset-slices) adds `spec.slices: int` to DisaggregatedSet, replicating the whole role topology into N independent copies. Each slice rolls independently, and `spec.roles[].spec.replicas` becomes a *per-slice* count (total pods per role = `replicas × slices`). LWS names gain a slice segment (`<ds>-<slice>-<revision>-<role>`).

This has three unresolved implications for scaler-driven roles:

1. **Scope of `scaler.spec.replicas`.** Three possible shapes:
   - **Aggregate** — value is total across slices; controller divides among slices. Matches HPA's usual mental model.
   - **Per-slice scaler CR** — one scaler per (DS, role, slice). Explicit; combinatorial (`roles × slices` scalers per DS).
   - **Per-role, applied per-slice** — value is *per slice*, consistent with `spec.roles[].spec.replicas`. UX footgun: HPA sets N, gets N × slices pods.
2. **`status.selector` across slices.** The leader-only selector used in the single-slice design (`disaggregatedset.x-k8s.io/name=<ds>,disaggregatedset.x-k8s.io/role=<role>,leaderworkerset.sigs.k8s.io/worker-index=0`) would match leaders across all slices too. That's the right shape if the scaler covers all slices (aggregate scope), but wrong if per-slice scalers are chosen — those would need to add a `disaggregatedset.x-k8s.io/slice=<slice>` filter.
3. **Concurrent per-slice rollouts.** Each slice runs its own rolling update on its own clock (that's the point of the slices feature), so a single role can be mid-rollout in one slice and steady-state in another simultaneously. The current no-shrink guard (which prevents the new-revision target from falling below the current in-flight count) tracks state per role; with slices it would have to track state per `(slice, role)` pair, or an HPA scale-up seen against one slice's in-flight count could incorrectly clamp another slice.

**Alpha scope: `spec.slices` must be 1 (default).** The DS webhook rejects a `spec.slices > 1` value while any role has `scaling.mode: External`. Since the scaler is auto-created only after this validation passes, no scaler ever exists in the multi-slice case. This lets alpha ship the single-slice case cleanly (which is the most common shape today) without prejudging the multi-slice design.

**Direction for slices > 1** (out of scope for alpha, to be revisited in a follow-up KEP after production feedback): still open. The three shapes above trade off along different axes and the right answer depends on how the placement model matures.

- **Aggregate scaler** looks the strongest for hardware-partitioned deployments (per KEP-846, slices typically map to a single accelerator domain like an NVL72 rack). One scaler and one HPA per role regardless of slice count means autoscaling config doesn't grow when a rack is added. HPA sees the whole fleet and makes a single decision; the controller distributes replicas across slices, which is well-defined when slices are hardware-uniform.
- **Per-slice scaler CR** is more Kubernetes-idiomatic in isolation (one CR = one `/scale` target, no distribution math) but forces `roles × slices` HPAs and scalers to be kept in sync, which for an 8-rack NVL72 deployment is 16 objects per role change — operationally painful.
- **Per-role applied per-slice** has the UX footgun called out above.

Alpha ships with `slices == 1`, so the choice does not need to be locked in yet.

### Test Plan

[X] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

#### Unit tests

- Replica resolution precedence between `Static` and `External` roles.
- Scaler webhook and CEL validation.
- Rolling-update planner behavior with a scaler-driven target.

#### e2e tests

- Opt-in flow: `External` role without a scaler holds; scaler creation triggers scale-up.
- `/scale` writes propagate to the underlying LeaderWorkerSet.
- Rolling update honors the scaler target and refreshes `status.selector` across revision transitions.
- Scaler and DisaggregatedSet lifecycle (deletion, garbage collection).
- End-to-end HPA loop driving replicas via a synthetic metric.
- Rolling update under active HPA load.

### Graduation Criteria

**Alpha (v0.X)**:
- `DisaggregatedSetRoleScaler` CRD with `/scale` subresource
- `scaling.mode` field on `DisaggregatedRoleSpec` (default `Static`, backward compatible)
- Controller wiring: watch + replica resolution + status writeback
- Validation: webhook + CEL, including the `spec.slices == 1` restriction for External roles
- Rolling-update no-shrink guard for the new-revision target
- Test coverage per plan above
- Documentation and example manifests for HPA and KEDA
- **Scope**: single-slice DisaggregatedSets only (`spec.slices == 1`). Multi-slice support is deferred to a follow-up KEP.

**Beta**:
- Production feedback incorporated
- Metrics: count of `WaitingForScaler` conditions
- Multi-slice support tracked in a follow-up KEP; the shape (aggregate vs per-slice scaler) is left open pending production feedback on the placement model — see [Interaction with DisaggregatedSet Slices](#interaction-with-disaggregatedset-slices).
- Coupling of HPA writes to the old-revision drain schedule (accelerate drain on scale-down; accelerate new-revision growth on a sudden scale-up spike) — see the "Future work" note in [Rolling Update Interaction](#rolling-update-interaction). Requires careful bounding against the planner's surge/unavailable budgets to avoid oscillation.

**Stable**:
- Proven stability across a range of autoscalers (HPA v2, KEDA, custom)
- No open bugs on the scaler pathway

## Implementation History

- 2026-07-03: Initial KEP draft (user-authored scaler CR shape).
- 2026-07-04: Documented interaction with KEP-846 slices; scoped alpha to `spec.slices == 1`.
- 2026-07-08: Redesigned around auto-created scalers.

## Drawbacks

1. **A second CRD kind for cluster admins to install.** Cluster admins now install `DisaggregatedSetRoleScaler` alongside `DisaggregatedSet` and `LeaderWorkerSet`. Users don't author instances directly, but the CRD schema still ships as part of the bundle.
2. **`kubectl get ds` no longer shows the effective replica count for External roles.** Users must `kubectl get dsrs` (short name for the scaler CRD) to see the desired count. The explicit `scaling.mode: External` marker on the role signals in the DisaggregatedSet spec that replicas are managed externally.
3. **Aggregate-metric averaging across revisions.** During a rolling update, HPA's per-pod metric averages over pods of both revisions (see Rolling Update Interaction). If the two revisions have materially different resource characteristics, HPA's decisions during the rollout window are informed by a mixed baseline. Fine for consecutive vLLM-style revisions; potentially inaccurate for radical resource changes until the rollout completes.
4. **Cold-start blind spot for exotic autoscalers.** Autoscalers that need per-pod metrics AND have no min-replicas floor cannot bootstrap the role from 0. Nearly every production autoscaler (HPA, KEDA, custom controllers that read from an external metrics store) either has a min-replicas floor or observes external (non-per-pod) metrics, so this is a narrow case.

## Alternatives

### Alternative 1: Point HPA directly at the underlying LeaderWorkerSet

The existing LWS documentation supports `scaleTargetRef` → LeaderWorkerSet. In principle a user could target one of the LWS objects a DisaggregatedSet manages.

**Rejected because**: LWS names include a revision hash (`<ds>-<slice>-<revision>-<role>` per KEP-846), so any HPA created against a specific LWS name becomes orphaned the moment a rolling update produces a new revision. The user would have to recreate the HPA on every deploy, which defeats the point.

### Alternative 2: Expose /scale directly on DisaggregatedSet

Add the `/scale` subresource to `DisaggregatedSet` itself. With [KEP-846](/keps/846-disaggregatedset-slices) adding `spec.slices` — a single integer that replicates the whole role topology — a `/scale` on the DS whose `specpath` maps to `spec.slices` is mechanically valid: HPA writes N, the controller creates N slice copies, done.

**Rejected because**: that's **slice-count autoscaling** (add whole racks/copies when overall load grows), not the **per-role autoscaling** KEP-849 addresses (grow prefill independently of decode when prefill hits a bottleneck). They're complementary features, not alternatives, and per-role scaling is what disaggregated inference deployments actually need in practice — prefill and decode have different bottlenecks (compute-bound vs memory-bandwidth-bound), so a single slice-count knob can't respond to a load change that only affects one role. A slice-count `/scale` on the DS could ship in a future KEP alongside per-role autoscaling; it does not replace it.

### Alternative 3: Embed scaling config inline in DisaggregatedRoleSpec

Skip the new CRD; instead embed autoscaling target/current fields on the role itself, and expose a virtual scale subresource on the DisaggregatedSet parameterized by role (e.g., `/scale?role=prefill`).

**Rejected because**: virtual scale subresources parameterized by query strings are not supported by the Kubernetes API machinery. HPA/KEDA cannot target them. The only path to standard-shape `/scale` is a dedicated CRD.

### Alternative 4: Require the user to author the scaler CR

Same CRD, but with an explicit `spec.targetRef {name, role}` field the user fills in. The DS controller reads whatever scaler the user created and attaches a non-controller ownerRef for GC.

**Rejected because**: it makes users author `2N+1` manifests for `N` scalable roles instead of `N+1`, and forces the controller to use a non-standard ownerRef (`Controller=false`) since the user owns the object — breaking with the Kubernetes precedent that composite workloads own their subordinate objects (Deployment→ReplicaSet, DisaggregatedSet→LeaderWorkerSet). The autocreate design keeps the same CRD schema; the (DS, role) association just moves from an explicit spec field to the deterministic name + controller ownerRef.

