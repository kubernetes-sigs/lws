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
  - [Validation](#validation)
  - [Edge Cases](#edge-cases)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Point HPA directly at the underlying LeaderWorkerSet](#alternative-1-point-hpa-directly-at-the-underlying-leaderworkerset)
  - [Alternative 2: Expose /scale directly on DisaggregatedSet](#alternative-2-expose-scale-directly-on-disaggregatedset)
  - [Alternative 3: Embed scaling config inline in DisaggregatedRoleSpec](#alternative-3-embed-scaling-config-inline-in-disaggregatedrolespec)
  - [Alternative 4: Require the user to author the scaler CR](#alternative-4-require-the-user-to-author-the-scaler-cr)
  - [Alternative 5: Bootstrap seed field on the role](#alternative-5-bootstrap-seed-field-on-the-role)
<!-- /toc -->

## Summary

This KEP proposes adding per-role autoscaling to `DisaggregatedSet` by introducing a new lightweight CRD, `DisaggregatedSetRoleScaler`, that exposes the `/scale` subresource. When a user opts a role into external scaling via `scaling.mode: External`, the `DisaggregatedSet` controller **automatically creates and manages** a `DisaggregatedSetRoleScaler` for that role. The scaler is a per-role `/scale` target that an HPA, KEDA `ScaledObject`, or any custom `/scale`-aware controller can drive; the DisaggregatedSet controller reads its `spec.replicas` on every reconcile and drives the underlying LeaderWorkerSet.

The feature is fully opt-in and per-role: roles without `scaling.mode: External` behave exactly as they do today.

Users author two objects: the `DisaggregatedSet` and their HPA/KEDA target. The scaler CR itself is controller-managed — created and cleaned up by the DisaggregatedSet controller, in the same way the DisaggregatedSet already creates a `LeaderWorkerSet` per role.

## Motivation

`DisaggregatedSet` orchestrates multiple LeaderWorkerSets (LWS) for disaggregated inference workloads. Under a rolling update, the controller creates a new LWS per role with a revision-hashed name (e.g., `myds-6ad7c921-prefill`) and progressively drains the old-revision LWS.

Autoscaling a role today is only possible by pointing an HPA at the underlying LWS (see the [LWS HPA example](https://lws.sigs.k8s.io/docs/examples/hpa/)). This breaks for `DisaggregatedSet` because:

1. **LWS names change on every rollout.** The revision hash is part of the name, so an HPA created for `myds-6ad7c921-prefill` becomes an orphan the moment a rolling update produces `myds-7bf3d1a2-prefill`. Users would have to recreate the HPA on every deploy.
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

That's it. The scaler CR appears on the next DS reconcile — the user does not author it. The autoscaler is responsible for the first write to `spec.replicas` (HPA enforces its `minReplicas` floor unconditionally; KEDA writes via its scale-from-zero paths; custom autoscalers with their own floor bootstrap themselves). Until that first write, the role is held at 0 replicas and the DS reports a `WaitingForScaler` condition.

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

The same layout works with vanilla HPA v2 — just swap the `ScaledObject` for a `HorizontalPodAutoscaler` targeting `my-llm-serving-prefill`. HPA enforces `minReplicas` unconditionally, so the role bootstraps from zero on the first HPA tick.

#### Story 2: Rolling update while HPA is active

An SRE pushes a new container image. A rolling update starts. Meanwhile, request load spikes and the HPA wants to grow the new revision from 5 to 8. The desired behavior:

- The old-revision LWS keeps its snapshotted initial replica count and continues to drain on the existing schedule.
- The new revision's target is bumped from 5 to 8 in-flight; the planner picks up the new target on the next stability window and continues rolling.
- The HPA is never confused by intermediate revision transitions because the scaler's `status.selector` is rewritten to point at the current new-revision LWS.

### Risks and Mitigations

**Risk**: HPA and rolling update race conditions cause thrashing or stuck rollouts.

**Mitigation**: The scaler drives only the *new-revision target*. Old-revision replicas continue to use the existing snapshot-and-drain mechanism (unchanged). A guard in the planner prevents the new-revision target from shrinking below its current in-flight value mid-rollout, which would otherwise force a scale-down/scale-up flip.

**Risk**: Users configure inline `replicas` and set `scaling.mode: External` on the same role, causing confusion about which wins.

**Mitigation**: An explicit `scaling.mode` enum on the role makes intent visible in the DS spec. A CEL rule on `DisaggregatedRoleSpec` forbids `spec.replicas > 0` when `scaling.mode == External`. The all-or-nothing CEL rule on `DisaggregatedSetSpec` is scoped to non-External roles.

**Risk**: The autoscaler is not yet applied (or has never written) when the DS is applied, or a role is flipped from `Static` to `External` while serving traffic — in production this must not silently drain the role to 0.

**Mitigation**: The controller **never scales an existing LeaderWorkerSet down as a side effect of switching to `External`**. Concretely, when the scaler for an External role has `spec.replicas: nil` (no autoscaler write yet):
- If the role's LWS **does not exist yet** (fresh DisaggregatedSet, or newly added role), it is created at 0 replicas and `WaitingForScaler` is reported.
- If the role's LWS **already exists** (e.g. a role that was `Static` with 5 replicas is switched to `External`), the controller holds it at its current replica count. The rolling-update planner behaves the same way: it clamps `targetNew` to the current in-flight value rather than shrinking. `WaitingForScaler` is still reported so operators can see that an autoscaler is expected.

Once the autoscaler makes its first write, the LWS scales to that value on the next reconcile. HPA and KEDA both write `minReplicas` / `minReplicaCount` unconditionally when the target is below the floor, so the "hold" window closes quickly. Custom autoscalers that only observe per-pod metrics and lack a min-replicas floor must issue a one-time `kubectl scale` to bootstrap.

**Risk**: A user deletes the auto-created scaler expecting it to stay gone.

**Mitigation**: The controller recreates it on the next reconcile. To truly opt out of external scaling, the user flips the role's `scaling.mode` back to `Static` (or removes `scaling` entirely); the controller then deletes the scaler in the same pass.

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
    // Replicas is the observed replica count of the role's current
    // new-revision LeaderWorkerSet. Read by the /scale subresource.
    // +optional
    Replicas int32 `json:"replicas,omitempty"`

    // Selector is a label selector (in string form) matching pods of the
    // role's current new-revision LeaderWorkerSet. Used by HPA to compute
    // per-pod metrics. Rewritten by the DS controller on every rolling
    // update as the new-revision LWS changes.
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
    //     .spec.replicas is ignored (and must be unset or zero).
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

A companion CEL rule on `DisaggregatedRoleSpec` forbids setting `replicas` when `Mode == External`:

```go
// +kubebuilder:validation:XValidation:rule="!has(self.scaling) || self.scaling.mode != 'External' || !has(self.spec.replicas) || self.spec.replicas == 0"
```

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

- `status.replicas`: pulled from the current new-revision LWS's `status.replicas` for that role.
- `status.selector`: the label selector matching pods of the current new-revision LWS. Format: `leaderworkerset.sigs.k8s.io/name=<lws-name>`. **This value must be rewritten on every rolling update** because the LWS name changes with the revision hash. Missing this update is what breaks HPA-on-LWS today.
- `status.observedGeneration`: `scaler.Generation`.
- `status.conditions`: `Ready` — True when the scaler is bound to a live DS + role and `status.replicas` / `status.selector` reflect the current observed LeaderWorkerSet. False during transient reconcile errors.

### Rolling Update Interaction

The scaler drives only the **new-revision target**. Old revisions retain their existing snapshot-and-drain behavior: the controller already snapshots each old LWS's `spec.replicas` into an `initial-replicas` annotation at rollout start, and that snapshot drives the drain trajectory. That mechanism is independent of where the "desired" count comes from, so it works unchanged for scaler-driven roles.

Two safety guards are added:

1. **New-target monotonicity mid-step.** Between planner iterations, the new-revision target for an External role is clamped to at least the current new-revision replica count. This prevents an HPA scale-down mid-rollout from flipping the planner into a scale-down state on a fleet that hasn't finished growing yet. Once the rollout completes, the guard releases and the target tracks the scaler exactly.

2. **Stability check unchanged.** The controller still waits for `replicas == readyReplicas` on the new revision before recomputing the next step. HPA writes that arrive during this window are simply picked up on the next iteration.

### Interaction with DisaggregatedSet Slices

[KEP-846](/keps/846-disaggregatedset-slices) adds `spec.slices: int` to DisaggregatedSet, replicating the whole role topology into N independent copies. Each slice rolls independently, and `spec.roles[].spec.replicas` becomes a *per-slice* count (total pods per role = `replicas × slices`). LWS names gain a slice segment (`<ds>-<slice>-<revision>-<role>`).

This has three unresolved implications for scaler-driven roles:

1. **Scope of `scaler.spec.replicas`.** Three possible shapes:
   - **Aggregate** — value is total across slices; controller divides among slices. Matches HPA's usual mental model.
   - **Per-slice scaler CR** — one scaler per (DS, role, slice). Explicit; combinatorial (`roles × slices` scalers per DS).
   - **Per-role, applied per-slice** — value is *per slice*, consistent with `spec.roles[].spec.replicas`. UX footgun: HPA sets N, gets N × slices pods.
2. **`status.selector` across slices.** With N slices, a per-role scaler must select pods across N LWS objects that may be on different revisions during a rollout. The single `leaderworkerset.sigs.k8s.io/name=<lws>` selector no longer suffices.
3. **N × R rollouts in flight.** The monotonicity guard would need to become per-(slice, role) since each slice rolls independently.

**Alpha scope: `spec.slices` must be 1 (default).** The DS webhook rejects a `spec.slices > 1` value while any role has `scaling.mode: External`. Since the scaler is auto-created only after this validation passes, no scaler ever exists in the multi-slice case. This lets alpha ship the single-slice case cleanly (which is the most common shape today) without prejudging the multi-slice design.

**Proposed direction for slices > 1** (out of scope for alpha, to be revisited in a follow-up KEP after production feedback): **per-slice scaler CR**. It is the most Kubernetes-idiomatic (one CR = one `/scale` target), has no math surprises, and lets users apply distinct scaling policies per slice (useful when slices map to different placement domains, per KEP-848). The cost — more CRs to manage — is opt-in and scales linearly with `roles × slices`.

### Validation

Because the controller owns the scaler lifecycle end-to-end (create, update, delete), the API surface exposed to the user is small and most validation lives in CEL rules on the DisaggregatedSet.

**Webhook (`DisaggregatedSetRoleScaler`)**:

- `spec.replicas >= 0` (also enforced by the OpenAPI schema).
- User-authored scalers are unusual — the controller creates them — but the webhook does not forbid them. If a user hand-authors a scaler that clashes with a controller-managed name, the controller detects that the object lacks its expected ownerRef, refuses to adopt it, and reports a warning event on the DisaggregatedSet.
- Alpha: reject creation if the DisaggregatedSet at the corresponding role (looked up by the scaler's `disaggregatedset.x-k8s.io/name` label) has `spec.slices > 1`. See [Interaction with DisaggregatedSet Slices](#interaction-with-disaggregatedset-slices).

**Webhook (`DisaggregatedSet`)**:

- For each role with `scaling.mode == External`, warn (not reject) if `spec.replicas` is set to a non-zero value — the CEL rule already enforces this, but the warning is friendlier.
- Reject if two roles share a name (already validated by `+listType=map`).
- For each role with `scaling.mode: External`, reject if `len(metadata.name) + 1 + len(role.name) > 253` (the derived scaler name `<ds>-<role>` would exceed the Kubernetes object-name limit). Error message names both the DS and the role so the user knows exactly which to shorten.
- Alpha: reject increasing `spec.slices` above 1 while any role has `scaling.mode == External`.

**CEL** (documented above): scaling-mode-aware refresh of the all-or-nothing rule, plus a role-level forbid-replicas-if-external rule.

### Edge Cases

| Case | Behavior |
|---|---|
| Role newly created as `External`, no LWS exists yet | Controller creates the scaler with `spec.replicas: nil` and the LWS at 0 replicas. DS condition `WaitingForScaler` set. Clears on the first autoscaler write. |
| Existing role flipped from `Static` to `External` (LWS running at N replicas) | Controller creates the scaler with `spec.replicas: nil`. LWS **holds at N** (planner clamps `targetNew` to current in-flight value; steady-state simple-reconcile skips scaling when scaler is not ready). DS reports `WaitingForScaler`. Once the autoscaler writes, the LWS moves to that value. No silent drain. |
| User deletes the auto-created scaler | Controller recreates it on the next reconcile. The LWS holds at its last observed count (planner treats a fresh `spec.replicas: nil` scaler as "not ready" and holds; see Replica Resolution). To truly opt out, flip the role to `Static`. |
| Role flipped from `External` back to `Static` | Controller deletes the auto-created scaler on the same reconcile pass; LWS drives from the inline `spec.replicas`. |
| DS deleted while scaler exists | Standard Kubernetes GC via `Controller=true` ownerRef. Scaler is deleted first, then the DS. |
| User hand-creates a scaler at the controller-managed name | Controller detects the missing ownerRef, refuses to adopt or overwrite it, emits a warning event on the DS. |
| Rolling update starts while HPA is actively writing | Rollout targets the scaler's current value; further HPA writes are picked up on each stability window. A mid-rollout HPA scale-down does not reverse direction (see Rolling Update Interaction). |
| Scaler `spec.replicas` = 0 | Role scales to 0 replicas. Consistent with `HPA min=0` semantics (e.g., KEDA scale-to-zero). |
| Custom autoscaler that observes per-pod metrics AND has no min-replicas floor | Cannot bootstrap from 0. User runs a one-time `kubectl scale disaggregatedsetrolescaler <ds>-<role> --replicas=N` to unblock. Documented as an operator limitation. |

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
- Rolling-update monotonicity guard
- Test coverage per plan above
- Documentation and example manifests for HPA and KEDA
- **Scope**: single-slice DisaggregatedSets only (`spec.slices == 1`). Multi-slice support is deferred to a follow-up KEP.

**Beta**:
- Production feedback incorporated
- Metrics: count of `WaitingForScaler` conditions
- Multi-slice support tracked in a follow-up KEP (proposed direction: per-slice scaler CR; see [Interaction with DisaggregatedSet Slices](#interaction-with-disaggregatedset-slices))

**Stable**:
- Proven stability across a range of autoscalers (HPA v2, KEDA, custom)
- No open bugs on the scaler pathway

## Drawbacks

1. **A second CRD kind for cluster admins to install.** Cluster admins now install `DisaggregatedSetRoleScaler` alongside `DisaggregatedSet` and `LeaderWorkerSet`. Users don't author instances directly, but the CRD schema still ships as part of the bundle.
2. **`kubectl get ds` no longer shows the effective replica count for External roles.** Users must also `kubectl get dsrs` (short name for the scaler CRD) to see the desired count. Mitigated by requiring the explicit `scaling.mode: External` marker so the DS spec at least signals that replicas are managed externally; a printer-column summary can be added post-alpha.
3. **Selector staleness window.** The scaler's `status.selector` is only refreshed at the DS controller's reconcile cadence. If a rolling update transitions the new-revision LWS just before the HPA reads the selector, the HPA could compute one round of metrics against the old LWS. Bounded (single-digit seconds in practice) but worth noting.
4. **Cold-start blind spot for exotic autoscalers.** Autoscalers that need per-pod metrics AND have no min-replicas floor cannot bootstrap the role from 0 (documented in edge cases). Nearly every production autoscaler (HPA, KEDA, custom Mimir-based, etc.) either has a min-replicas floor or observes external metrics, so this is a narrow case.

## Alternatives

### Alternative 1: Point HPA directly at the underlying LeaderWorkerSet

The existing LWS documentation supports `scaleTargetRef` → LeaderWorkerSet. In principle a user could target one of the LWS objects a DisaggregatedSet manages.

**Rejected because**: LWS names include a revision hash (`<ds>-<revision>-<role>`), so any HPA created against a specific LWS name becomes orphaned the moment a rolling update produces a new revision. The user would have to recreate the HPA on every deploy, which defeats the point.

### Alternative 2: Expose /scale directly on DisaggregatedSet

Add the `/scale` subresource to `DisaggregatedSet` itself.

**Rejected because**: `/scale` is single-valued (`spec.replicas`, `status.replicas`, `status.selector`). A DisaggregatedSet manages 2–10 roles. There is no coherent way for a single scale value to describe "prefill=5, decode=3" or for a single selector to match pods across multiple LWS revisions. Extending the scale subresource semantics is far beyond the scope of this KEP and would fork from standard HPA behavior.

### Alternative 3: Embed scaling config inline in DisaggregatedRoleSpec

Skip the new CRD; instead embed autoscaling target/current fields on the role itself, and expose a virtual scale subresource on the DisaggregatedSet parameterized by role (e.g., `/scale?role=prefill`).

**Rejected because**: virtual scale subresources parameterized by query strings are not supported by the Kubernetes API machinery. HPA/KEDA cannot target them. The only path to standard-shape `/scale` is a dedicated CRD.

### Alternative 4: Require the user to author the scaler CR

Same CRD shape, but with a `targetRef {name, role}` field the user fills in explicitly. The DS controller reads the scaler where present and attaches a non-controller ownerRef for GC.

**Rejected because**:
- Users have to write and name every scaler themselves (2N+1 manifests for N scalable roles instead of N+1).
- Ownership is unconventional: the controller can't take `Controller=true` because the user owns the object.
- Two consistency risks disappear with autocreate: (a) user-authored `targetRef` disagreeing with the intended DS/role (e.g. a typo); (b) users forgetting to create the scaler after flipping a role to `External` (leaves the role at 0 with no clear indication that a manifest is missing).
- Kubernetes precedent: composite workloads own their subordinate objects (Deployment→ReplicaSet, DisaggregatedSet→LeaderWorkerSet), and no established API asks users to hand-author what is really an implementation-managed piece of the parent's lifecycle. The scaler being the /scale target rather than a "pure internal" like a ReplicaSet doesn't change the ownership argument — the controller still needs to create and clean it up in lockstep with the role's mode.

The autocreate design keeps the same CRD schema — the target association just moves from an explicit `spec.targetRef` field to the deterministic name + controller ownerRef.

### Alternative 5: Bootstrap seed field on the role

Add `scaling.initialReplicas *int32` so the DS controller can seed the auto-created scaler at cold start (before any autoscaler has written).

**Rejected because**:
- Every real-world autoscaler bootstraps itself: HPA writes `minReplicas` unconditionally when the target is below the floor; KEDA does the same via `minReplicaCount`; custom Mimir-based autoscalers (e.g. mistral) have their own min-replicas config and write on their first tick.
- The field would duplicate a value users already set on their HPA/KEDA object (`minReplicas` / `minReplicaCount`), which reviewers reject as "why does the same information live in two places?".
- The only autoscalers it would help are those that observe per-pod metrics AND have no min-replicas floor — a narrow enough case that a one-time `kubectl scale` is an acceptable workaround.

Keeping the API narrow (no `initialReplicas` field, no cross-CRD watch on HPA) is the preferred trade.
