# KEP-849: DisaggregatedSet HPA support per role

<!--
This KEP proposes adding per-role HorizontalPodAutoscaler (HPA) support to
DisaggregatedSet through a new DisaggregatedSetRoleScaler CRD that exposes the
/scale subresource. This allows KEDA, HPA, or any autoscaler to drive replica
counts for individual roles (e.g., "prefill" or "decode") independently.
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
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Point HPA directly at the underlying LeaderWorkerSet](#alternative-1-point-hpa-directly-at-the-underlying-leaderworkerset)
  - [Alternative 2: Expose /scale directly on DisaggregatedSet](#alternative-2-expose-scale-directly-on-disaggregatedset)
  - [Alternative 3: Embed scaling config inline in DisaggregatedRoleSpec](#alternative-3-embed-scaling-config-inline-in-disaggregatedrolespec)
<!-- /toc -->

## Summary

This KEP proposes adding per-role autoscaling to `DisaggregatedSet` by introducing a new lightweight CRD, `DisaggregatedSetRoleScaler`, that exposes the `/scale` subresource. When a user opts into external scaling for a role, the `DisaggregatedSet` controller stops reading `replicas` from the role's inline spec and instead pulls the desired replica count from the scaler CR — meaning **the number of replicas is effectively supplied by the HPA** (or KEDA, or any other `/scale`-aware controller).

The feature is fully opt-in and per-role: roles without a scaler continue to work exactly as they do today.

## Motivation

`DisaggregatedSet` orchestrates multiple LeaderWorkerSets (LWS) for disaggregated inference workloads. Under a rolling update, the controller creates a new LWS per role with a revision-hashed name (e.g., `myds-6ad7c921-prefill`) and progressively drains the old-revision LWS.

Autoscaling a role today is only possible by pointing an HPA at the underlying LWS (see the [LWS HPA example](https://lws.sigs.k8s.io/docs/examples/hpa/)). This breaks for `DisaggregatedSet` because:

1. **LWS names change on every rollout.** The revision hash is part of the name, so an HPA created for `myds-6ad7c921-prefill` becomes an orphan the moment a rolling update produces `myds-7bf3d1a2-prefill`. Users would have to recreate the HPA on every deploy.
2. **DisaggregatedSet has no stable per-role scale target.** The parent CR aggregates multiple roles, so a single `/scale` subresource on `DisaggregatedSet` cannot express "scale prefill from 5 to 8 without touching decode".
3. **Disaggregated workloads scale per role.** Prefill and decode have different bottlenecks (compute-bound vs memory-bandwidth-bound). Autoscaling each independently is the natural pattern; a single knob for both roles is the wrong shape.

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

Each `DisaggregatedSetRoleScaler` instance targets exactly one `(DisaggregatedSet, role)` pair via a `targetRef`. To opt into external scaling, the user:

1. Sets `scaling.mode: External` on the role inside the `DisaggregatedSet` spec.
2. Creates a `DisaggregatedSetRoleScaler` whose `targetRef` names the DisaggregatedSet and role.
3. Creates an HPA (or KEDA `ScaledObject`) whose `scaleTargetRef` names the scaler CR.

The DisaggregatedSet controller then reads the desired replica count for that role from `scaler.spec.replicas` on every reconcile and writes back `scaler.status.replicas` and `scaler.status.selector` so the HPA loop closes.

### User Stories

#### Story 1: KEDA scales prefill on queue depth

An SRE runs a disaggregated vLLM deployment. Prefill is bottlenecked on request queue depth exposed via a Prometheus metric; decode is bottlenecked on GPU memory bandwidth. They want KEDA to scale prefill from 2 to 20 based on queue depth, and keep decode at a static 4.

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: my-llm-serving
spec:
  roles:
    - name: prefill
      scaling:
        mode: External           # replicas come from the scaler
      spec:
        # replicas: omitted — ignored anyway
        leaderWorkerTemplate: {...}
    - name: decode
      spec:
        replicas: 4              # static
        leaderWorkerTemplate: {...}
---
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSetRoleScaler
metadata:
  name: my-llm-prefill-scaler
spec:
  targetRef:
    name: my-llm-serving
    role: prefill
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: my-llm-prefill
spec:
  scaleTargetRef:
    apiVersion: disaggregatedset.x-k8s.io/v1
    kind: DisaggregatedSetRoleScaler
    name: my-llm-prefill-scaler
  minReplicaCount: 2
  maxReplicaCount: 20
  triggers:
    - type: prometheus
      metadata:
        query: vllm_queue_depth{role="prefill"}
        threshold: "10"
```

#### Story 2: Rolling update while HPA is active

An SRE pushes a new container image. A rolling update starts. Meanwhile, request load spikes and the HPA wants to grow the new revision from 5 to 8. The desired behavior:

- The old-revision LWS keeps its snapshotted initial replica count and continues to drain on the existing schedule.
- The new revision's target is bumped from 5 to 8 in-flight; the planner picks up the new target on the next stability window and continues rolling.
- The HPA is never confused by intermediate revision transitions because the scaler's `status.selector` is rewritten to point at the current new-revision LWS.

### Risks and Mitigations

**Risk**: HPA and rolling update race conditions cause thrashing or stuck rollouts.

**Mitigation**: The scaler drives only the *new-revision target*. Old-revision replicas continue to use the existing snapshot-and-drain mechanism (unchanged). A guard in the planner prevents the new-revision target from shrinking below its current in-flight value mid-rollout, which would otherwise force a scale-down/scale-up flip.

**Risk**: Users configure inline `replicas` and a scaler for the same role, causing confusion about which wins.

**Mitigation**: An explicit `scaling.mode` enum on the role makes intent visible in the DS spec. Webhook validation forbids `replicas > 0` on an `External` role. CEL validation on the DS spec is scoped to non-External roles.

**Risk**: The scaler CR is created after the DS references it (`scaling.mode: External` but no scaler exists yet), leaving the role at 0 replicas indefinitely.

**Mitigation**: The controller surfaces a `WaitingForScaler` condition and emits a warning event on the DS. The role stays at 0 replicas until the scaler is created — safer than guessing an initial count.

**Risk**: The scaler is deleted while a role is running, silently returning replicas to `spec.replicas` (which may be 0 or unset).

**Mitigation**: An owner reference from the DS to the scaler (non-controller, non-blocking) allows GC when the DS is deleted but keeps the scaler visible in `kubectl get`. If the user deletes the scaler manually, the controller reports a `WaitingForScaler` condition and holds at the last known replica count until the user either recreates the scaler or flips the role back to `Static`.

## Design Details

### DisaggregatedSetRoleScaler API

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:shortName=dsrs

// DisaggregatedSetRoleScaler exposes the /scale subresource for a single
// (DisaggregatedSet, role) pair, allowing external autoscalers (HPA, KEDA,
// custom) to drive that role's replica count independently of the rest of the
// DisaggregatedSet.
type DisaggregatedSetRoleScaler struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitzero"`

    Spec   DisaggregatedSetRoleScalerSpec   `json:"spec"`
    Status DisaggregatedSetRoleScalerStatus `json:"status,omitzero"`
}

type DisaggregatedSetRoleScalerSpec struct {
    // TargetRef selects the DisaggregatedSet and role this scaler drives.
    // The DisaggregatedSet must exist in the same namespace.
    // +required
    TargetRef DisaggregatedSetRoleRef `json:"targetRef"`

    // Replicas is the desired replica count for the referenced role's
    // new-revision LeaderWorkerSet. Written by the /scale subresource
    // (i.e., by HPA/KEDA). Read by the DisaggregatedSet controller.
    // +optional
    Replicas *int32 `json:"replicas,omitempty"`
}

type DisaggregatedSetRoleRef struct {
    // Name is the DisaggregatedSet name (same namespace).
    // +required
    Name string `json:"name"`

    // Role is the role name inside that DisaggregatedSet.
    // +required
    Role string `json:"role"`
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

    // Conditions expose scaler-level state:
    //   - Ready: True when a matching DS and role were resolved and status is fresh
    //   - WaitingForScaler is NOT a scaler condition (see DS conditions)
    //   - TargetMissing: True when the referenced DS or role does not exist
    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

**Uniqueness**: A validating webhook rejects the creation of a `DisaggregatedSetRoleScaler` if another scaler with the same `targetRef` already exists in the namespace. As a backstop against races, the scaler controller sets a `Conflicting` status condition and refuses to serve reads.

**Owner reference**: On first reconcile, the DS controller adds an owner reference from the DS to the scaler with `Controller=false, BlockOwnerDeletion=false`. This ensures the scaler is garbage-collected if the DS is deleted, without transferring ownership away from the user.

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
    //   - External: expect a DisaggregatedSetRoleScaler whose targetRef
    //     points at this DisaggregatedSet + role. .spec.replicas is ignored.
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

The `Scaling` field is a sub-struct rather than a bare enum to leave room for per-role scaling policies (e.g., `PauseDuringRollout`, `MinReplicasAtBoot`) without a v2 bump. `Scaling: nil` is fully backward compatible: existing DisaggregatedSet objects behave exactly as before.

**Existing CEL rule change**: today's rule enforces that replicas is either zero for all roles or non-zero for all roles. It must become scaling-mode-aware — External roles are exempt from the all-or-nothing rule because their effective replicas live outside the DS spec:

```go
// +kubebuilder:validation:XValidation:rule="self.roles.filter(r, !has(r.scaling) || r.scaling.mode != 'External').all(r, !has(r.spec.replicas) || r.spec.replicas == 0) || self.roles.filter(r, !has(r.scaling) || r.scaling.mode != 'External').all(r, has(r.spec.replicas) && r.spec.replicas > 0)"
```

A companion CEL rule on `DisaggregatedRoleSpec` forbids setting `replicas` when `Mode == External`:

```go
// +kubebuilder:validation:XValidation:rule="!has(self.scaling) || self.scaling.mode != 'External' || !has(self.spec.replicas) || self.spec.replicas == 0"
```

### Replica Resolution: DS reads replicas from HPA

**This is the load-bearing change of this KEP.** The `DisaggregatedSet` controller no longer treats `.spec.roles[].spec.replicas` as the sole source of truth. For roles with `scaling.mode: External`, it delegates the replica count to the HPA via the scaler CR.

The resolution rule for a role's desired replica count is:

- **`scaling.mode == External`** → read `scaler.spec.replicas` from the `DisaggregatedSetRoleScaler` whose `targetRef` matches this DS + role. If no scaler exists yet, or `scaler.spec.replicas` is unset, treat the role as "not ready" and surface a `WaitingForScaler` condition on the DS.
- **`scaling.mode == Static` (or `scaling` unset)** → read `.spec.replicas` from the role, defaulting to `1` if unset. This is today's behavior, preserved unchanged.

The controller loads all scalers whose `targetRef.name` matches the DS name at the start of each reconcile (single list call) and uses that map for every replica-count lookup within the reconcile pass. The same map feeds the rolling-update planner so that scaler-driven values are consistent across scale-up, scale-down, and stability checks within one pass.

### Controller Wiring

The DS controller adds a watch on `DisaggregatedSetRoleScaler`. Scaler events are mapped to a reconcile request for the DS named in `targetRef` (same namespace as the scaler). This closes the write path end-to-end:

```
HPA writes /scale → scaler.spec.replicas updates → watch fires → DS reconciles → LWS scaled
```

Reconcile latency for a scaler-triggered event is a single controller hop.

### Scale Subresource Status Fields

The DS controller writes back to each scaler's status at the end of every reconcile:

- `status.replicas`: pulled from the current new-revision LWS's `status.replicas` for that role.
- `status.selector`: the label selector matching pods of the current new-revision LWS. Format: `leaderworkerset.sigs.k8s.io/name=<lws-name>`. **This value must be rewritten on every rolling update** because the LWS name changes with the revision hash. Missing this update is what breaks HPA-on-LWS today.
- `status.observedGeneration`: `scaler.Generation`.
- `status.conditions`: `Ready` / `TargetMissing` / `Conflicting`.

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

**Alpha scope: `spec.slices` must be 1 (default).** The scaler webhook rejects creation of a `DisaggregatedSetRoleScaler` whose `targetRef` points at a DS with `slices > 1`. Symmetrically, the DS webhook rejects an increase of `slices` above 1 while any External-mode role exists. This lets alpha ship the single-slice case cleanly (which is the most common shape today) without prejudging the multi-slice design.

**Proposed direction for slices > 1** (out of scope for alpha, to be revisited in a follow-up KEP after production feedback): **per-slice scaler CR**. It is the most Kubernetes-idiomatic (one CR = one `/scale` target), has no math surprises, and lets users apply distinct scaling policies per slice (useful when slices map to different placement domains, per KEP-848). The cost — more CRs to manage — is opt-in and scales linearly with `roles × slices`.

### Validation

**Webhook (`DisaggregatedSetRoleScaler`)**:

- `targetRef.name` and `targetRef.role` are non-empty.
- The referenced DS need not exist at admission time (allow GitOps ordering); missing target surfaces as a `TargetMissing` status condition.
- Reject if another scaler in the namespace already has the same `targetRef`.
- Reject `spec.replicas < 0`.
- Alpha: reject if the referenced DS has `spec.slices > 1` (see [Interaction with DisaggregatedSet Slices](#interaction-with-disaggregatedset-slices)).

**Webhook (`DisaggregatedSet`)**:

- For each role with `scaling.mode == External`, warn (not reject) if `spec.replicas` is set to a non-zero value — the CEL rule already enforces this, but the warning is friendlier.
- Reject if two roles share a name (already validated by `+listType=map`).
- Alpha: reject increasing `spec.slices` above 1 while any role has `scaling.mode == External`.

**CEL** (documented above): scaling-mode-aware refresh of the all-or-nothing rule, plus a role-level forbid-replicas-if-external rule.

### Edge Cases

| Case | Behavior |
|---|---|
| DS references `External` role but no scaler exists yet | Role held at 0 replicas. DS condition `WaitingForScaler` set with a message pointing at the expected `targetRef`. |
| Scaler exists with `spec.replicas` unset | Same as "no scaler exists". Waiting for HPA to set an initial value. |
| Scaler exists but DS role has `scaling.mode == Static` (or `scaling` nil) | Scaler is ignored. Its status surfaces `TargetMissing` condition (the role is not `External`). |
| Scaler deleted mid-run | On next reconcile, `WaitingForScaler` condition set. LWS is *not* scaled to 0; last-known target holds until the user acts. |
| Two scalers created for the same (DS, role) | Second is rejected by webhook. If a race slips through, both get `Conflicting` conditions and neither is honored. |
| DS deleted while scaler exists | Owner ref triggers GC of the scaler. |
| Rolling update starts while HPA is actively writing | Rollout targets the scaler's current value; further HPA writes are picked up on each stability window. A mid-rollout HPA scale-down does not reverse direction (see Rolling Update Interaction). |
| Scaler `spec.replicas` = 0 | Role scales to 0 replicas. Consistent with `HPA min=0` semantics (e.g., KEDA scale-to-zero). |

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

## Implementation History

- 2026-07-03: Initial KEP draft

## Drawbacks

1. **A second CRD to manage.** Users who want autoscaling must create one scaler per role plus their HPA/KEDA resource. Compared with pointing HPA directly at a resource that already exists, this is more objects.
2. **`kubectl get ds` no longer shows the effective replica count for External roles.** Users must also `kubectl get dsrs` (short name for the scaler CRD) to see the desired count. Mitigated by including scaler status summary in `kubectl get ds -o wide` (post-alpha) and by requiring the explicit `scaling.mode: External` marker so the DS spec at least signals that replicas are managed externally.
3. **Selector staleness window.** The scaler's `status.selector` is only refreshed at the DS controller's reconcile cadence. If a rolling update transitions the new-revision LWS just before the HPA reads the selector, the HPA could compute one round of metrics against the old LWS. This is a bounded and small window (single-digit seconds in practice) but is worth noting.

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
