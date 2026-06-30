# KEP-848: DisaggregatedSet Placement Policy

<!--
This KEP proposes adding a placement policy to DisaggregatedSet that co-locates a slice's roles within a topology domain and/or spreads a DisaggregatedSet's slices across domains, via injected pod affinity/anti-affinity.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Co-locate a slice for KV-cache handoff](#story-1-co-locate-a-slice-for-kv-cache-handoff)
    - [Story 2: Isolate slices for fault tolerance](#story-2-isolate-slices-for-fault-tolerance)
    - [Story 3: Best-effort on a contended cluster](#story-3-best-effort-on-a-contended-cluster)
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
  - [Alternative 1: A single strictness ladder](#alternative-1-a-single-strictness-ladder)
  - [Alternative 2: Reuse the LWS exclusive-topology annotation](#alternative-2-reuse-the-lws-exclusive-topology-annotation)
  - [Alternative 3: A mutating webhook instead of controller injection](#alternative-3-a-mutating-webhook-instead-of-controller-injection)
<!-- /toc -->

## Summary

This KEP adds a placement policy to the [DisaggregatedSet](/keps/766-DisaggregatedSet) API. It lets a user (1) co-locate all of a slice's roles within a single topology domain (e.g. an NVL72 rack) and (2) spread a DisaggregatedSet's slices across domains, by having the controller inject pod affinity and anti-affinity into the underlying LeaderWorkerSet pod templates.

Placement is expressed with two independent knobs, `RoleColocation` and `SliceSpread`, each set to `None`, `Preferred`, or `Required`. The mechanism is purely node-label based (a `topology` key the user supplies), so it is accelerator-agnostic. This builds directly on the slice identity introduced in [KEP-846](/keps/846-disaggregatedset-slices).

## Motivation

Disaggregated serving has two placement needs that DisaggregatedSet cannot currently express:

1. **Locality.** A slice's prefill and decode pods hand off the KV cache to each other; that transfer wants them in the same low-latency domain. Today the DisaggregatedSet controller injects no topology placement, so a slice's roles land wherever the scheduler puts them, possibly in different domains.

2. **Isolation.** Operators want a DisaggregatedSet's slices spread across domains so that losing one domain takes down at most one slice, while still allowing *different* DisaggregatedSets to share a domain for dense packing.

### Goals

1. Co-locate all roles of a slice within a user-specified topology domain, either best-effort or as a hard requirement.
2. Spread a DisaggregatedSet's slices across domains, either best-effort or as a hard requirement, without preventing *different* DisaggregatedSets from sharing a domain.
3. Make the two controls independent, so an operator can, for example, require isolation while keeping co-location best-effort.
4. Keep the mechanism hardware-agnostic — driven entirely by a node-label topology key — so it works for GPU domains, TPU domains, zones, racks, etc.

### Non-Goals

1. **Gang scheduling / atomic whole-slice admission.** Guaranteeing that a whole slice can fit a domain before any of its pods are placed is a separate, larger effort. The `Required` settings here depend on adequate capacity (or, later, gang scheduling) to avoid leaving pods Pending; see [Behavior Without Gang Scheduling](#behavior-without-gang-scheduling).
2. **Cross-namespace or multi-cluster placement.**

## Proposal

Add an optional `PlacementPolicy` to `DisaggregatedSetSpec` with two independent strictness knobs and a topology key:

- `RoleColocation` ∈ `{None, Preferred, Required}` — co-locate a slice's roles in one domain.
- `SliceSpread` ∈ `{None, Preferred, Required}` — place this DisaggregatedSet's slices on different domains.
- `Topology` — the node-label key that defines a domain.

The DisaggregatedSet controller translates these into pod affinity (`RoleColocation`) and pod anti-affinity (`SliceSpread`) terms and injects them into the LeaderWorkerSet pod templates it already manages, keyed on the slice and DisaggregatedSet-name labels from [KEP-846](/keps/846-disaggregatedset-slices).

### User Stories

#### Story 1: Co-locate a slice for KV-cache handoff

An operator runs prefill and decode roles and needs each slice's prefill and decode pods in the same NVL72 rack so the KV-cache transfer stays in-domain. They set `roleColocation: Required` with `topology` set to the rack label. Every slice's pods are then required to schedule into one rack.

#### Story 2: Isolate slices for fault tolerance

An operator wants a rack failure to take down at most one slice, but is willing to let a slice's pods spill across racks if a single rack can't hold the whole slice. They set `sliceSpread: Required` and `roleColocation: Preferred`. Slices are guaranteed to occupy disjoint racks (a rack hosts pods from only one of this DisaggregatedSet's slices), while co-location remains best-effort. A single slice may take up multiple racks if necessary, but no other slices from that DS will use those racks.

#### Story 3: Best-effort on a contended cluster

An operator on a busy shared cluster wants locality and isolation when possible but never wants a slice stuck Pending. They set both `roleColocation: Preferred` and `sliceSpread: Preferred`. The scheduler honors the preferences when it can and places pods anyway when it cannot.

### Notes/Constraints/Caveats

- `topology` is a node-label key the operator provides; the nodes must actually carry that label or the affinity can never be satisfied.
- "Slice" here means a copy of the whole role topology (from KEP-846). It is unrelated to a "TPU slice" (a set of TPU chips/hosts) despite the shared word; for TPUs, `topology` simply points at whichever node label marks the domain you want.
- `Required` settings can leave pods Pending under contention because nothing admits a whole slice atomically; see [Behavior Without Gang Scheduling](#behavior-without-gang-scheduling).

### Risks and Mitigations

**Risk**: `RoleColocation: Required` can wedge a slice. The scheduler places pods one at a time with no whole-slice look-ahead, so a slice's first pod can land in a domain that lacks room for the rest, pinning the remaining pods there until that domain frees up.

**Mitigation**: This is opt-in (only at `Required`), and it is the same risk LWS already accepts for its group-level co-location. `Preferred` never wedges and is the recommended setting until capacity is guaranteed. Gang scheduling (a separate, future effort) removes the risk entirely.

**Risk**: `SliceSpread: Required` can leave a slice Pending when no domain free of this DisaggregatedSet's other slices is available (e.g. more slices than domains).

**Mitigation**: Opt-in; `Preferred` degrades gracefully (best-effort isolation that never blocks).

**Risk**: A role that also carries the LWS `exclusive-topology` annotation conflicts with `RoleColocation` at the same topology level. Group-exclusivity pins each group to its own domain, which cannot satisfy slice-level co-location, so the slice never schedules.

**Mitigation**: Admission validation will reject setting `RoleColocation` together with the annotation on a role. See [Interaction With LWS Exclusive Placement](#interaction-with-lws-exclusive-placement).

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
    // RoleColocation controls placing all of a slice's roles in one topology domain.
    // +kubebuilder:validation:Enum={None,Preferred,Required}
    // +kubebuilder:default=None
    // +optional
    RoleColocation PlacementStrictness `json:"roleColocation,omitempty"`

    // SliceSpread controls placing this DisaggregatedSet's slices on different domains.
    // +kubebuilder:validation:Enum={None,Preferred,Required}
    // +kubebuilder:default=None
    // +optional
    SliceSpread PlacementStrictness `json:"sliceSpread,omitempty"`

    // Topology is the node-label key that defines a domain. Required when
    // RoleColocation or SliceSpread is not None.
    // +optional
    Topology string `json:"topology,omitempty"`
}

// PlacementStrictness selects how strongly a placement preference is enforced.
type PlacementStrictness string

const (
    // PlacementNone injects nothing.
    PlacementNone PlacementStrictness = "None"
    // PlacementPreferred injects best-effort (preferred) affinity that never blocks scheduling.
    PlacementPreferred PlacementStrictness = "Preferred"
    // PlacementRequired injects hard (required) affinity that can leave pods Pending.
    PlacementRequired PlacementStrictness = "Required"
)
```

The two knobs are fully orthogonal: all nine combinations are valid, and `None`/`None` (the default) reproduces today's behavior of injecting nothing.

Validation:
- `Topology` is required when either knob is `Preferred` or `Required`.
- No knob combination is forbidden. Even unusual ones are coherent: for example `RoleColocation: None` with `SliceSpread: Required` isolates slices onto disjoint domains without co-locating each slice's roles.
- `RoleColocation` may not be combined with the LWS `exclusive-topology` annotation on a role (see [Interaction With LWS Exclusive Placement](#interaction-with-lws-exclusive-placement)).

### Affinity Construction

The controller keys both terms off the labels already applied to managed pods by KEP-846 — `disaggregatedset.x-k8s.io/name` (the DisaggregatedSet) and `disaggregatedset.x-k8s.io/slice` (the slice index). No new label is required.

- **`RoleColocation`** → **podAffinity** selecting this slice's own pods, on `topology`:
  - selector: `name In [<ds>]` AND `slice In [<slice>]`
  - This pulls all roles of the slice into one domain. It is self-referential, so the first pod of a slice is not pinned to a particular domain (required podAffinity with no matching pods yet is satisfied) and later pods are drawn to its domain.

- **`SliceSpread`** → **podAntiAffinity** selecting this DisaggregatedSet's *other* slices, on `topology`:
  - selector: `name In [<ds>]` AND `slice NotIn [<slice>]`
  - This repels only same-DisaggregatedSet, different-slice pods, so the DisaggregatedSet's slices avoid each other while pods of *other* DisaggregatedSets (different `name`) are not matched and may share the domain.

`Preferred` emits the term under `PreferredDuringSchedulingIgnoredDuringExecution` (with a weight); `Required` emits it under `RequiredDuringSchedulingIgnoredDuringExecution`. As with all pod affinity, enforcement is at scheduling time only ("IgnoredDuringExecution").

### Topology

`Topology` is a node-label key (the same concept as a Kubernetes affinity `topologyKey`). The controller copies it verbatim into the `topologyKey` of every injected term, so the operator chooses the domain boundary: a per-zone label, a per-node label, a cloud node-pool label, a custom NVL72 domain label, a TPU domain label, etc. It must name a label the nodes actually carry.

### Where Injection Happens

The controller injects the affinity into the LeaderWorkerSet leader and worker pod templates at creation time — the same place it already injects the DisaggregatedSet labels. Injection needs no new mutating webhook, is deterministic, and rides on the existing template-construction path. The `exclusive-topology` validation uses the DisaggregatedSet's existing validating webhook. The pods carry the `name` and `slice` labels (already injected), so the selectors resolve correctly.

### Interaction With LWS Exclusive Placement

LWS has its own exclusive-placement feature (the `leaderworkerset.sigs.k8s.io/exclusive-topology` annotation), which operates at the **group** granularity: one leader-worker group per domain, exclusive to that group. DisaggregatedSet placement operates one level up, at the **slice** granularity. The only real conflict is `RoleColocation` against group-exclusivity at the same topology level: co-location wants a slice's groups together while group-exclusivity wants each apart, leaving the slice unschedulable. `SliceSpread` does not conflict, since group-exclusivity already spreads, and the two compose at *different* levels (for example slice per rack, group per host). The controller never sets the annotation itself, and admission validation will reject setting `RoleColocation` together with the `exclusive-topology` annotation on a role.

### Behavior Without Gang Scheduling

The default scheduler places pods one at a time with no whole-slice look-ahead, and nothing admits a slice atomically. Consequently:

- A `Required` co-location can wedge a single slice: if the slice's first pod is scheduled into a domain that lacks room for the remaining pods, those pods are pinned to that domain and stay Pending until it frees up. This does not require multiple DisaggregatedSets; one slice plus an unlucky first-pod placement (or any other workload occupying the domain) is enough. Choosing a domain that fits the *entire* slice up front requires gang/atomic scheduling.
- A `Required` spread can leave a slice Pending if no domain free of this DisaggregatedSet's other slices is available.

`Preferred` settings never block — they degrade to best-effort. Gang scheduling is the follow-up that makes the `Required` settings safe under contention (and would let a future design harden spread further). It is intentionally out of scope here.

### Accelerator Portability

The entire mechanism is pod (anti-)affinity over a node-label `topologyKey`; it contains no GPU- or TPU-specific logic. It works for any accelerator (or none) as long as the domain's nodes share a consistent label and `topology` names that label. For TPUs, point `topology` at the node label that marks the TPU domain you want to co-locate within (managed TPU node pools likely apply topology labels automatically; custom domains would require the cluster to label nodes).

### Test Plan

[X] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Unit tests

- Affinity construction: for each of `None`/`Preferred`/`Required`, the correct podAffinity/podAntiAffinity term (or none) is produced, with the right `topologyKey` and label selectors.
- Selectors: co-location selects `name In [ds] AND slice In [s]`; spread selects `name In [ds] AND slice NotIn [s]`.
- Injection: terms are written into both leader and worker pod templates; existing affinity on the template is preserved.
- Validation: `topology` required when a knob is non-`None`; all nine knob combinations accepted.

#### Integration tests

- `RoleColocation: Required` → a slice's pods land in a single domain.
- `SliceSpread: Required` → the DisaggregatedSet's slices occupy disjoint domains, while a second DisaggregatedSet may share a domain.
- `Preferred` settings never block scheduling.
- Placement is keyed correctly across roles and across a rolling update (revisions of the same slice co-locate/spread consistently).

### Graduation Criteria

Placement policy graduates together with DisaggregatedSet slices ([KEP-846](/keps/846-disaggregatedset-slices)).

**Alpha**:
- `PlacementPolicy` with `RoleColocation`, `SliceSpread`, and `Topology`, plus validation.
- Controller-side affinity injection into LeaderWorkerSet pod templates.
- Unit and integration test coverage.
- Documentation and a sample manifest.

**Beta / Stable**: incorporate production feedback, revisit hardening `Required` placement once gang scheduling exists.

## Implementation History

- 2026-06-29: Initial KEP draft.

## Drawbacks

1. **`Required` settings can wedge or leave pods Pending** without adequate capacity or gang scheduling, which is a sharp edge for users who reach for them without understanding the scheduling implications.
2. **Added API surface** and scheduling behavior to document, test, and support.

## Alternatives

### Alternative 1: A single strictness ladder

Model placement as one enum — e.g. `Soft` (soft co-location + soft spread), `RequireColocation` (hard co-location + soft spread), `RequireColocationAndSpread` (both hard).

**Rejected because** a single monotonic ladder cannot express co-location and spread at *different* strictnesses in the inverted direction — specifically "best-effort co-location + required spread," which is a real isolation-first use case (guarantee that a domain failure affects at most one slice, while tolerating a slice spilling across domains). Two independent `None`/`Preferred`/`Required` knobs express every coherent combination, including that one, and map directly onto Kubernetes affinity.

### Alternative 2: Reuse the LWS exclusive-topology annotation

Have the DisaggregatedSet set LWS's `exclusive-topology` annotation on the roles instead of injecting its own affinity.

**Rejected because** that annotation works at the group granularity and is globally exclusive (one group per domain), which is both the wrong granularity (we want all of a slice's groups together) and the wrong exclusivity (we want different DisaggregatedSets to be able to share). It also cannot express same-DisaggregatedSet-only spread. The DisaggregatedSet therefore needs its own slice-level affinity.

### Alternative 3: A mutating webhook instead of controller injection

Inject the affinity via a pod mutating webhook keyed off an annotation, mirroring the LWS pod webhook.

**Rejected because** the DisaggregatedSet controller already constructs and mutates the LeaderWorkerSet pod templates (it injects the slice/role/name labels there), so adding the affinity in the same path is simpler and deterministic, with no extra webhook in the admission path.
