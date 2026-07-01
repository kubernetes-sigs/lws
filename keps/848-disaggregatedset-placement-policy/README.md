# KEP-848: DisaggregatedSet Placement Policy

<!--
This KEP proposes adding a placement policy to DisaggregatedSet that co-locates a slice's roles within a topology domain and spreads a DisaggregatedSet's slices across domains, with an option to make each domain exclusive to a single slice, via injected pod affinity and anti-affinity.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Co-locate and spread a DisaggregatedSet's slices](#story-1-co-locate-and-spread-a-disaggregatedsets-slices)
    - [Story 2: Give each slice a dedicated domain](#story-2-give-each-slice-a-dedicated-domain)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
  - [Affinity Construction](#affinity-construction)
  - [Topology](#topology)
  - [Where Injection Happens](#where-injection-happens)
  - [Interaction With LWS Exclusive Placement](#interaction-with-lws-exclusive-placement)
  - [Behavior Without Gang Scheduling](#behavior-without-gang-scheduling)
  - [Accelerator Portability](#accelerator-portability)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Two independent strictness knobs](#alternative-1-two-independent-strictness-knobs)
  - [Alternative 2: Reuse the LWS exclusive-topology annotation](#alternative-2-reuse-the-lws-exclusive-topology-annotation)
  - [Alternative 3: A mutating webhook instead of controller injection](#alternative-3-a-mutating-webhook-instead-of-controller-injection)
<!-- /toc -->

## Summary

This KEP adds a placement policy to the [DisaggregatedSet](/keps/766-DisaggregatedSet) API. It co-locates all of a slice's roles within a single topology domain (e.g. an NVL72 rack) and spreads a DisaggregatedSet's slices across domains, by having the controller inject pod affinity and anti-affinity into the underlying LeaderWorkerSet pod templates.

Placement is expressed with a single `PlacementPolicy.Type` field with three values: `None`, `ExclusiveSlice`, and `ExclusiveTopology`. `ExclusiveSlice` co-locates and spreads a DisaggregatedSet's own slices while letting other DisaggregatedSets share a domain. `ExclusiveTopology` additionally makes each domain exclusive to one slice across all DisaggregatedSets (a 1:1 domain to slice mapping). The mechanism is purely node-label based (a `topology` key the user supplies), so it is accelerator-agnostic. This builds directly on the slice identity introduced in [KEP-846](/keps/846-disaggregatedset-slices).

## Motivation

Disaggregated serving has placement needs that DisaggregatedSet cannot currently express:

1. **Locality.** A slice's prefill and decode pods hand off the KV cache to each other, so that transfer wants them in the same low-latency domain. Today the controller injects no topology placement, so a slice's roles land wherever the scheduler puts them, possibly in different domains.

2. **Isolation.** Operators want a DisaggregatedSet's slices spread across domains so that losing one domain takes down at most one slice. By default other DisaggregatedSets should still be able to share a domain for dense packing.

3. **Domain exclusivity.** Some deployments want each slice to own its domain outright, a 1:1 domain to slice mapping that excludes other DisaggregatedSets too, for stronger performance isolation.

### Goals

1. Co-locate all roles of a slice within a user-specified topology domain.
2. Spread a DisaggregatedSet's slices across domains, without preventing other DisaggregatedSets from sharing a domain.
3. Optionally make each domain exclusive to a single slice across all DisaggregatedSets.
4. Keep the mechanism hardware-agnostic, driven entirely by a node-label topology key, so it works for GPU domains, TPU domains, zones, racks, and so on.

### Non-Goals

1. **Gang scheduling / atomic whole-slice admission.** Guaranteeing that a whole slice can fit a domain before any of its pods are placed is a separate, larger effort. Every non-`None` option here is hard (required) affinity and depends on adequate capacity to avoid leaving pods Pending. Handling that is deferred to gang scheduling. See [Behavior Without Gang Scheduling](#behavior-without-gang-scheduling).
2. **Best-effort (soft) placement.** Every non-`None` option is a hard requirement. Soft, never-blocking variants are out of scope for this KEP.
3. **Cross-namespace or multi-cluster placement.**

## Proposal

Add an optional `PlacementPolicy` to `DisaggregatedSetSpec` with a single type and a topology key:

- `Type` is one of `None`, `ExclusiveSlice`, or `ExclusiveTopology`.
- `Topology` is the node-label key that defines a domain.

The three types:

- `None`: inject nothing (today's behavior).
- `ExclusiveSlice`: co-locate each slice's roles in one domain and spread this DisaggregatedSet's slices across domains. Other DisaggregatedSets may share a domain.
- `ExclusiveTopology`: everything `ExclusiveSlice` does, plus the domain is exclusive to one slice across all DisaggregatedSets (a 1:1 domain to slice mapping).

The controller translates the type into pod affinity (co-location) and pod anti-affinity (spread, and cross-DisaggregatedSet exclusion for `ExclusiveTopology`) terms and injects them into the LeaderWorkerSet pod templates it already manages, keyed on the slice and DisaggregatedSet-name labels from [KEP-846](/keps/846-disaggregatedset-slices).

### User Stories

#### Story 1: Co-locate and spread a DisaggregatedSet's slices

An operator runs prefill and decode roles and needs each slice's pods in the same NVL72 rack so the KV-cache transfer stays in-domain, and wants a rack failure to take down at most one slice. They set `type: ExclusiveSlice` with `topology` set to the rack label. Each slice's roles co-locate in one rack and the DisaggregatedSet's slices land on different racks, while other DisaggregatedSets can still pack onto those racks.

#### Story 2: Give each slice a dedicated domain

An operator wants each slice to own its rack outright, with no pods from any other slice (including other DisaggregatedSets) sharing it, for strict isolation. They set `type: ExclusiveTopology` with `topology` set to the rack label. Each slice co-locates in a rack that holds only that slice.

### Notes/Constraints/Caveats

- `topology` is a node-label key the operator provides. The nodes must actually carry that label or the affinity can never be satisfied.
- "Slice" here means a copy of the whole role topology (from KEP-846). It is unrelated to a "TPU slice" (a set of TPU chips or hosts) despite the shared word. For TPUs, `topology` simply points at whichever node label marks the domain you want.
- Every non-`None` option is hard (required) affinity, so it can leave pods Pending under contention because nothing admits a whole slice atomically. See [Behavior Without Gang Scheduling](#behavior-without-gang-scheduling).

### Risks and Mitigations

**Risk**: Hard placement can wedge a slice or leave pods Pending. With co-location, the scheduler places pods one at a time with no whole-slice look-ahead, so a slice's first pod can land in a domain that lacks room for the rest. With spread (and especially `ExclusiveTopology`), a slice can stay Pending when no eligible domain is free.

**Mitigation**: This is an accepted tradeoff for options whose whole purpose is a hard guarantee. There is no soft fallback in this KEP, so operators must provision enough capacity and domains. Gang scheduling (a separate, future effort) is what makes hard placement safe under contention, and handling partial scheduling is deferred to it.

**Risk**: A role that also carries the LWS `exclusive-topology` annotation conflicts with DisaggregatedSet placement, which co-locates a slice's roles. Group-exclusivity pins each group to its own domain, which cannot satisfy slice-level co-location, so the slice never schedules.

**Mitigation**: Admission validation will reject a non-`None` `PlacementPolicy` together with the `exclusive-topology` annotation on a role. See [Interaction With LWS Exclusive Placement](#interaction-with-lws-exclusive-placement).

## Design Details

### API

```go
// DisaggregatedSetSpec defines the desired state of DisaggregatedSet
type DisaggregatedSetSpec struct {
    Roles  []DisaggregatedRoleSpec `json:"roles"`
    Slices *int32                  `json:"slices,omitempty"` // from KEP-846

    // PlacementPolicy controls how a slice's roles are co-located and how the
    // DisaggregatedSet's slices are spread across topology domains.
    // +optional
    PlacementPolicy *PlacementPolicy `json:"placementPolicy,omitempty"`
}

type PlacementPolicy struct {
    // Type selects the placement guarantee. Defaults to None.
    // +kubebuilder:validation:Enum={None,ExclusiveSlice,ExclusiveTopology}
    // +kubebuilder:default=None
    // +optional
    Type PlacementType `json:"type,omitempty"`

    // Topology is the node-label key that defines a domain. Required when Type is not None.
    // +optional
    Topology string `json:"topology,omitempty"`
}

// PlacementType selects the DisaggregatedSet placement guarantee.
type PlacementType string

const (
    // PlacementNone injects no affinity.
    PlacementNone PlacementType = "None"
    // PlacementExclusiveSlice co-locates a slice's roles in one domain and spreads this
    // DisaggregatedSet's slices across domains. Other DisaggregatedSets may share a domain.
    PlacementExclusiveSlice PlacementType = "ExclusiveSlice"
    // PlacementExclusiveTopology is ExclusiveSlice plus domain exclusivity: a domain holds
    // at most one slice across all DisaggregatedSets (a 1:1 domain to slice mapping).
    PlacementExclusiveTopology PlacementType = "ExclusiveTopology"
)
```

Validation:
- `Topology` is required when `Type` is not `None`.
- A `Type` other than `None` may not be combined with the LWS `exclusive-topology` annotation on a role (see [Interaction With LWS Exclusive Placement](#interaction-with-lws-exclusive-placement)).

### Affinity Construction

The controller keys all terms off the labels already applied to managed pods by KEP-846: `disaggregatedset.x-k8s.io/name` (the DisaggregatedSet) and `disaggregatedset.x-k8s.io/slice` (the slice index). No new label is required. Every injected term is `RequiredDuringSchedulingIgnoredDuringExecution`.

**`ExclusiveSlice`** injects two terms on `topology`:

- podAffinity (co-locate this slice's roles): selector `name In [<ds>]` AND `slice In [<slice>]`. This pulls all roles of the slice into one domain. It is self-referential, so the first pod of a slice is not pinned to a particular domain (required podAffinity with no matching pods yet is satisfied) and later pods are drawn to its domain.
- podAntiAffinity (spread this DisaggregatedSet's slices): selector `name In [<ds>]` AND `slice NotIn [<slice>]`. This repels only same-DisaggregatedSet, different-slice pods, so other DisaggregatedSets (different `name`) are not matched and may share the domain.

**`ExclusiveTopology`** injects the two `ExclusiveSlice` terms plus one more podAntiAffinity term on `topology`:

- podAntiAffinity (exclude other DisaggregatedSets' slices): selector `name NotIn [<ds>]` AND `slice Exists`. This repels any pod that belongs to a different DisaggregatedSet's slice.

Required podAntiAffinity terms are evaluated together, so a domain must be free of every selected pod. With both anti-affinity terms, the domain ends up free of any other slice, same-DisaggregatedSet or not, which is the 1:1 domain to slice guarantee.

### Topology

`Topology` is a node-label key (the same concept as a Kubernetes affinity `topologyKey`). The controller copies it verbatim into the `topologyKey` of every injected term, so the operator chooses the domain boundary: a per-zone label, a per-node label, a cloud node-pool label, a custom NVL72 domain label, a TPU domain label, and so on. It must name a label the nodes actually carry.

### Where Injection Happens

The controller injects the affinity into the LeaderWorkerSet leader and worker pod templates at creation time, the same place it already injects the DisaggregatedSet labels. Injection needs no new mutating webhook, is deterministic, and rides on the existing template-construction path. The placement validation uses the DisaggregatedSet's existing validating webhook. The pods carry the `name` and `slice` labels (already injected), so the selectors resolve correctly.

### Interaction With LWS Exclusive Placement

LWS has its own exclusive-placement feature (the `leaderworkerset.sigs.k8s.io/exclusive-topology` annotation), which operates at the **group** granularity: one leader-worker group per domain, exclusive to that group. DisaggregatedSet placement operates one level up, at the **slice** granularity, and `ExclusiveTopology` is the slice-level analog (one slice per domain). The two conflict when applied to the same role at the same topology level, because both DisaggregatedSet options co-locate a slice's roles while group-exclusivity wants each group apart, so the slice never schedules. They can still compose at *different* levels (for example a slice per rack via DisaggregatedSet placement and a group per host via the LWS annotation). The controller never sets the annotation itself, and admission validation will reject a non-`None` `PlacementPolicy` together with the `exclusive-topology` annotation on a role.

### Behavior Without Gang Scheduling

The default scheduler places pods one at a time with no whole-slice look-ahead, and nothing admits a slice atomically. Because every non-`None` option is hard:

- Co-location can wedge a single slice: if the slice's first pod is scheduled into a domain that lacks room for the remaining pods, those pods are pinned to that domain and stay Pending until it frees up. This does not require multiple DisaggregatedSets. One slice plus an unlucky first-pod placement (or any other workload occupying the domain) is enough.
- Spread, and especially `ExclusiveTopology`, can leave a slice Pending when no eligible domain is free.

There is no soft fallback in this KEP. Choosing a domain that fits the entire slice up front, and recovering cleanly under contention, requires gang or atomic scheduling, which is a separate future effort this design defers to.

### Accelerator Portability

The entire mechanism is pod affinity and anti-affinity over a node-label `topologyKey`, and it contains no GPU- or TPU-specific logic. It works for any accelerator (or none) as long as the domain's nodes share a consistent label and `topology` names that label. For TPUs, point `topology` at the node label that marks the TPU domain you want to co-locate within. Managed TPU node pools usually apply topology labels automatically, while custom domains require the cluster to label nodes.

### Test Plan

[X] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Unit tests

- Affinity construction: `None` injects nothing, `ExclusiveSlice` injects the co-location podAffinity and the same-DisaggregatedSet spread podAntiAffinity, and `ExclusiveTopology` adds the cross-DisaggregatedSet exclusion podAntiAffinity. Each term has the right `topologyKey` and label selectors and is required.
- Injection: terms are written into both leader and worker pod templates, and existing affinity on the template is preserved.
- Validation: `topology` is required when `Type` is not `None`, and a non-`None` type combined with the LWS `exclusive-topology` annotation is rejected.

#### Integration tests

- `ExclusiveSlice`: a slice's pods co-locate in one domain, this DisaggregatedSet's slices occupy disjoint domains, and a second DisaggregatedSet may share a domain.
- `ExclusiveTopology`: a slice's domain holds only that slice, excluding a second DisaggregatedSet's slices.
- Placement is keyed correctly across roles and across a rolling update (revisions of the same slice co-locate and spread consistently).

### Graduation Criteria

Placement policy graduates together with DisaggregatedSet slices ([KEP-846](/keps/846-disaggregatedset-slices)).

**Alpha**:
- `PlacementPolicy` with `Type` and `Topology`, plus validation.
- Controller-side affinity injection into LeaderWorkerSet pod templates.
- Unit and integration test coverage.
- Documentation and a sample manifest.

**Beta / Stable**: incorporate production feedback, and revisit making hard placement safe under contention once gang scheduling exists.

## Implementation History

- 2026-06-29: Initial KEP draft.
- 2026-07-01: Consolidated to a single PlacementPolicy.Type enum.

## Drawbacks

1. **Hard-only placement can wedge or leave pods Pending** without adequate capacity, domains, or gang scheduling. There is no best-effort option in this KEP, so the sharp edge is unavoidable for users who need the guarantees.
2. **Added API surface** and scheduling behavior to document, test, and support.

## Alternatives

### Alternative 1: Two independent strictness knobs

Model placement as two fields, `RoleColocation` and `SliceSpread`, each `None`, `Preferred`, or `Required` (nine combinations).

**Rejected because** it exposes far more surface than the use cases need, the real cases collapse to a few named bundles, and, most importantly, a same-DisaggregatedSet-scoped spread cannot express `ExclusiveTopology` (a domain exclusive to one slice across all DisaggregatedSets, the 1:1 domain to slice requirement). A single typed enum covers the needed cases and adds the cross-DisaggregatedSet exclusion. Best-effort variants are deferred (see Non-Goals).

### Alternative 2: Reuse the LWS exclusive-topology annotation

Have the DisaggregatedSet set LWS's `exclusive-topology` annotation on the roles instead of injecting its own affinity.

**Rejected because** that annotation works at the group granularity and is globally exclusive (one group per domain), which is the wrong granularity (we want all of a slice's groups together) and, for `ExclusiveSlice`, the wrong exclusivity (we want other DisaggregatedSets to be able to share). It also cannot express same-DisaggregatedSet-only spread. The DisaggregatedSet therefore needs its own slice-level affinity.

### Alternative 3: A mutating webhook instead of controller injection

Inject the affinity via a pod mutating webhook keyed off an annotation, mirroring the LWS pod webhook.

**Rejected because** the DisaggregatedSet controller already constructs and mutates the LeaderWorkerSet pod templates (it injects the slice/role/name labels there), so adding the affinity in the same path is simpler and deterministic, with no extra webhook in the admission path.
