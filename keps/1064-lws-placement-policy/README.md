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
  - [Interaction With SubGroup Placement](#interaction-with-subgroup-placement)
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
  - [Alternative 2: Express exclusivity through spec.scheduling](#alternative-2-express-exclusivity-through-specscheduling)
  - [Alternative 3: Keep only the annotations](#alternative-3-keep-only-the-annotations)
  - [Alternative 4: Wait for exclusive placement in kube-scheduler](#alternative-4-wait-for-exclusive-placement-in-kube-scheduler)
  - [Alternative 5: Nest the policy in leaderWorkerTemplate and subGroupPolicy](#alternative-5-nest-the-policy-in-leaderworkertemplate-and-subgrouppolicy)
<!-- /toc -->

## Summary

This KEP graduates LeaderWorkerSet exclusive placement from the
`leaderworkerset.sigs.k8s.io/exclusive-topology` and
`leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology` annotations to a typed
top-level field, `spec.placementPolicy`, with replica-level placement on the
policy itself and subgroup-level placement in `spec.placementPolicy.subGroup`.

The field keeps the annotations' semantics exactly. It follows the shape of the
DisaggregatedSet placement policy from [KEP-848](/keps/848-disaggregatedset-placement-policy),
a `type` plus a node-label `topology` key at the same `spec.placementPolicy`
path. Replica-level exclusive placement stays mutually exclusive with
workload-aware scheduling (WAS) gang and topology constraints in
`spec.scheduling`, as the annotation is today under
[KEP-666](/keps/666-gang-scheduling-in-lws).

## Motivation

Exclusive placement is a feature LWS users depend on, but it is configured
through annotations:

1. **No schema validation.** Annotations bypass OpenAPI validation, so a
   malformed topology key is accepted and only fails later, when the API server
   rejects the leader pods whose injected affinity uses it.
2. **Inconsistent with DisaggregatedSet.** DisaggregatedSet already exposes a
   typed `spec.placementPolicy` (KEP-848). LWS should express the same kind of
   constraint the same way, at the same path.
3. **Ergonomics.** Placement at both levels is easier to read and review as one
   policy next to the other top-level LWS policies, such as `spec.scheduling`,
   `spec.networkConfig` and `spec.startupPolicy`.

Workload-aware scheduling does not cover this today. KEP-5732 keeps a PodGroup
inside one topology domain but does not keep other PodGroups out of it, so it
cannot express exclusivity. That gap is raised with WG Workload-Aware Scheduling
in kubernetes/kubernetes#142690. The annotation's semantics are effectively a v1
feature of LWS that cannot be dropped, so LWS needs a typed API for them now
rather than waiting on that discussion.

### Goals

1. A typed, validated field for replica-level and subgroup-level exclusive
   placement, with the same semantics as the annotations.
2. Reject malformed topology keys and invalid combinations at admission.
3. Keep existing manifests that use the annotations working, with a migration
   path that does not replace running groups.
4. Keep the KEP-666 WAS exclusion and the DisaggregatedSet placement conflict
   check correct for both the field and the annotations.

### Non-Goals

1. Changing what exclusive placement means. The scope stays as it is today.
2. Combining replica-level exclusive placement with WAS gang scheduling or
   topology constraints. This stays rejected, as KEP-666 validation rule 10 does
   for the annotation.
3. Exclusive placement in kube-scheduler. That is the subject of
   kubernetes/kubernetes#142690.
4. Best-effort (preferred) placement. Like the annotations, the field is a hard
   constraint.
5. Removing the annotations. See [Open Questions](#open-questions).
6. Integrating the policy with Kueue topology-aware scheduling. The two stay
   alternatives; see
   [Interaction With Kueue Topology-Aware Scheduling](#interaction-with-kueue-topology-aware-scheduling).

## Proposal

Add an optional `spec.placementPolicy` to `LeaderWorkerSetSpec` with:

- `type`: `None` (default) or `ExclusiveTopology`, for each replica.
- `topology`: the node-label key that defines a replica's domain. Required when
  `type` is `ExclusiveTopology`.
- `subGroup`: an optional `type` and `topology` pair for each subgroup.

`ExclusiveTopology` on `spec.placementPolicy` behaves like the
`exclusive-topology` annotation, and on `spec.placementPolicy.subGroup` like the
`subgroup-exclusive-topology` annotation. The controller applies the field
through the same pod-template annotations it propagates today, so the pod
webhook and the pods it produces do not change.

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
`placementPolicy.subGroup` instead of the `subgroup-exclusive-topology`
annotation.

#### Story 3: Moving off the annotations

An operator with existing manifests keeps them unchanged after upgrading. LWS
warns that the annotations are deprecated, and the operator moves each manifest
to the field on their own schedule. The move does not replace running groups.

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

**Risk**: Placement becomes part of the revision, so changing an exclusive
annotation on a live LeaderWorkerSet now rolls out through a new revision,
where today it changes the leader StatefulSet template outside LWS rollout
tracking.

**Mitigation**: This is the intended change: the rollout now honors
`maxUnavailable` and `maxSurge` instead of replacing every group at once.
Release notes call it out. An upgrade alone, or moving a value between the
annotation and the field, keeps the revision. See
[Update Semantics](#update-semantics).

**Risk**: WAS later adds exclusive placement with different semantics, leaving
LWS with two APIs for one concept.

**Mitigation**: Replica-level placement is mutually exclusive with WAS
constraints. A follow-up can map `ExclusiveTopology` onto WAS once
kubernetes/kubernetes#142690 settles.

## Design Details

### API

```go
// PlacementType selects the placement guarantee.
// +kubebuilder:validation:Enum=None;ExclusiveTopology
type PlacementType string

const (
	// PlacementNone injects no placement affinity. This is the default.
	PlacementNone PlacementType = "None"
	// PlacementExclusiveTopology places all pods of the scope (a replica or a
	// subgroup) in one topology domain that no other LeaderWorkerSet replica or
	// subgroup in the namespace occupies.
	PlacementExclusiveTopology PlacementType = "ExclusiveTopology"
)

type LeaderWorkerSetSpec struct {
	// ... existing fields

	// PlacementPolicy controls how replicas and subgroups are placed across
	// topology domains.
	// +optional
	PlacementPolicy *PlacementPolicy `json:"placementPolicy,omitempty"`
}

type PlacementPolicy struct {
	// Type selects the replica-level placement guarantee. Defaults to None.
	// +optional
	// +kubebuilder:default=None
	Type PlacementType `json:"type,omitempty"`

	// Topology is the node-label key that defines a replica topology domain.
	// Required when Type is ExclusiveTopology.
	// +optional
	Topology string `json:"topology,omitempty"`

	// SubGroup configures topology placement for each subgroup within a replica.
	// +optional
	SubGroup *SubGroupPlacementPolicy `json:"subGroup,omitempty"`
}

type SubGroupPlacementPolicy struct {
	// Type selects the subgroup-level placement guarantee. Defaults to None.
	// +optional
	// +kubebuilder:default=None
	Type PlacementType `json:"type,omitempty"`

	// Topology is the node-label key that defines a subgroup topology domain.
	// Required when Type is ExclusiveTopology.
	// +optional
	Topology string `json:"topology,omitempty"`
}
```

The field is added to `leaderworkerset.x-k8s.io/v1`. The type names match the
unprefixed policy types already in the package, and `PlacementPolicy` and
`PlacementType` match the DisaggregatedSet names in their own package.
DisaggregatedSet roles embed `LeaderWorkerSetSpec`, so a role sets it at
`spec.roles[i].spec.placementPolicy`, beside `spec.placementPolicy` on the
DisaggregatedSet.

### Semantics

`ExclusiveTopology` means exactly what the matching annotation means today:

- **Replica level**: required pod affinity to the replica's own pods and required
  pod anti-affinity to pods of any other LeaderWorkerSet replica in the same
  namespace, both keyed on `topology`.
- **Subgroup level**: the same terms, scoped to subgroups.

Pods that do not carry LeaderWorkerSet labels can still share the domain unless
taints are used, as today.

### How the Field Is Applied

`getPatch` writes the effective placement for each level, the field first and
the annotation second, into the revision (see
[Update Semantics](#update-semantics)). The LeaderWorkerSet controller writes
the effective topology key into the leader pod template annotations it
propagates today. The pod controller copies the key from the group's leader pod
into the worker StatefulSet template, since the leader was created from the
group's own revision. The pod webhook builds the affinity terms from those pod
annotations and does not change.

Every other reader of the LeaderWorkerSet annotations moves to the same
resolver:

- the replica-level path in the pod controller that waits for the leader to be
  scheduled and pins workers to its domain;
- the subgroup check in the LeaderWorkerSet webhook;
- KEP-666 validation rule 10 in `pkg/schedulerprovider/phase_one.go`, for the
  replica level only;
- the DisaggregatedSet conflict check in `validatePlacement`.

### Validation

| Condition | Rule |
| --- | --- |
| `type: ExclusiveTopology` at either level | `topology` must be a valid label key. |
| `type: None` or unset at either level | `topology` must be empty. |
| `placementPolicy.subGroup` without subgroups defined | Rejected, as the annotation is today without `subGroupSize`. See [Interaction With SubGroup Placement](#interaction-with-subgroup-placement). |
| Both levels set | The two `topology` keys must differ, because one domain cannot hold a replica exclusively and each of its subgroups exclusively. Checked only when `placementPolicy` is set, so an object that sets both levels through the annotations alone is validated as before and an existing one stays updatable. |
| Field and annotation both set for one level | Rejected if the field is `ExclusiveTopology` with a different key. A level whose `type` is `None` uses the annotation, since the API server defaults `type` to `None` whenever `placementPolicy` is set, including when only `subGroup` is. |
| Replica-level policy or annotation with WAS gang or topology constraints at the selected level | Rejected, as rule 10 does for the annotation today. Subgroup-level placement stays allowed. See [Interaction With Workload-Aware Scheduling](#interaction-with-workload-aware-scheduling). |
| Non-`None` policy on a template that carries Kueue topology annotations | Admission warning. See [Interaction With Kueue Topology-Aware Scheduling](#interaction-with-kueue-topology-aware-scheduling). |

### Migration From the Annotations

| Phase | Behavior |
| --- | --- |
| Alpha | The field and the annotations are both accepted. Admission returns a warning when an annotation is used. Conflicting values are rejected. |
| Later | No automatic migration is planned. Removing the annotations is left to a later update of this KEP. |

Because `getPatch` hashes the effective placement rather than the raw field,
moving a value from an annotation to the field keeps the revision, whether the
operator does it in one update or adds the matching field first and removes the
annotation later. No group is replaced, and every revision carries its own
placement, so a group still on an old revision keeps its constraint when its
worker StatefulSet is rebuilt.

Revisions created before this change carry no placement in their data. The
worker StatefulSet takes its key from the group's leader pod in every case, so a
group on such a revision also keeps the key it was created with.

### Interaction With Workload-Aware Scheduling

KEP-666 validation rule 10 rejects WAS gang scheduling or topology constraints
together with the `exclusive-topology` annotation, because replica-level
exclusivity holds back the worker StatefulSet until the leader pod is scheduled,
which deadlocks a gang that covers the whole replica. The replica-level field is
rejected under the same conditions.

Subgroup-level exclusivity creates the leader and worker StatefulSets right
away, and v1 already allows `subgroup-exclusive-topology` with `spec.scheduling`.
Rejecting it now would fail updates to existing manifests (Goal 3), so neither
`placementPolicy.subGroup` nor the subgroup annotation is checked by rule 10.

If WAS adds exclusive placement (kubernetes/kubernetes#142690), a follow-up can
map `ExclusiveTopology` onto it and lift the restriction.

### Interaction With DisaggregatedSet

`validatePlacement` rejects a non-`None` DisaggregatedSet `placementPolicy`
together with either exclusive annotation on a role. KEP-848 explains that slice
placement and group exclusivity conflict only at the same topology level and
compose across levels, for example slice co-location on a rack label with group
or subgroup exclusivity on `kubernetes.io/hostname`.

With this KEP, `validatePlacement` rejects a role's replica-level or
subgroup-level placement, from the field or the annotation, only when its
`topology` key equals the DisaggregatedSet `spec.placementPolicy.topology`. This
relaxes the blanket rejection of the annotations, which is a compatible change,
and matches the cross-level rule this KEP applies between the replica and
subgroup levels. Admission cannot see how node labels nest, so a role level
coarser than the slice level still cannot schedule; the docs say that the role
level must be finer than the slice level. The change ships with the field so the
check never misses it.

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
Kueue maintainers have said on kubernetes-sigs/kueue#15057 that TAS will not
honor affinity or anti-affinity, and that the long-term plan is to reuse
kube-scheduler logic through a scheduler library, v0.21 at the earliest. That
would make the injected affinity visible to Kueue without an integration
specific to LeaderWorkerSet, and a common WAS constraint
(kubernetes/kubernetes#142690) would remove the need for per-controller
integrations altogether.

### Interaction With SubGroup Placement

kubernetes-sigs/lws#854 proposes KEP-859, a `subGroupPolicy.subGroupPlacement`
list that defines subgroups explicitly instead of through `subGroupSize`, and
keeps `subgroup-exclusive-topology` working for those subgroups.
`placementPolicy.subGroup` applies to subgroups however they are defined, so it
carries the same guarantee. The webhook rejects the subgroup annotation without
`subGroupSize` today; with KEP-859 that check would need to accept
`subGroupPlacement` as well, and the field follows the same rule. Keeping
exclusivity in `spec.placementPolicy` also keeps it apart from
`subGroupPlacement`, which maps subgroups to nodes.

### Update Semantics

**Today.** The LeaderWorkerSet revision hashes only `spec.leaderWorkerTemplate`
(without `maxGroupRestarts` and `restartBackoff`) and `spec.networkConfig`, so
the exclusive annotations are not part of it. The leader pod template, however,
copies the annotation from the live object. Changing the annotation on a live
LeaderWorkerSet therefore changes the leader StatefulSet's pod template without
creating a new revision. With no LeaderWorkerSet rollout in progress the
StatefulSet partition is 0, unless the user set
`rollingUpdateConfiguration.partition`, so the StatefulSet controller replaces
every leader, and with it every group, outside LeaderWorkerSet rollout tracking.
In Hash mode the leader Deployment does the same.

**Proposed: effective placement in the revision.** `getPatch` adds
`spec.placementPolicy` to the revision patch, as it does `spec.networkConfig`,
but writes the effective value for each level: the field if set, otherwise the
annotation. Both sources produce the same revision, so:

1. Moving a value between the annotation and the field, in one update or two,
   keeps the revision and replaces no group.
2. Upgrading the controller keeps the revision. `SetMatchesRevision` applies the
   existing revision to the live object and regenerates the patch, which picks
   up the live annotation, so it matches the new patch even though the stored
   bytes lack placement.
3. Changing the effective placement, through either source, creates a new
   revision and rolls out under `maxUnavailable` and `maxSurge`, replacing the
   untracked rollout above.
4. `ApplyRevision` restores placement from the revision itself and ignores
   the live annotations, so a revision without placement stays without it.
   When the revision predates this change and carries no `placementPolicy`, the
   restored object keeps the live field and annotations, as placement was not
   part of the revision then. Moving between the annotation and the field
   therefore keeps such a revision too, and a placement change on it behaves as
   it does today until the next template change records a new revision.

The field is not defaulted when absent, and an absent policy hashes the same as
`type: None`, so objects without placement keep their revision.

During a rollout that changes the topology key, old and new groups carry
different terms, and a new group can wait for capacity until old groups are
replaced, as with any rollout under hard placement.

**Alternative: raw field in the revision.** Hashing the field as written makes
every annotation-to-field migration replace all groups once, with no change to
the pods. Rejected for that cost.

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
2. **Feature gate.** None. KEP-848 added the DisaggregatedSet `placementPolicy`
   without one, and an absent field leaves behavior unchanged.
3. **Automatic migration in a mutating webhook.** Not planned, because of the
   GitOps drift it causes.
4. **Removing the annotations.** Out of scope. They stay supported, with a
   deprecation warning.
5. **Combining with Kueue TAS.** Warn at admission. Rejecting would make LWS
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
  templates, the worker node selector, and the leader-pod fallback for
  revisions without placement.
- `pkg/schedulerprovider`: rule 10 with the replica-level field, and subgroup
  placement accepted with WAS constraints.
- `pkg/webhooks/disaggregatedset`: `validatePlacement` rejects a role whose
  replica or subgroup key matches the DisaggregatedSet key, from the field or the
  annotation, and accepts different keys.
- `pkg/utils/revision`: the annotation and the field produce the same revision;
  a LeaderWorkerSet without placement keeps its revision; a revision created
  before this change still matches after the upgrade; changing the effective
  placement creates a new revision; `ApplyRevision` restores placement from the
  revision and clears the field for revisions without it.

##### Integration tests

- Webhook accept and reject cases, including WAS constraints at both levels and
  DisaggregatedSet roles with matching and different keys.
- A LeaderWorkerSet that uses only the field produces the same pod templates and
  the same revision as one that uses only the annotation.
- Moving the value from the annotation to the field, in one update and in two,
  replaces no group.
- Changing the annotation value rolls out through a new revision.

##### e2e tests

LeaderWorkerSet e2e has no exclusive-placement case today. Add one that labels
kind nodes with a topology key and checks that each replica lands in its own
domain, once with the field and once with the annotation.

### Graduation Criteria

**Alpha (v0.12)**

- API, validation, propagation, revision handling and the DisaggregatedSet check
  implemented.
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
- 2026-10-14: Revised after review: top-level `spec.placementPolicy`, effective
  placement in the revision, rule 10 kept at replica level, and the
  DisaggregatedSet check limited to matching topology keys.

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

### Alternative 2: Express exclusivity through spec.scheduling

**Rejected for now because** WAS cannot express exclusivity (KEP-5732 keeps a
PodGroup inside a domain but does not keep others out), so this needs
kube-scheduler support first. Revisit when kubernetes/kubernetes#142690 settles.

### Alternative 3: Keep only the annotations

**Rejected because** it leaves an established feature without schema validation
and inconsistent with DisaggregatedSet.

### Alternative 4: Wait for exclusive placement in kube-scheduler

**Rejected because** the annotation's semantics are a v1 feature that must stay
supported, and waiting would leave them untyped indefinitely.

### Alternative 5: Nest the policy in leaderWorkerTemplate and subGroupPolicy

An earlier draft put replica placement in `spec.leaderWorkerTemplate.placementPolicy`
and subgroup placement in `spec.leaderWorkerTemplate.subGroupPolicy.placementPolicy`.

**Rejected because** it splits one concern across two nesting depths, diverges
from `spec.placementPolicy` on DisaggregatedSet and from the other top-level LWS
policies, and puts `placementPolicy` next to KEP-859's `subGroupPlacement` in
`SubGroupPolicy`. Living inside `leaderWorkerTemplate` was also not needed to
make the field part of the revision, since `getPatch` already includes the
top-level `spec.networkConfig`.
