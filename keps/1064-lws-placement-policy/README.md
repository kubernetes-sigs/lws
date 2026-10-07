# KEP-1064: LeaderWorkerSet Placement Policy

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: One replica per domain](#story-1-one-replica-per-domain)
    - [Story 2: One subgroup per domain](#story-2-one-subgroup-per-domain)
    - [Story 3: Moving off the annotations](#story-3-moving-off-the-annotations)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
  - [Semantics](#semantics)
  - [How the Field Is Applied](#how-the-field-is-applied)
  - [Validation](#validation)
  - [Migration From the Annotations](#migration-from-the-annotations)
  - [Interaction With Workload-Aware Scheduling](#interaction-with-workload-aware-scheduling)
  - [Interaction With DisaggregatedSet](#interaction-with-disaggregatedset)
  - [Interaction With Kueue Topology-Aware Scheduling](#interaction-with-kueue-topology-aware-scheduling)
  - [Update Semantics](#update-semantics)
  - [Open Questions](#open-questions)
  - [Test Plan](#test-plan)
      - [Prerequisite testing updates](#prerequisite-testing-updates)
      - [Unit tests](#unit-tests)
      - [Integration tests](#integration-tests)
      - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Reuse the DisaggregatedSet type](#alternative-1-reuse-the-disaggregatedset-type)
  - [Alternative 2: Express exclusivity through <code>spec.scheduling</code>](#alternative-2-express-exclusivity-through-)
  - [Alternative 3: Keep only the annotations](#alternative-3-keep-only-the-annotations)
  - [Alternative 4: Wait for exclusive placement in kube-scheduler](#alternative-4-wait-for-exclusive-placement-in-kube-scheduler)
<!-- /toc -->

## Summary

This KEP graduates LeaderWorkerSet exclusive placement from the
`leaderworkerset.sigs.k8s.io/exclusive-topology` and
`leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology` annotations to typed
fields: `spec.leaderWorkerTemplate.placementPolicy` for replica-level placement
and `spec.leaderWorkerTemplate.subGroupPolicy.placementPolicy` for subgroup-level
placement.

The fields keep the annotations' semantics exactly. They follow the shape of the
DisaggregatedSet placement policy from [KEP-848](/keps/848-disaggregatedset-placement-policy),
a `type` plus a node-label `topology` key, and they are mutually exclusive with
workload-aware scheduling (WAS) constraints in `spec.scheduling`, as the
annotation is today under [KEP-666](/keps/666-gang-scheduling-in-lws).

## Motivation

Exclusive placement is a feature LWS users depend on, but it is configured
through annotations:

1. **No schema validation.** Annotations bypass OpenAPI validation, so a
   malformed topology key is accepted and only fails later, when the API server
   rejects the leader pods whose injected affinity uses it.
2. **Inconsistent with DisaggregatedSet.** DisaggregatedSet already exposes a
   typed `spec.placementPolicy` (KEP-848). LWS should express the same kind of
   constraint the same way.
3. **Ergonomics.** Replica-level and subgroup-level placement are easier to read
   and review as fields next to the template and the subgroup policy they apply
   to.

Workload-aware scheduling does not cover this today. KEP-5732 keeps a PodGroup
inside one topology domain but does not keep other PodGroups out of it, so it
cannot express exclusivity. That gap is raised with WG Workload-Aware Scheduling
in kubernetes/kubernetes#142690. The annotation's semantics are effectively a v1
feature of LWS that cannot be dropped, so LWS needs a typed API for them now
rather than waiting on that discussion.

### Goals

1. Typed, validated fields for replica-level and subgroup-level exclusive
   placement, with the same semantics as the annotations.
2. Reject malformed topology keys and invalid combinations at admission.
3. Keep existing manifests that use the annotations working, with a clear
   migration path.
4. Keep the KEP-666 WAS exclusion and the DisaggregatedSet placement conflict
   check correct for both the field and the annotations.

### Non-Goals

1. Changing what exclusive placement means. The scope stays as it is today.
2. Combining exclusive placement with WAS gang scheduling or topology
   constraints. This stays rejected, as KEP-666 validation rule 10 does for the
   annotation.
3. Exclusive placement in kube-scheduler. That is the subject of
   kubernetes/kubernetes#142690.
4. Best-effort (preferred) placement. Like the annotations, the fields are hard
   constraints.
5. Removing the annotations. See [Open Questions](#open-questions).
6. Integrating the policy with Kueue topology-aware scheduling. The two stay
   alternatives; see
   [Interaction With Kueue Topology-Aware Scheduling](#interaction-with-kueue-topology-aware-scheduling).

## Proposal

Add an optional `placementPolicy` to `LeaderWorkerTemplate` and to
`SubGroupPolicy`, each with:

- `type`: `None` (default) or `ExclusiveTopology`.
- `topology`: the node-label key that defines a domain. Required when `type` is
  `ExclusiveTopology`.

`ExclusiveTopology` on `leaderWorkerTemplate` behaves like the `exclusive-topology`
annotation, and on `subGroupPolicy` like the `subgroup-exclusive-topology`
annotation. The controller applies the field through the same pod-template
annotations it propagates today, so the pod webhook and the pods it produces do
not change.

### User Stories

#### Story 1: One replica per domain

An operator serves a model whose leader and workers must share an NVLink domain,
with one replica per domain. Today they set `exclusive-topology` to the domain's
node label. With this KEP they set
`placementPolicy: {type: ExclusiveTopology, topology: <label>}` instead, and a
malformed label key is rejected when the LeaderWorkerSet is applied, instead of
surfacing later as failed pod creation.

#### Story 2: One subgroup per domain

A multi-host workload splits each replica into subgroups with `subGroupSize`, and
each subgroup must own a domain such as a node pool. The operator sets
`subGroupPolicy.placementPolicy` instead of the `subgroup-exclusive-topology`
annotation.

#### Story 3: Moving off the annotations

An operator with existing manifests keeps them unchanged after upgrading. LWS
warns that the annotations are deprecated, and the operator moves each manifest
to the field on their own schedule.

### Notes/Constraints/Caveats

- `ExclusiveTopology` is a hard constraint, as the annotation is. Without enough
  free domains, pods stay Pending.
- At replica level only the leader carries the affinity terms. The worker
  StatefulSet is created once the leader is scheduled, and workers follow the
  leader's domain through a node selector. This KEP keeps that behavior.
- A well-formed topology key that no node carries still leaves pods Pending.
  Admission validation cannot check node labels.

### Risks and Mitigations

**Risk**: During migration the same constraint can be written twice, in the field
and in an annotation, with different values.

**Mitigation**: Admission rejects conflicting values. See
[Open Questions](#open-questions) for the alternative of letting the field take
precedence.

**Risk**: An automatic annotation-to-field migration in a mutating webhook would
show up as drift in GitOps tools, because the live object no longer matches the
manifest.

**Mitigation**: Automatic migration is not part of alpha. See
[Migration From the Annotations](#migration-from-the-annotations).

**Risk**: The field is part of the template revision and the annotation is not, so
moving an existing annotation value into the field creates a new revision and
replaces every group once, even though placement does not change.

**Mitigation**: The migration docs recommend making the move together with the
next planned template change, which rolls the groups anyway. See
[Update Semantics](#update-semantics) for the alternative that avoids this.

**Risk**: WAS later adds exclusive placement with different semantics, leaving
LWS with two APIs for one concept.

**Mitigation**: The fields are mutually exclusive with WAS constraints, so no
object uses both. A follow-up can map `ExclusiveTopology` onto WAS once
kubernetes/kubernetes#142690 settles.

## Design Details

### API

```go
// LeaderWorkerSetPlacementPolicyType selects the placement guarantee.
// +kubebuilder:validation:Enum=None;ExclusiveTopology
type LeaderWorkerSetPlacementPolicyType string

const (
	// PlacementPolicyNone injects no placement affinity. This is the default.
	PlacementPolicyNone LeaderWorkerSetPlacementPolicyType = "None"
	// PlacementPolicyExclusiveTopology places all pods of the scope (a replica or
	// a subgroup) in one topology domain that no other LeaderWorkerSet replica or
	// subgroup in the namespace occupies.
	PlacementPolicyExclusiveTopology LeaderWorkerSetPlacementPolicyType = "ExclusiveTopology"
)

// LeaderWorkerSetPlacementPolicy configures topology placement for a replica or
// a subgroup.
type LeaderWorkerSetPlacementPolicy struct {
	// Type selects the placement guarantee. Defaults to None.
	// +optional
	// +kubebuilder:default=None
	Type LeaderWorkerSetPlacementPolicyType `json:"type,omitempty"`

	// Topology is the node-label key that defines a domain. Required when Type
	// is ExclusiveTopology.
	// +optional
	Topology string `json:"topology,omitempty"`
}

type LeaderWorkerTemplate struct {
	// ... existing fields

	// PlacementPolicy configures topology placement for each replica.
	// +optional
	PlacementPolicy *LeaderWorkerSetPlacementPolicy `json:"placementPolicy,omitempty"`
}

type SubGroupPolicy struct {
	// ... existing fields

	// PlacementPolicy configures topology placement for each subgroup.
	// +optional
	PlacementPolicy *LeaderWorkerSetPlacementPolicy `json:"placementPolicy,omitempty"`
}
```

The fields are added to `leaderworkerset.x-k8s.io/v1`.

### Semantics

`ExclusiveTopology` means exactly what the matching annotation means today:

- **Replica level**: required pod affinity to the replica's own pods and required
  pod anti-affinity to pods of any other LeaderWorkerSet replica in the same
  namespace, both keyed on `topology`.
- **Subgroup level**: the same terms, scoped to subgroups.

Pods that do not carry LeaderWorkerSet labels can still share the domain unless
taints are used, as today.

### How the Field Is Applied

The controller resolves the effective topology key for each level, the field
first and the annotation second, and writes it into the same pod-template
annotations it propagates today: the leader pod template in the LeaderWorkerSet
controller and the worker StatefulSet template in the pod controller. The pod
webhook builds the affinity terms from those pod annotations and does not change.

Every other reader of the LeaderWorkerSet annotations moves to the same resolver:

- the replica-level path in the pod controller that waits for the leader to be
  scheduled and pins workers to its domain;
- the subgroup check in the LeaderWorkerSet webhook;
- KEP-666 validation rule 10 in `pkg/schedulerprovider/phase_one.go`;
- the DisaggregatedSet conflict check in `validatePlacement`.

### Validation

| Condition | Rule |
| --- | --- |
| `type: ExclusiveTopology` | `topology` must be a valid label key. |
| `type: None` or unset | `topology` must be empty. |
| Both levels set | The two `topology` keys must differ, because one domain cannot hold a replica exclusively and each of its subgroups exclusively. |
| Field and annotation both set for one level | Rejected if the values differ, including `type: None` with the annotation set. |
| Non-`None` policy at either level with WAS gang or topology constraints at the selected level | Rejected. Rule 10 does this for the replica-level annotation today; alpha applies it to the subgroup level too. |
| Non-`None` policy on a template that carries Kueue topology annotations | Admission warning. See [Interaction With Kueue Topology-Aware Scheduling](#interaction-with-kueue-topology-aware-scheduling). |

### Migration From the Annotations

| Phase | Behavior |
| --- | --- |
| Alpha | The field and the annotations are both accepted. Admission returns a warning when an annotation is used. Conflicting values are rejected. |
| Later | No automatic migration is planned. Removing the annotations is left to a later update of this KEP. |

### Interaction With Workload-Aware Scheduling

KEP-666 rejects WAS gang scheduling or topology constraints together with the
`exclusive-topology` annotation (validation rule 10). Both fields are rejected
under the same conditions; the subgroup level is included so that alpha starts
strict, since relaxing validation later is compatible and tightening it is not.
If WAS adds exclusive placement
(kubernetes/kubernetes#142690), a follow-up can map `ExclusiveTopology` onto it
and lift the restriction.

### Interaction With DisaggregatedSet

DisaggregatedSet roles embed `LeaderWorkerTemplate`, so the new fields become
settable on roles. `validatePlacement` already rejects a non-`None`
DisaggregatedSet `placementPolicy` together with either exclusive annotation on a
role, since slice co-location and group exclusivity conflict (KEP-848 explains
why). It must check the fields too, and that change ships with the fields so the
check never misses them.

### Interaction With Kueue Topology-Aware Scheduling

The LeaderWorkerSet docs already present exclusive placement and Kueue
topology-aware scheduling (TAS) as alternatives. Kueue admits a group only if it
fits one domain and can pack several groups into a domain; exclusive placement
reserves a domain for each group. The placement policy keeps that position: it is
a standalone feature for clusters where LeaderWorkerSet schedules directly, not
an input to Kueue.

The gap is that Kueue chooses domains without seeing the affinity the pod
webhook injects, so a group that uses both can be assigned a domain its
anti-affinity forbids (kubernetes-sigs/kueue#15057). For alpha, admission returns
a warning when a non-`None` policy is set on a template that carries Kueue
topology annotations such as `kueue.x-k8s.io/podset-required-topology`.
An integration would need Kueue to read the policy and treat the domain as
exclusive when it assigns topology. A typed field gives Kueue a stable API to
read if it takes that on, and a common WAS constraint
(kubernetes/kubernetes#142690) would remove the need for per-controller
integrations altogether.

### Update Semantics

**Today.** The LeaderWorkerSet revision hashes only `spec.leaderWorkerTemplate`
(without `maxGroupRestarts`) and `spec.networkConfig`, so the exclusive
annotations are not part of it. The leader pod template, however, copies the
annotation from the live object. Changing the annotation on a live
LeaderWorkerSet therefore changes the leader StatefulSet's pod template without
creating a new revision. With no LeaderWorkerSet rollout in progress the
StatefulSet partition is 0, unless the user set
`rollingUpdateConfiguration.partition`, so the StatefulSet controller replaces
every leader, and with it every group, outside LeaderWorkerSet rollout tracking.
In Hash mode the leader Deployment does the same.

**Proposed: part of the revision.** `placementPolicy` lives in
`leaderWorkerTemplate`, so it is hashed into the revision like the rest of the
template. Changing it rolls out through a new revision under `maxUnavailable`
and `maxSurge` instead of the untracked rollout above. The annotations stay
outside the revision, so changing an annotation keeps today's behavior until the
annotations are retired. Two consequences:

1. The field must not be defaulted when absent, or every existing object would
   get a new revision on its next update. `getPatch` already guards
   `networkConfig` against the same problem.
2. Moving an existing annotation value into the field replaces every group once.
   See [Risks and Mitigations](#risks-and-mitigations).

During a rollout that changes the topology key, old and new groups carry
different terms, and a new group can wait for capacity until old groups are
replaced, as with any rollout under hard placement.

**Alternative: excluded from the revision**, as `maxGroupRestarts` is. Migration
then costs nothing, but a change keeps today's untracked StatefulSet rollout.

**Rejected: immutable**, as first proposed on kubernetes-sigs/lws#1064. It is
stricter than the annotation is today, and unless setting the field once on an
existing object were exempt, migrating would require recreating the
LeaderWorkerSet.

### Open Questions

Each question has a proposed answer for reviewers to confirm.

1. **Conflicting field and annotation values.** Reject them. The issue suggested
   letting the field take precedence, but a silent override is harder to debug,
   and rejecting can be relaxed later.
2. **Update semantics.** Part of the revision, as above, or excluded from it?
3. **Feature gate.** None. KEP-848 added the DisaggregatedSet `placementPolicy`
   without one, and an absent field leaves behavior unchanged.
4. **WAS exclusion at the subgroup level.** Include it in alpha, as above. Rule 10
   checks only `exclusive-topology` today.
5. **Automatic migration in a mutating webhook.** Not planned, because of the
   GitOps drift it causes.
6. **Removing the annotations.** Out of scope. They stay supported, with a
   deprecation warning.
7. **Combining with Kueue TAS.** Warn at admission. Rejecting would make LWS
   validation depend on Kueue's annotation names, and a warning is enough while
   the combination stays unsupported.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

##### Prerequisite testing updates

None identified.

##### Unit tests

- `pkg/webhooks`: the validation matrix above, for both levels, including the
  Kueue warning.
- `pkg/controllers`: the resolver, propagation into leader and worker pod
  templates, and the worker node selector.
- `pkg/schedulerprovider`: rule 10 with the field.
- `pkg/webhooks/disaggregatedset`: `validatePlacement` with the field on a role.
- `pkg/utils/revision`: a LeaderWorkerSet without the field keeps its revision,
  and setting or changing the field creates a new one.

##### Integration tests

- Webhook accept and reject cases, including WAS constraints and a DisaggregatedSet
  role that sets the field.
- A LeaderWorkerSet that uses only the field produces the same pod templates as
  one that uses only the annotation.

##### e2e tests

LeaderWorkerSet e2e has no exclusive-placement case today. Add one that labels
kind nodes with a topology key and checks that each replica lands in its own
domain, once with the field and once with the annotation.

### Graduation Criteria

**Alpha (v0.12)**

- API, validation, propagation and the DisaggregatedSet check implemented.
- Concept docs and API reference updated, with the annotations marked deprecated.
- Unit and integration tests above.

**Beta**

- Open questions resolved.
- e2e coverage for both levels.

## Implementation History

- 2026-10-01: Plan proposed on kubernetes-sigs/lws#1064: a `v1` field, a separate
  LeaderWorkerSet type, and a KEP following KEP-848.
- 2026-10-05: kubernetes/kubernetes#142690 opened for WG Workload-Aware
  Scheduling.
- 2026-10-06: Decision to proceed with the field, mutually exclusive with WAS.
- 2026-10-06: KEP drafted.

## Drawbacks

1. Two ways to express the same constraint until the annotations are retired.
2. A LeaderWorkerSet-specific placement API next to WAS, which may need a mapping
   later.

## Alternatives

### Alternative 1: Reuse the DisaggregatedSet type

**Rejected because** DisaggregatedSet's `PlacementPolicy` includes
`ExclusiveSlice`, which has no meaning for LeaderWorkerSet, and its
`ExclusiveTopology` is scoped to slices across DisaggregatedSets. A shared type
would expose values that LeaderWorkerSet must reject, and the same value name
would mean a different scope in each API.

### Alternative 2: Express exclusivity through `spec.scheduling`

**Rejected for now because** WAS cannot express exclusivity (KEP-5732 keeps a
PodGroup inside a domain but does not keep others out), so this needs
kube-scheduler support first. Revisit when kubernetes/kubernetes#142690 settles.

### Alternative 3: Keep only the annotations

**Rejected because** it leaves an established feature without schema validation
and inconsistent with DisaggregatedSet.

### Alternative 4: Wait for exclusive placement in kube-scheduler

**Rejected because** the annotation's semantics are a v1 feature that must stay
supported, and waiting would leave them untyped indefinitely.
