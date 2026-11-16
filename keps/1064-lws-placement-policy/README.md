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
    - [Story 4: A capacity-first inference pool](#story-4-a-capacity-first-inference-pool)
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
  - [Alternative 6: A separate scope field next to type](#alternative-6-a-separate-scope-field-next-to-type)
<!-- /toc -->

## Summary

This KEP graduates LeaderWorkerSet exclusive placement from the
`leaderworkerset.sigs.k8s.io/exclusive-topology` and
`leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology` annotations to a typed
top-level field, `spec.placementPolicy`, with replica-level placement on the
policy itself and subgroup-level placement in `spec.placementPolicy.subGroup`.

It extends the policy with the two dimensions the annotations never had. An
`enforcement` — `Required` (the default) or `Preferred` — decides what happens
when no free domain remains: wait for one, or share a replica's domain. A `type`
value `ExclusiveSet` scopes the exclusivity to the LeaderWorkerSet's own
replicas, so unrelated LeaderWorkerSets can share the domains
(kubernetes-sigs/lws#1311).

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
4. **The shape of the field is too narrow for capacity-first clusters.** The
   first iteration of the field mirrored the annotation: exclusive against
   every LeaderWorkerSet in the namespace, and a hard constraint. On real
   clusters that surfaced two gaps. First, keeping one's own replicas apart
   should not monopolize each domain against unrelated workloads
   (kubernetes-sigs/lws#1311): the empty capacity in a domain is only reusable
   by other workloads when they exist, and on a single-tenant GPU cluster it
   simply idles. Second, with a required anti-affinity the field forces
   "perfectly spread or not running at all": once every domain holds a replica,
   the next replica stays Pending while the domains still have room
   (kubernetes-sigs/lws#1324). For inference pools a Pending replica is a
   deterministic loss, and a co-located replica is survivable — a domain
   failure displaces it only for the minutes Kubernetes takes to rebuild it
   elsewhere. `topologySpreadConstraints` exposes the same choice as
   `whenUnsatisfiable`, and pod anti-affinity has required and preferred forms;
   the field gains both a scope and an enforcement so the user picks the
   failure mode.

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
5. An `enforcement` (`Required` or `Preferred`) at both levels, and a set-scoped
   exclusivity (`ExclusiveSet`) that keeps unrelated LeaderWorkerSets out of the
   repelled set. The defaults resolve to exactly today's behavior.

### Non-Goals

1. Combining replica-level exclusive placement with WAS gang scheduling or
   topology constraints. This stays rejected, as KEP-666 validation rule 10 does
   for the annotation.
2. Exclusive placement in kube-scheduler. That is the subject of
   kubernetes/kubernetes#142690.
3. Making the weight of the preferred enforcement configurable. It is fixed at
   100 for now; see [Open Questions](#open-questions).
4. Removing the annotations. See [Open Questions](#open-questions).
5. Integrating the policy with Kueue topology-aware scheduling. The two stay
   alternatives; see
   [Interaction With Kueue Topology-Aware Scheduling](#interaction-with-kueue-topology-aware-scheduling).

## Proposal

Add an optional `spec.placementPolicy` to `LeaderWorkerSetSpec` with:

- `type`: `None` (default), `ExclusiveSet` or `ExclusiveTopology`, for each
  replica.
- `topology`: the node-label key that defines a replica's domain. Required when
  `type` is not `None`.
- `enforcement`: `Required` (default) or `Preferred`.
- `subGroup`: an optional `type`, `topology` and `enforcement` triple for each
  subgroup.

`ExclusiveTopology` on `spec.placementPolicy` behaves like the
`exclusive-topology` annotation, and on `spec.placementPolicy.subGroup` like the
`subgroup-exclusive-topology` annotation. `ExclusiveSet` is the same placement
with the exclusivity scoped to the LeaderWorkerSet itself: only its own replicas
are kept out of a domain, so unrelated LeaderWorkerSets may share it. `Required`
keeps the injected anti-affinity required, so a replica without a free domain
stays Pending; `Preferred` makes it a weighted scheduling preference (weight
100), so the scheduler keeps replicas apart while a free domain remains and
co-locates them afterwards.

The controller applies the field through the same pod-template annotations it
propagates today, so the pod webhook and the pods it produces do not change.

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

#### Story 4: A capacity-first inference pool

An inference team runs four replicas of a serving workload across three racks
and cares about capacity more than about strict isolation. With
`type: ExclusiveSet` and `enforcement: Preferred`, the replicas spread over the
racks while free ones remain, and the fourth replica co-locates instead of
staying Pending, so the pool serves at full capacity. The racks' remaining
resources are used, and a rack failure displaces the replicas on it only for the
minutes Kubernetes takes to rebuild them on the surviving racks. With
`ExclusiveTopology`, keeping the replicas apart would also lock unrelated
workloads out of the racks, and with `Required` the fourth replica would sit
Pending while capacity is idle.

### Notes/Constraints/Caveats

- `ExclusiveTopology` with `Required` is a hard constraint, as the annotation
  is. Without enough free domains, pods stay Pending.
- `ExclusiveTopology` with `Preferred` is the same global repulsion, softened:
  another LeaderWorkerSet is repelled only while a free domain remains, and two
  replicas may share a domain once none does. This is the global-but-soft form,
  for workloads that keep other LeaderWorkerSets out when they can. It is not
  appropriate for hardware-sliced accelerators, which need one domain per
  replica and therefore `Required`; the capacity-first case is what
  `ExclusiveSet` is for.
- `Preferred` makes the replica-level anti-affinity a weighted preference
  (weight 100). The scheduler keeps replicas apart while a free domain remains;
  once every domain holds a replica, every domain is penalized equally and the
  next replica co-locates with whichever sibling the remaining scores pick. This
  is best-effort repulsion, not a guaranteed even split — the replicas may land
  together rather than in a balanced packing.
- The replica's own pods stay co-located by the required affinity at every
  enforcement; that term does not soften.
- `Preferred` and `ExclusiveSet` are only expressible through the field, never
  through the annotations, so no existing manifest changes behavior.
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

**Risk**: `Preferred` trades the isolation guarantee for capacity: a domain can
hold several replicas, and a domain failure displaces all of them at once.

**Mitigation**: The mode is opt-in and both defaults stay `Required`, so an
upgraded object keeps today's behavior. Concept docs call out the trade, and
`Preferred` is never expressible through the annotations.

**Risk**: WAS later adds exclusive placement with different semantics, leaving
LWS with two APIs for one concept.

**Mitigation**: Replica-level placement is mutually exclusive with WAS
constraints. A follow-up can map `ExclusiveTopology` onto WAS once
kubernetes/kubernetes#142690 settles.

## Design Details

### API

```go
// PlacementType selects the topology placement guarantee.
// +kubebuilder:validation:Enum=None;ExclusiveSet;ExclusiveTopology
type PlacementType string

// PlacementEnforcement selects how the exclusive placement is enforced.
// +kubebuilder:validation:Enum=Required;Preferred
type PlacementEnforcement string

const (
	// PlacementNone injects no placement affinity. This is the default.
	PlacementNone PlacementType = "None"

	// PlacementExclusiveSet places all pods of a group, or of a subgroup, in
	// one topology domain and keeps the pods of other groups, or subgroups, of
	// the same LeaderWorkerSet out of that domain. Groups of other
	// LeaderWorkerSets may share the domain.
	PlacementExclusiveSet PlacementType = "ExclusiveSet"

	// PlacementExclusiveTopology places all pods of the scope (a replica or a
	// subgroup) in one topology domain that no other LeaderWorkerSet replica or
	// subgroup in the namespace occupies.
	PlacementExclusiveTopology PlacementType = "ExclusiveTopology"

	// PlacementRequired keeps the injected anti-affinity required, so a group
	// without a free domain stays Pending. This is the default.
	PlacementRequired PlacementEnforcement = "Required"

	// PlacementPreferred makes the group-level anti-affinity a scheduling
	// preference, so the scheduler co-locates the groups once it runs out of
	// domains.
	PlacementPreferred PlacementEnforcement = "Preferred"
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
	// Required when Type is not None.
	// +optional
	Topology string `json:"topology,omitempty"`

	// Enforcement selects how the exclusive placement is enforced. Defaults to
	// Required.
	// +optional
	// +kubebuilder:default=Required
	Enforcement PlacementEnforcement `json:"enforcement,omitempty"`

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
	// Required when Type is not None.
	// +optional
	Topology string `json:"topology,omitempty"`

	// Enforcement selects how the exclusive placement is enforced, as at the
	// replica level. Defaults to Required.
	// +optional
	// +kubebuilder:default=Required
	Enforcement PlacementEnforcement `json:"enforcement,omitempty"`
}
```

The field is added to `leaderworkerset.x-k8s.io/v1`. The type names match the
unprefixed policy types already in the package, and `PlacementPolicy` and
`PlacementType` match the DisaggregatedSet names in their own package.
DisaggregatedSet roles embed `LeaderWorkerSetSpec`, so a role sets it at
`spec.roles[i].spec.placementPolicy`, beside `spec.placementPolicy` on the
DisaggregatedSet.

`ExclusiveSet` is deliberately not named after DisaggregatedSet's
`ExclusiveSlice`. That value co-locates a slice's roles in one domain, the
opposite emphasis from the spread this scope performs, and KEP-1064 already
rejected sharing the DisaggregatedSet type because the same name would mean
different things in each API (see [Alternative 1](#alternative-1-reuse-the-disaggregatedset-type)).
"Set" is this API's own term and cannot collide. `None` and `ExclusiveTopology`
keep their names and behavior unchanged.

### Semantics

`ExclusiveTopology` with the default `Required` enforcement means exactly what
the matching annotation means today:

- **Replica level**: required pod affinity to the replica's own pods and required
  pod anti-affinity to pods of any other LeaderWorkerSet replica in the same
  namespace, both keyed on `topology`.
- **Subgroup level**: the same terms, scoped to subgroups.

With `Preferred` the terms are unchanged; only the anti-affinity moves to the
preferred list, so the repulsion is still global but yields to capacity once no
free domain remains. That combination is for keeping other LeaderWorkerSets out
when possible, never for a hardware slice that relies on the hard guarantee.

Pods that do not carry LeaderWorkerSet labels can still share the domain unless
taints are used, as today.

`ExclusiveSet` injects the same affinity and narrows the anti-affinity to the
LeaderWorkerSet's own replicas: the selector repels pods whose group-key label
exists and is not this group's, and additionally carries this set's name label,
so replicas of other LeaderWorkerSets are not repelled.

`enforcement` chooses the list the anti-affinity term lands in, not its
contents, at both levels:

- **Required** (default): the term goes into the required list. A replica
  without a free domain stays Pending.
- **Preferred**: the term goes into the preferred list with weight 100. The
  scheduler penalizes a domain that already holds a sibling but may still pick
  it; when every free domain is exhausted the replicas co-locate.

The group's own pods stay co-located by the required pod affinity at either
enforcement; exclusivity never softens the intra-group cohesion.

### How the Field Is Applied

`getPatch` writes the effective placement for each level, the field first and
the annotation second, into the revision (see
[Update Semantics](#update-semantics)). The LeaderWorkerSet controller writes
the effective topology key into the leader pod template annotations it
propagates today, plus an `exclusive-scope` annotation when the scope is
`ExclusiveSet`, an `exclusive-enforcement` annotation when the enforcement is
`Preferred`, and the subgroup forms when `subGroup` is set — the non-default
values only, so an upgrade or a migration between the annotation and the field
does not change the template. The pod controller copies the keys from the
group's leader pod into the worker StatefulSet template, since the leader was
created from the group's own revision. The pod webhook builds the affinity terms
from those pod annotations: the scope decides the selector and the enforcement
decides the target list.

The annotations are part of the leader pod template, so a scope or enforcement
change is a template change and rolls out through a new revision, like a
topology change. Default values are never stamped, so an upgrade or a migration
between the annotation and the field does not change the template.

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
| `type: ExclusiveTopology` or `ExclusiveSet` at either level | `topology` must be a valid label key. |
| `type: None` or unset at either level | `topology` must be empty, and `enforcement` must be unset or `Required`. |
| `exclusive-scope`, `exclusive-enforcement`, or their subgroup forms set by a user in the pod templates | Rejected. The controller manages these annotations; a user-set value could silently relax the guarantee the policy asks for. A value unchanged from the previous template is allowed so objects that already carry one remain updatable. |
| The same internal annotations on a DisaggregatedSet role's pod templates | Rejected by the DisaggregatedSet webhook, so a DisaggregatedSet cannot admit a role whose child LeaderWorkerSet admission would then reject. |
| `placementPolicy.subGroup` without subgroups defined | Rejected, as the annotation is today without `subGroupSize`. See [Interaction With SubGroup Placement](#interaction-with-subgroup-placement). |
| Both levels set | The two `topology` keys must differ, because one domain cannot hold a replica exclusively and each of its subgroups exclusively. Checked only when `placementPolicy` is set, so an object that sets both levels through the annotations alone is validated as before and an existing one stays updatable. |
| Field and annotation both set for one level | Rejected if the field is `ExclusiveTopology` with a different key. `ExclusiveSet` is rejected while the annotation of the same level is set at all, even on the same key, because the annotation is globally scoped and would silently change which pods are repelled. The check reads the LWS metadata annotation: an annotation on a pod template is inert whenever the field sets that level, since the controller stamps the field topology over it, and only produces the deprecation warning. A level whose `type` is `None` uses the annotation, since the API server defaults `type` to `None` whenever `placementPolicy` is set, including when only `subGroup` is. |
| Replica-level policy or annotation with WAS gang or topology constraints at the selected level | Rejected, as rule 10 does for the annotation today. Subgroup-level placement stays allowed. See [Interaction With Workload-Aware Scheduling](#interaction-with-workload-aware-scheduling). |
| Non-`None` policy on a template that carries Kueue topology annotations | Admission warning. See [Interaction With Kueue Topology-Aware Scheduling](#interaction-with-kueue-topology-aware-scheduling). |

### Migration From the Annotations

| Phase | Behavior |
| --- | --- |
| Alpha | The field and the annotations are both accepted. Admission returns a warning when an annotation is used. Conflicting values are rejected, and switching to `ExclusiveSet` requires removing the same level's annotation in the same update. |
| Later | No automatic migration is planned. Removing the annotations is left to a later update of this KEP. |

Because `getPatch` hashes the effective placement rather than the raw field,
moving a value from an annotation to the field keeps the revision, whether the
operator does it in one update or adds the matching field first and removes the
annotation later. No group is replaced, and every revision carries its own
placement, so a group still on an old revision keeps its constraint when its
worker StatefulSet is rebuilt.

Moving to `ExclusiveSet` has one extra step: remove the annotation in the same
update as the type change, or move to the matching `ExclusiveTopology` field
first and remove the annotation before switching. The annotation is globally
scoped, so the two cannot be combined; removing it without setting the field
drops the placement.

Revisions created before this change carry no placement in their data. The
worker StatefulSet takes its key from the group's leader pod in every case, so a
group on such a revision also keeps the key it was created with. The controller
records placement in such a revision the first time it reconciles the
LeaderWorkerSet after the upgrade, as described in Update Semantics item 2.

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
together with either exclusive annotation on a role. With this KEP it also
rejects a role's replica-level or subgroup-level exclusive placement from
`spec.placementPolicy`, on any topology key, alongside a non-`None`
DisaggregatedSet `placementPolicy`.

The rejection does not depend on the keys. The slice affinity and the group or
subgroup affinity are two required `PodAffinityTerm` entries with disjoint
selectors on the same pod, and kube-scheduler requires one existing pod to match
every required term. Once the first group of a slice is placed, no pod matches
both terms for the next group, role or subgroup, so kube-scheduler places it in
another domain and splits the slice. Composing them, for example a slice per
rack with a group per host, is therefore not supported.

The role's pod templates are also validated the way the child LeaderWorkerSet
would be: the controller-managed `exclusive-scope`, `exclusive-enforcement` and
their subgroup forms are rejected at DisaggregatedSet admission, and the
field-versus-annotation rules above apply to the role's placement policy. Without
this the DisaggregatedSet would be admitted and the child LeaderWorkerSet it
creates would be rejected by the LeaderWorkerSet webhook, so the
DisaggregatedSet could never create the role.

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
2. Upgrading the controller keeps the revision. The first time the controller
   reconciles a LeaderWorkerSet whose current revision does not record
   placement, it creates a revision under the same revision key that records
   the placement the groups run with. It reads that placement from the
   exclusive-topology annotations on the pod template of the leader
   StatefulSet (or Deployment), which the previous controller set, and removes
   the old revision. The key is unchanged, so the partition and the revision
   labels on pods and StatefulSets do not move, and no group is replaced.
   Reading the leader template rather than the live annotation means that an
   annotation changed while the controller was being upgraded still rolls out.
3. Changing the effective placement, through either source, creates a new
   revision and rolls out under `maxUnavailable` and `maxSurge`, replacing the
   untracked rollout above.
4. `ApplyRevision` restores placement from the revision itself and ignores
   the live annotations, so a revision without placement stays without it.
   When the revision predates this change and carries no `placementPolicy`, the
   restored object clears the field and keeps the live annotations, as the
   annotations were the only way to set placement then. The controller replaces
   such a revision before it compares revisions (item 2), so after an upgrade
   moving between the annotation and the field keeps the revision, and a
   placement change creates a new revision, as on any other revision.

The scope is recorded as it is today, and only a `Preferred` enforcement is
recorded: a `Required` enforcement resolves as the absent field, so an object
created before this change keeps its hash and does not roll on upgrade. Any
scope or enforcement change that alters the injected terms creates a new
revision under the existing rules.

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
6. **Preferred anti-affinity vs `topologySpreadConstraints` `ScheduleAnyway`.**
   The preferred anti-affinity is chosen for now, with a fixed weight of 100.
   The difference shows up exactly when domains run out: a soft anti-affinity
   may pile several replicas onto one domain, while `ScheduleAnyway` spread aims
   for a bounded skew. Happy to reshape per review.
7. **Configurable weight.** Not in this iteration. A fixed 100 is the maximum
   weight the inter-pod affinity plugin supports, so spreading is the strongest
   preference that plugin can express; it is a preference, not a guarantee
   against the other plugins' scores. Exposing the knob can follow if a use case
   appears.

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
- `pkg/webhooks/disaggregatedset`: `validatePlacement` rejects a role with
  replica or subgroup exclusive placement, from the field or the annotation, on
  any topology key, when the DisaggregatedSet `placementPolicy` is not `None`;
  and the internal `exclusive-scope`/`exclusive-enforcement` annotations on a
  role's pod templates are rejected, matching the LeaderWorkerSet webhook.
- `pkg/utils/disaggregatedset`: `ComputeRevisionV1` changes when a role's
  `spec.placementPolicy` sets, changes or clears exclusive placement, and keeps
  the existing hash for roles without exclusive placement.
- `pkg/webhooks`: the injected terms for both scopes and both enforcements,
  including the set-scoped selector and the preferred weight; the guard
  recognizes injected required and preferred terms by their selector and does
  not skip the injection for user-declared terms on the same topology key.
- `pkg/utils/revision`: the annotation and the field produce the same revision;
  a LeaderWorkerSet without placement keeps its revision; a revision created
  before this change still matches after the upgrade; changing the effective
  placement creates a new revision; `ApplyRevision` restores placement from the
  revision and clears the field for revisions without it; a `Required`
  enforcement hashes as the absent field.

##### Integration tests

- Webhook accept and reject cases, including WAS constraints at both levels and
  DisaggregatedSet roles with exclusive placement on the slice key and on other
  keys.
- A LeaderWorkerSet that uses only the field produces the same pod templates and
  the same revision as one that uses only the annotation.
- Moving the value from the annotation to the field, in one update and in two,
  replaces no group.
- Changing the annotation value, or the field, rolls out through a new revision
  under `maxUnavailable`.
- After an upgrade, a LeaderWorkerSet whose revision does not record placement
  keeps its leader and worker StatefulSets, and a later annotation change rolls
  out through a new revision.

##### e2e tests

LeaderWorkerSet e2e has no exclusive-placement case today. Add one that labels
kind nodes with a topology key and checks that each replica lands in its own
domain, once with the field and once with the annotation. Extend it to the new
matrix: two unrelated `ExclusiveSet` workloads share a domain, a `Required`
sibling stays Pending on a single domain, a `Preferred` sibling co-locates, and
an `ExclusiveTopology` replica refuses to share with any other workload.

### Graduation Criteria

**Alpha (v1.1)**

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
- 2026-11-01: Marked implementable and retargeted alpha to v1.1, since LWS
  went from v0.11 to v1.0.0. Implementation in kubernetes-sigs/lws#1165.
- 2026-11-05: Revised after review: the DisaggregatedSet check rejects role
  exclusive placement on any topology key again, since the slice and group
  affinity terms cannot both hold, and unversioned DisaggregatedSet revisions
  include the role's `spec.placementPolicy`.
- 2026-11-16: Revised after review: add the `enforcement` (Required / Preferred)
  and the set-scoped `ExclusiveSet` placement. The value is named after the
  LeaderWorkerSet, not DisaggregatedSet's `ExclusiveSlice`, so the two APIs do
  not share a name for different scopes.
- 2026-11-17: Revised with the implementation: the field-versus-annotation
  conflict is checked on the LWS metadata only, since a template annotation is
  inert once the field sets the level; switching to `ExclusiveSet` requires
  removing the same level's annotation; documented `ExclusiveTopology` with
  `Preferred` and the role-template validation on the DisaggregatedSet side.

## Drawbacks

1. Two ways to express the same constraint until the annotations are retired.
2. A LeaderWorkerSet-specific placement API next to WAS, which may need a mapping
   later.
3. `Preferred` weakens the isolation guarantee the feature was originally built
   for. It stays opt-in, but two failure modes now exist.

## Alternatives

### Alternative 1: Reuse the DisaggregatedSet type

**Rejected because** DisaggregatedSet's `PlacementPolicy` includes
`ExclusiveSlice`, which aggregates a slice's roles in one domain — the opposite
emphasis from the spread this API needs — and its `ExclusiveTopology` is scoped
to slices across DisaggregatedSets. A shared type would expose values that
LeaderWorkerSet must reject, and the same value name would mean a different
scope in each API. The `ExclusiveSet` introduced later is deliberately a new
LeaderWorkerSet term rather than a reuse of `ExclusiveSlice`.

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

### Alternative 6: A separate scope field next to type

Keep `type` as on/off values and move the exclusivity scope into a sibling
field, for example `type: Exclusive` plus `scope: Cluster | Set`, so no enum
value carries a scope in its name.

**Rejected for now because** `placementPolicy` merged to main on 2026-11-05 and
has not shipped in a release, so nothing forces compatibility — but
restructuring values that are already in main (`ExclusiveTopology`) is a larger
change than the name collision requires. A new enum value, `ExclusiveSet`, fixes
the collision with the smallest possible surface. If review prefers the field
shape, the KEP update can adopt it before the API ships.
