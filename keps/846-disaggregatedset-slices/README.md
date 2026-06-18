# KEP-846: DisaggregatedSet Slices

<!--
This KEP proposes adding a `slices` parameter to DisaggregatedSet that replicates the whole role topology into N independent copies, each rolling out independently.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Scale identical deployments](#story-1-scale-identical-deployments)
    - [Story 2: One slice per accelerator domain](#story-2-one-slice-per-accelerator-domain)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
  - [Naming and Labels](#naming-and-labels)
  - [Slices Are Excluded From the Revision Hash](#slices-are-excluded-from-the-revision-hash)
  - [Per-Slice Reconciliation](#per-slice-reconciliation)
  - [Services](#services)
  - [Scale-Down](#scale-down)
  - [Backward Compatibility](#backward-compatibility)
  - [Object Cardinality](#object-cardinality)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Use per-role replicas](#alternative-1-use-per-role-replicas)
  - [Alternative 2: Multiple DisaggregatedSet objects](#alternative-2-multiple-disaggregatedset-objects)
  - [Alternative 3: Include slices in the revision hash](#alternative-3-include-slices-in-the-revision-hash)
  - [Alternative 4: Revision before slice in names](#alternative-4-revision-before-slice-in-names)
<!-- /toc -->

## Summary

This KEP adds a `slices` field to the [DisaggregatedSet](/keps/766-DisaggregatedSet) API. `slices` is an integer that replicates the entire role topology into N independent copies. Each slice is a complete set of all roles (e.g. `prefill` and `decode`) that rolls out independently of the other slices.

A DisaggregatedSet today describes exactly one copy of a disaggregated-serving topology. Running K identical copies requires duplicating the YAML, which does not scale and offers no per-copy placement or rollout control. `slices` makes replication a single field and is the foundation for per-domain placement policy (tracked separately in [#848](https://github.com/kubernetes-sigs/lws/issues/848)).

## Motivation

DisaggregatedSet coordinates a set of roles as one version-synchronized unit, but it has no notion of replicating that whole unit. Operators who want several identical prefill/decode deployments must today:

1. **Duplicate YAML** per copy, with all the drift and review burden that implies.
2. **Manage each copy as a separate object**, with no shared spec and no single unit to scale.
3. **Lack a placement primitive**: there is no per-copy identity to pin a copy to an accelerator domain (e.g. an NVL72 rack), or at the very least have an affinity against sharing a domain with other copies, which disaggregated serving deployments need for durability.

A single replication count on the DisaggregatedSet solves the first two directly, and introduces the per-copy identity (the "slice") that placement policy will build on.

### Goals

1. Add `spec.slices` (integer, default 1, minimum 1) that replicates the whole role topology into N independent copies.
2. Make each slice an independent rolling-update domain: a template change rolls each slice on its own clock, always keeping a complete same-version set serving per slice.
3. Treat `slices` as a scale operation: changing it never triggers a rollout of existing slices.
4. Give each slice a stable identity (label + name segment) that placement policy can key on later.

### Non-Goals

1. **Placement policy / topology spreading.** Scheduling slices onto separate or shared domains is tracked in [#848](https://github.com/kubernetes-sigs/lws/issues/848). This KEP only provides the slice identity it will use.
2. **Failure rebalancing.** Maintaining a target ratio of replicas across roles when nodes fail is out of scope.
3. **Aggregate cross-slice Service / traffic routing.** This KEP keeps the existing per-revision-per-role Service model, scoped per slice. A single front-door Service across slices is not introduced.
4. **Heterogeneous slices.** All slices share the same spec; per-slice overrides are not supported.

## Proposal

Add a top-level `spec.slices` field to `DisaggregatedSetSpec`. The controller reconciles each slice index in `[0, slices)` independently, reusing the existing per-DisaggregatedSet logic (LWS creation, the N-dimensional rolling-update planner, and Service management) scoped to one slice. A new `disaggregatedset.x-k8s.io/slice` label and a slice segment in generated names distinguish the copies. Decreasing `slices` deletes the removed slices' resources directly.

### User Stories

#### Story 1: Scale identical deployments

An operator runs a prefill/decode deployment and needs four identical copies for capacity. Instead of maintaining four near-identical manifests, they set `spec.slices: 4` on one DisaggregatedSet and manage it as a single object. Scaling to six copies is a one-line edit; the controller stands up slices 4 and 5 at the current revision without touching the existing four.

#### Story 2: One slice per accelerator domain

A disaggregated-serving deployment wants each complete prefill+decode copy confined to a single low-latency accelerator domain (e.g. an NVL72 rack), so prefill can hand its KV cache to decode within the domain. `slices` delivers the prerequisite for this: each copy gets a stable identity and an independent rollout, making it a self-contained, individually-addressable unit that can be rolled or recovered on its own. Pinning each slice to a domain is out of scope here and is delivered by placement policy ([#848](https://github.com/kubernetes-sigs/lws/issues/848)), which builds on the slice identity introduced by this KEP.

### Notes/Constraints/Caveats

- Slices are identical copies of the spec. The only thing that differs between slice 0 and slice 1 is the slice index in their labels and names.
- A slice is the durable, outer identity; a revision is ephemeral. Within a slice, normally one revision is live, and two coexist transiently while that slice rolls.
- Because slices roll independently, different slices can momentarily be on different revisions during a rollout. This is intentional and is what lets a single slice (domain) be updated or recover on its own.

### Risks and Mitigations

**Risk**: Adding the slice segment changes the generated LWS names (`<ds>-<revision>-<role>` becomes `<ds>-<slice>-<revision>-<role>`). A naive rollout would rename, and therefore recreate, the LWS (and pods) of DisaggregatedSets that already exist in clusters running a pre-slices release.

**Mitigation**: The controller adopts a pre-slices LWS (one with no `disaggregatedset.x-k8s.io/slice` label) in place as slice 0 under its legacy name, so upgrading recreates nothing. New LWS always use the slice-aware name, and a legacy slice converges to it on its next rollout or when `slices` is increased above 1. See [Backward Compatibility](#backward-compatibility).

**Risk**: More API objects exist (`slices x roles` LWS, and the same multiple of Services).

**Mitigation**: LWS is a lightweight coordination resource; the number of pods per copy is unchanged. The pod count scales with `slices` exactly as it would with duplicated manifests.

**Risk**: The independent per-slice rollout could leave slices on different revisions for an extended time if one slice is stuck.

**Mitigation**: Each slice runs the same per-slice rollout logic that already guarantees a complete same-version set is always serving within that slice; a stuck slice degrades only itself, not the others. The controller is stateless and can be inspected per slice via labels.

## Design Details

### API

A single optional field is added to `DisaggregatedSetSpec`:

```go
// DisaggregatedSetSpec defines the desired state of DisaggregatedSet
type DisaggregatedSetSpec struct {
    // Roles defines the list of roles (at least 2 required, maximum 10).
    // +kubebuilder:validation:MinItems=2
    // +kubebuilder:validation:MaxItems=10
    // +required
    Roles []DisaggregatedRoleSpec `json:"roles"`

    // Slices is the number of independent copies of the whole role topology.
    // Each slice is a complete set of all roles that rolls out independently.
    // Changing Slices scales copies up or down and does not trigger a rollout.
    // +optional
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=1
    Slices *int32 `json:"slices,omitempty"`
}
```

`slices` uses the JSON name `slices` (not `replicas`) to avoid confusion with the existing per-role `spec.roles[].spec.replicas`, which controls the number of groups within a single role.

### Naming and Labels

Managed LWS objects (and their Services) gain a slice segment:

- LWS name: `<ds>-<slice>-<revision>-<role>` (e.g. `my-ds-0-abc12345-prefill`).
- Service name: `<ds>-<slice>-<revision>-<role>-prv`.

A new label is added to every managed LWS, pod template, and Service:

- `disaggregatedset.x-k8s.io/slice`: the slice index.

The slice segment is placed **before** the revision because the slice is the durable identity (it maps to a placement domain and persists across rollouts) while the revision is ephemeral (created and drained on each template change). All controller logic keys off the labels, not the parsed name, so the name ordering is for human readability and uniqueness.

### Slices Are Excluded From the Revision Hash

The revision hash is computed only from each role's name and `LeaderWorkerTemplate` (the pod template). `slices` is a top-level field and is deliberately not part of the hash. Therefore changing `slices` never changes the revision and never triggers a rollout: adding a slice brings a new copy up at the current revision, and removing a slice tears one down, in both cases as a scale operation.

### Per-Slice Reconciliation

`Reconcile` computes the (DisaggregatedSet-wide) target revision once, then iterates slice indices `[0, slices)`. For each slice it runs the existing per-DisaggregatedSet logic scoped to that slice: drained-revision cleanup, the rolling-update-vs-simple decision, and Service reconciliation.

All listing, creation, and rolling-update calls are scoped to the slice via the slice label. Legacy LWS that carry no slice label are bucketed into slice 0 (see [Backward Compatibility](#backward-compatibility)).

Crucially, the **rolling-update planner is unchanged**. The N-dimensional algorithm ([KEP-766](/keps/766-DisaggregatedSet)) operates on the per-role replica vectors of a single slice; running it once per slice yields independent per-slice rollouts with no new algorithm. The controller aggregates the per-slice reconcile results (e.g. requeue) into a single result.

### Services

Services remain per `(slice, revision, role)`, named `<ds>-<slice>-<revision>-<role>-prv`, with the slice added to both the labels and the selector. Scoping the selector to the slice keeps role-to-role pairing within a slice: a prefill server in slice 0 of a revision discovers the decode servers in slice 0 of the same revision, which is required for in-domain KV-cache handoff. As today, a slice's per-revision Service is created only once that revision is ready on all roles within the slice.

### Scale-Down

Decreasing `slices` from M to N directly deletes the LWS and Services whose slice index is `>= N`. Their pods terminate via the normal pod grace period; no controller-orchestrated drain is performed.

This is intentional and differs from revision teardown. The existing multi-phase drain exists to preserve the cross-role, same-version invariant *within* a slice during a rollout. Slice removal has no cross-slice invariant to protect (slices are independent), so it is a plain scale operation, mirroring how reducing a role's `replicas` removes the highest-ordinal groups directly.

### Backward Compatibility

DisaggregatedSet shipped before this feature, so clusters already run DisaggregatedSets whose LWS use the old `<ds>-<revision>-<role>` name and carry no slice label. Object names are immutable and a pod's slice label comes from its template, so a legacy pod cannot gain that label without being recreated. The controller therefore adopts legacy objects in place rather than renaming them on upgrade, then migrates them to the slice-aware form at the first safe opportunity. This follows the precedent set when controller revisions were introduced (KEP-238).

**Adoption.** Per-slice listing buckets any LWS with no slice label into slice 0, so a legacy LWS is reconciled as slice 0 under its existing name and its existing Service keeps serving it. Every LWS created from now on uses the slice-aware name, including the `-0-` segment for slice 0. A plain upgrade with no spec change recreates nothing.

**Migration on the next rollout.** A template change creates the new revision's slice-0 LWS in slice-aware form while the legacy LWS drains through the normal rolling update. The two are at different revisions and Services are revision-scoped, so they never select each other's pods, and the legacy Service is deleted when the old revision drains.

**Migration when `slices` increases above 1.** Here no new revision is created, and the legacy slice-0 Service is revision-scoped but slice-agnostic, so it would also select the new sibling slices' pods. For example, with legacy slice 0 at revision `r`, a `slice 1` created at the same `r` is matched by the legacy `{set, role, revision: r}` selector because the new `{set, slice: 1, role, revision: r}` pod is a superset of it. To prevent this, increasing `slices` first migrates slice 0 to the slice-aware form at the current revision, and blocks sibling creation until no label-less slice-0 LWS remains at the target revision. Once slice 0 is slice-aware, every Service is slice-scoped and the siblings can safely share the revision.

### Object Cardinality

For a DisaggregatedSet with R roles, S slices, and revision hash H in steady state:

- LWS: exactly one per `(slice, revision, role)`; `S x R` total, all labeled revision H.
- Pods: `replicas x size` per `(slice, role)`.
- Services: one per `(slice, revision, role)`.

During a rollout, a slice that is mid-transition holds two revisions at once (old draining, new filling), so up to `2 x R` LWS exist for that slice until the old revision drains.

### Test Plan

[X] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Unit tests

- Name/label helpers: slice segment in names, slice label emitted.
- Controller: fan-out creates `slices x roles` LWS with correct slice labels; slice scale-down deletes only the removed slices' LWS and Services and leaves lower slices intact.
- Service manager: per-slice Service naming, labels, and selector; per-slice drained cleanup.
- Executor: slice is threaded through rolling-update calls; the planner is exercised per slice (existing planner tests are unchanged).

#### Integration tests

- Creating a DisaggregatedSet with `slices: N` produces N independent copies (LWS, pods, per-slice Services) with correct names and labels.
- A template change rolls each slice independently to the new revision while keeping a complete same-version set serving per slice, then garbage-collects the old revision.
- Increasing `slices` adds copies at the current revision without rolling existing slices.
- Decreasing `slices` deletes only the removed slices.

### Graduation Criteria

`slices` graduates together with DisaggregatedSet ([KEP-766](/keps/766-DisaggregatedSet)), as part of the same API and controller.

**Alpha**:
- `spec.slices` with validation (default 1, minimum 1).
- Per-slice fan-out, independent per-slice rollout, and slice scale-down.
- Per-slice Services.
- Unit and integration test coverage.
- Documentation and a sample manifest.

**Beta / Stable**: follows DisaggregatedSet graduation; incorporate production feedback and add metrics/observability for per-slice rollout state.

## Implementation History

- 2026-06-10: Initial KEP draft.
- 2026-06-10: Implementation submitted in [PR #873](https://github.com/kubernetes-sigs/lws/pull/873).

## Drawbacks

1. **More objects.** `slices x roles` LWS and Services exist instead of one set, increasing API-server and controller bookkeeping (though pod count per copy is unchanged).
2. **Transitional migration logic.** Because DisaggregatedSet already shipped, the controller must adopt legacy `<ds>-<revision>-<role>` objects in place and migrate them to the slice-aware form (see [Backward Compatibility](#backward-compatibility)). This adds adoption and one-time migration code that is purely transitional: it stops mattering once every DisaggregatedSet has rolled once or been scaled past a single slice.

## Alternatives

### Alternative 1: Use per-role replicas

DisaggregatedSet already has a per-role `replicas` that controls the number of groups in a role.

**Rejected because** per-role replicas scales the groups *within* one role; it does not replicate the whole prefill+decode topology, does not give each copy a stable identity for placement, and does not keep role-to-role pairing within a copy. Slices replicate the entire role set as independent, individually-placeable, independently-rolling units.

### Alternative 2: Multiple DisaggregatedSet objects

Create one DisaggregatedSet per copy.

**Rejected because** there is no single object to manage or scale, the copies share no spec (inviting drift), and there is no built-in per-copy identity tying the copies together as one logical deployment. A replication count keeps one declarative unit.

### Alternative 3: Include slices in the revision hash

Fold `slices` into the template revision hash.

**Rejected because** it would treat scaling copies as a template change and re-roll every existing slice whenever `slices` changes. Keeping `slices` out of the hash makes adding or removing a copy a cheap scale operation, consistent with how per-role `replicas` is treated.

### Alternative 4: Revision before slice in names

Order the generated name as `<ds>-<revision>-<slice>-<role>`, putting the revision before the slice.

**Rejected because** the slice is the durable identity (it maps to a placement domain and survives rollouts) while the revision is ephemeral. Ordering the stable axis (slice) before the transient one (revision) reflects ownership: a slice cycles through revisions, not the reverse. Controller logic keys off labels regardless, but the slice-first ordering is the accurate human-facing model and aligns with the per-slice independent rollout design.
