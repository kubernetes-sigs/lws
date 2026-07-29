# KEP-859: SubGroup Placement

<!--
This is the title of your KEP. Keep it short, simple, and descriptive. A good
title can help communicate what the KEP is and should be considered as part of
any review.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1](#story-1)
    - [Story 2](#story-2)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
  - [Validation](#validation)
  - [Annotation Propagation](#annotation-propagation)
  - [Pod Webhook Defaulting](#pod-webhook-defaulting)
  - [Node Affinity Injection](#node-affinity-injection)
  - [Exclusive Topology Interaction](#exclusive-topology-interaction)
  - [TPU Environment Injection](#tpu-environment-injection)
  - [Test Plan](#test-plan)
      - [Prerequisite testing updates](#prerequisite-testing-updates)
      - [Unit tests](#unit-tests)
      - [Integration tests](#integration-tests)
      - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

<!--
This section is incredibly important for producing high-quality, user-focused
documentation such as release notes or a development roadmap. It should be
possible to collect this information before implementation begins, in order to
avoid requiring implementers to split their attention between writing release
notes and implementing the feature itself. KEP editors and SIG Docs
should help to ensure that the tone and content of the `Summary` section is
useful for a wide audience.

A good summary is probably at least a paragraph in length.
-->

This KEP extends the existing SubGroup feature with a new `subGroupPlacement`
field under `SubGroupPolicy`. Today subgroups are formed implicitly by evenly
partitioning workers according to `subGroupSize`. `subGroupPlacement` instead
lets users explicitly assign specific worker indexes to each subgroup and
constrain each subgroup to nodes carrying specific labels. The pod webhook
translates the per-subgroup `matchLabels` into required `NodeAffinity` on the
corresponding worker pods, giving users deterministic, semantic control over
which class of nodes each subgroup lands on.

## Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this KEP.  Describe why the change is important and the benefits to users.
-->

The current SubGroup API (`subGroupSize`) only supports uniform partitioning:
all subgroups are the same size and membership is derived from worker ordinals.
Combined with `subgroup-exclusive-topology`, it guarantees that pods in the same
subgroup co-locate in the same topology domain and that different subgroups do
not share a domain. However, it provides no way to say *which* subgroup should
run on *which* class of nodes.

This is insufficient for workloads that require:

- explicit mapping of subgroup members to distinct node pools or node classes;
- deterministic placement of different workers onto nodes with different
  hardware, locality, or scheduling labels (e.g. a "local" zone vs. a "remote"
  zone);
- subgroup definitions driven by workload semantics rather than only by
  topology exclusivity or uniform size.

`subGroupPlacement` addresses a need that is orthogonal to exclusive topology:

- exclusive topology ensures pods in the same subgroup co-locate and that pods
  with different subgroup keys are isolated from each other;
- `subGroupPlacement` explicitly defines subgroup membership and constrains each
  subgroup to nodes matching user-specified labels.

In short, exclusive topology provides subgroup co-location and isolation, while
`subGroupPlacement` provides explicit subgroup-to-node placement mapping. The
two can be combined.

### Goals

<!--
List the specific goals of the KEP. What is it trying to achieve? How will we
know that this has succeeded?
-->

- Allow users to explicitly assign `workerIndexes` to each subgroup.
- Allow users to specify `matchLabels` per subgroup, which the pod webhook
  translates into required `NodeAffinity` for the pods in that subgroup.
- Support subgroups of non-uniform size.
- Preserve compatibility with `subgroup-exclusive-topology` so a subgroup can
  simultaneously be co-located and constrained to a node class.
- Preserve correct TPU environment variable injection for placement-defined
  subgroups.

### Non-Goals

<!--
What is out of scope for this KEP? Listing non-goals helps to focus discussion
and make progress.
-->

- Supporting `subGroupPlacement` together with `subGroupSize`. The two are
  mutually exclusive.
- Supporting `subGroupPlacement` with the `LeaderWorker` subgroup type. Only
  `LeaderExcluded` is supported, because placement addresses worker pods and the
  leader is assumed to have independent scheduling requirements.
- Supporting a TPU-requesting leader pod. When `leaderTemplate` is omitted the
  `workerTemplate` is also used for the leader, so this is validated and
  rejected.
- Mutating `subGroupPlacement` after the LeaderWorkerSet is created. The field
  is immutable, consistent with `subGroupSize`.
- Expressing richer node-selection semantics than equality-based `matchLabels`
  (e.g. `matchExpressions`, operators other than `In`).

## Proposal

Add an optional `subGroupPlacement` list to `SubGroupPolicy`. Each entry names
the set of worker indexes that form one subgroup, together with a set of
`matchLabels`. When set, the LeaderWorkerSet controller encodes the placement
into a pod-template annotation. The pod webhook then, for each worker pod, finds
the subgroup that owns the pod's worker index, assigns the subgroup labels, and
merges the subgroup's `matchLabels` into the pod's required node affinity.

### User Stories

#### Story 1

As a user running a disaggregated inference workload, I want workers 1 and 2 to
run on nodes labeled `zone=remote` and worker 3 to run on nodes labeled
`zone=local`, so that each worker lands on the node class appropriate for its
role:

```yaml
leaderWorkerTemplate:
  size: 4 # 1 leader + 3 workers
  subGroupPolicy:
    subGroupPolicyType: LeaderExcluded
    subGroupPlacement:
      - workerIndexes: [1, 2]
        matchLabels:
          zone: remote
      - workerIndexes: [3]
        matchLabels:
          zone: local
```

Workers 1 and 2 are constrained to `zone=remote` and form one subgroup; worker 3
is constrained to `zone=local` and forms another.

#### Story 2

As a user running a multi-host TPU workload split into placement-defined
subgroups, I want the TPU environment variables (`TPU_WORKER_HOSTNAMES`,
`TPU_WORKER_ID`, etc.) to be computed over the explicit members of each
subgroup, so that each subgroup initializes as an independent TPU slice.

### Notes/Constraints/Caveats

- `subGroupPlacement` is only valid with `subGroupPolicyType: LeaderExcluded`.
- `subGroupPlacement` and `subGroupSize` are mutually exclusive; exactly one
  must be set when `subGroupPolicy` is present.
- `workerIndexes` are 1-based worker indexes in the range `[1, size-1]`. Every
  worker must be covered by exactly one subgroup; no index may be duplicated or
  omitted.
- `matchLabels` are equality-based and translated to `NodeSelectorOpIn`
  requirements; they are merged into any existing required node affinity rather
  than replacing it.

### Risks and Mitigations

<!--
What are the risks of this proposal, and how do we mitigate? Think broadly.
For example, consider both security and how this will impact the larger
Kubernetes ecosystem.
-->

- **Unschedulable pods.** If no node carries the requested labels, affected
  worker pods stay Pending. This is expected node-affinity behavior; validation
  cannot verify label existence at admission time. Documentation will call this
  out.
- **Misconfiguration.** Incomplete or overlapping `workerIndexes` would produce
  an ambiguous mapping. Mitigated by admission validation that requires every
  worker to be covered exactly once.
- **Feature interaction.** Interaction with exclusive topology and TPU injection
  is the main source of complexity; mitigated by explicit unit tests for both
  paths (see Test Plan).
- The change is additive and behind a new optional field, so existing
  LeaderWorkerSets are unaffected.

## Design Details

<!--
This section should contain enough information that the specifics of your
change are understandable.
-->

### API

Two new types are added to `api/leaderworkerset/v1`. `SubGroupPolicy` gains an
optional `subGroupPlacement` list:

```golang
type SubGroupPolicy struct {
	// +kubebuilder:validation:Enum={LeaderWorker,LeaderExcluded}
	// +kubebuilder:default=LeaderWorker
	Type *SubGroupPolicyType `json:"subGroupPolicyType,omitempty"`

	// SubGroupSize partitions workers evenly. Mutually exclusive with
	// subGroupPlacement.
	SubGroupSize *int32 `json:"subGroupSize,omitempty"`

	// subGroupPlacement explicitly assigns workers to subgroups and constrains
	// each subgroup to nodes matching the given labels. This field is only
	// supported with LeaderExcluded and is mutually exclusive with subGroupSize.
	// +optional
	SubGroupPlacement []SubGroupPlacement `json:"subGroupPlacement,omitempty"`
}

// SubGroupPlacement explicitly describes one subgroup's members and node labels.
type SubGroupPlacement struct {
	// workerIndexes contains the worker indexes assigned to this subgroup.
	WorkerIndexes []int32 `json:"workerIndexes"`

	// matchLabels are merged into required node affinity for all workers in
	// this subgroup.
	MatchLabels map[string]string `json:"matchLabels"`
}
```

Two new annotation keys carry placement data onto pod templates:

- `leaderworkerset.sigs.k8s.io/subgroup-placement`: the JSON-encoded
  `[]SubGroupPlacement`, set on both leader and worker pod templates by the
  controllers.
- `leaderworkerset.sigs.k8s.io/subgroup-members`: the JSON-encoded sorted list
  of worker indexes in a pod's subgroup, populated by the pod webhook and
  consumed by TPU injection.

Helper functions `EncodeSubGroupPlacement`/`DecodeSubGroupPlacement` and
`EncodeSubGroupMembers`/`DecodeSubGroupMembers` (in `subgroup_helpers.go`)
handle the JSON round-trip.

### Validation

Validation runs in the LeaderWorkerSet webhook (`validateSubGroupPolicy`, which
replaces the old create/update-specific naming) on both create and update:

- If both `subGroupSize` and `subGroupPlacement` are set, reject as mutually
  exclusive.
- If neither is set while `subGroupPolicy` is present, reject.
- When `subGroupPlacement` is set (`validateSubGroupPlacement`):
  - require `size - 1 >= 1` (at least one worker);
  - require `subGroupPolicyType == LeaderExcluded`;
  - reject if the effective leader spec requests TPUs (the leader spec is the
    `leaderTemplate` if present, otherwise the `workerTemplate`);
  - require each entry's `workerIndexes` and `matchLabels` to be non-empty;
  - validate every label key with `IsQualifiedName` and every label value with
    `IsValidLabelValue`;
  - require every `workerIndex` to be within `[1, size-1]`, with no duplicates;
  - require the union of all `workerIndexes` to cover every worker exactly once,
    reporting the missing indexes otherwise.

On update, both `subGroupSize` and `subGroupPlacement` are validated as
immutable, and enabling/removing `subGroupPolicy` after creation is rejected.

### Annotation Propagation

Both `constructLeaderStatefulSetApplyConfiguration` (leader controller) and
`constructWorkerStatefulSetApplyConfiguration` (pod controller) are made
nil-safe for `Type` and `SubGroupSize` and, when `subGroupPlacement` is
non-empty, set the `subgroup-placement` annotation on the pod template via
`EncodeSubGroupPlacement`.

### Pod Webhook Defaulting

In `PodWebhook.Default`:

- For the leader pod, the existing "leader lands on subgroup 0" defaulting is
  skipped when the `subgroup-placement` annotation is present (placement is a
  `LeaderExcluded`-only feature, so the leader is not part of any subgroup).
- For a worker pod, if placement is present and the subgroup index label is not
  yet set, the webhook:
  1. decodes the placement and finds the entry whose `workerIndexes` contains
     this worker's index (error if none);
  2. sets the subgroup index label to the entry's position and derives the
     subgroup unique hash from the leader name and that index;
  3. writes the sorted members into the `subgroup-members` annotation;
  4. applies node affinity from the entry's `matchLabels`;
  5. if `subgroup-exclusive-topology` is set, applies the exclusive affinity for
     the subgroup key.
- If placement is absent, the pre-existing `subGroupSize` path is used
  unchanged.

### Node Affinity Injection

`applyPlacementNodeAffinity` converts `matchLabels` into
`NodeSelectorRequirement`s with operator `In`, iterating label keys in sorted
order for deterministic output. The requirements are **merged** into existing
required node affinity: if no `RequiredDuringSchedulingIgnoredDuringExecution`
terms exist, a single term is created; otherwise the requirements are appended
to every existing `NodeSelectorTerm`.

### Exclusive Topology Interaction

The exclusive-affinity handling (`SetExclusiveAffinities`) is preserved on the
placement path, so a placement-defined subgroup can still be co-located within a
topology domain via `subgroup-exclusive-topology` while also being pinned to a
node class via `matchLabels`.

### TPU Environment Injection

`AddTPUVariables` checks for the `subgroup-members` annotation first. When
present, `addTPUVariablesPlacement` computes TPU variables over the explicit,
sorted subgroup members:

- `TPU_WORKER_ID` is the pod's position within the sorted member list;
- `TPU_WORKER_HOSTNAMES` / `TPU_PROCESS_ADDRESSES` are built from the member
  hostnames (`<leader>-<member>.<subdomain>`);
- `TPU_NAME` is the leader name.

When the annotation is absent, the existing `subGroupSize` and non-subgroup
paths are used unchanged.

### Test Plan

[X] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes
necessary to implement this enhancement.

##### Prerequisite testing updates

None. Existing SubGroup tests continue to cover the `subGroupSize` path.

##### Unit tests

- `pkg/webhooks`: LeaderWorkerSet webhook validation for `subGroupPlacement`
  (mutual exclusivity with `subGroupSize`, `LeaderExcluded`-only, TPU-leader
  rejection, empty/duplicate/out-of-range/uncovered worker indexes, invalid
  label key/value).
- `pkg/webhooks`: pod webhook defaulting — worker pods get the correct subgroup
  index, `subgroup-members` annotation, and merged node affinity; leader pod is
  excluded from subgrouping.
- `pkg/utils/accelerators`: TPU variable injection over explicit members
  (`addTPUVariablesPlacement`), including `TPU_WORKER_ID` ordering.
- `api/leaderworkerset/v1`: encode/decode round-trip helpers.

##### Integration tests

- A LeaderWorkerSet with `subGroupPlacement` produces worker pods with the
  expected subgroup index labels, `subgroup-members` annotations, and required
  node affinity.
- `subGroupPlacement` combined with `subgroup-exclusive-topology` yields both
  the exclusive affinity and the placement node affinity.

##### e2e tests

- Deploy a LeaderWorkerSet using `subGroupPlacement` against a cluster with
  labeled nodes and verify each subgroup's pods schedule onto the matching
  nodes.

### Graduation Criteria

#### Alpha

- Feature implemented behind the new optional API field.
- Validation, defaulting, node-affinity injection, and TPU injection covered by
  unit and integration tests.
- Documentation added.

#### Beta

- e2e coverage on a multi-node cluster.
- No open bugs against the feature for one release cycle.
- User feedback incorporated.

## Implementation History

- 2026-07-14: KEP drafted (Summary, Motivation, Proposal, Design Details) based
  on the implementation on the `subGroupPlacement` branch.

## Drawbacks

<!--
Why should this KEP _not_ be implemented?
-->

- Adds a second, independent way to define subgroups, increasing the API surface
  and the number of feature interactions that must be tested (exclusive
  topology, TPU injection, LeaderExcluded).
- Encoding placement through pod-template annotations is an internal-coupling
  mechanism that must be kept in sync between the controllers and the webhook.

## Alternatives

<!--
What other approaches did you consider, and why did you rule them out?
-->

- **Reuse `subGroupSize` and require users to add node affinity manually on the
  pod template.** Rejected because a single template cannot express different
  affinity per subgroup, which is the core requirement.
- **Support `matchExpressions` instead of `matchLabels`.** Deferred; equality
  `matchLabels` covers the motivating use cases and keeps the API small. Richer
  selectors can be added later without breaking the field.
- **Allow `subGroupPlacement` with the `LeaderWorker` type.** Rejected for the
  alpha because placement targets worker pods; mixing the leader into a
  placement subgroup complicates TPU indexing and node-affinity semantics with
  no clear use case.
