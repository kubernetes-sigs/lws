# KEP-893: DisaggregatedSet Integration with CompositePodGroup

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1](#story-1)
    - [Story 2](#story-2)
  - [Scheduling Hierarchy](#scheduling-hierarchy)
  - [One Root per Slice and Revision](#one-root-per-slice-and-revision)
  - [User-Facing API](#user-facing-api)
  - [Notes and Caveats](#notes-and-caveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Generated Object Shapes](#generated-object-shapes)
  - [DisaggregatedSet as Root Controller](#disaggregatedset-as-root-controller)
  - [Example](#example)
  - [Open Questions](#open-questions)
  - [Test Plan](#test-plan)
    - [Unit Tests](#unit-tests)
    - [Integration Tests](#integration-tests)
    - [End-to-End Tests](#end-to-end-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

This KEP designs how `DisaggregatedSet` ([KEP-766][kep766]) integrates with the
upstream `CompositePodGroup` (CPG) API so a disaggregated serving unit is
gang-scheduled as a coordinated hierarchy instead of a flat `PodGroup`.

`DisaggregatedSet` owns several `LeaderWorkerSet` (LWS) objects, one per role,
for example `prefill` and `decode`. [KEP-666][kep666] already compiles a single
LWS into a Workload and a three-level LWS-owned hierarchy, gated by
`WorkloadAwareScheduling`, and names a future `DisaggregatedSet` root
controller as the owner of the role and slice levels above delegated LWS
subtrees. This KEP is that follow-up. It defines the role and slice levels, the
`DisaggregatedSet` authoring surface, and the delegation to each child LWS.

The design stays provisional and sits behind the separate nested-scheduling LWS
gate from KEP-666, because the runtime `CompositePodGroup` API is
`scheduling.k8s.io/v1alpha3`, alpha, and off by default in Kubernetes 1.37. It
targets Beta in 1.38 ([kubernetes/enhancements#6012][cpg]).

## Motivation

Deploying disaggregated inference today means running one `LeaderWorkerSet` per
role and relying on per-LWS gang scheduling, with no coordination across roles.
A flat `PodGroup` with a single `minCount` cannot express two requirements that
disaggregated serving needs:

- A slice is useful only when both roles are schedulable together. Prefill
  that comes up without decode, or the reverse, reserves GPUs that cannot
  serve.
- Prefill and decode need different minimum availability, different topology
  constraints, and different disruption behavior. One `minCount` cannot encode
  per-role policy.

`CompositePodGroup` was designed with LWS and JobSet as intended consumers. A
root group can represent one serving slice, role and replica groups sit below
it, and leaf `PodGroup`s carry the leader and worker pod gangs. KEP-666 defers
the role and slice levels to this design; the API discussion in
[#894][api894] proposed the concrete shape.

### Goals

- Define the `DisaggregatedSet` to `CompositePodGroup` hierarchy, including the
  slice root, per-role, per-replica, and leader/worker leaf levels.
- Define an optional `DisaggregatedSet` authoring surface that compiles to that
  hierarchy, so users do not author CPG objects directly.
- Keep `DisaggregatedSet` as the root Workload controller and delegate each
  child LWS subtree through the KEP-666 parent-linkage annotations.
- Stay forward-compatible with the per-role topology work in [#892][issue892]
  (DisaggregatedSet topology-aware scheduling).

### Non-Goals

- Implementing the controller. This KEP is a design. Implementation is a
  follow-up once the direction and open questions are settled.
- Weighted prefill-to-decode ratios. `minGroupCount` on a `CompositePodGroup`
  is an unweighted child count and cannot express a ratio. Ratio, scale, and
  rollout semantics are tracked separately (see [Open Questions](#open-questions)).
- Changing the `DisaggregatedSet` slice or rollout model from [KEP-766][kep766]
  and [KEP-846][kep846]. Adding or removing a slice must not trigger a rollout.
- A bring-your-own `Workload` or `CompositePodGroup` object mode. Parent-linkage
  annotations are the supported delegation path, as in KEP-666.
- Defining the single-LWS `spec.scheduling` field or the flat Phase 1 path.
  Those belong to KEP-666.

## Proposal

`DisaggregatedSet` gains an optional scheduling surface. When it is set, the
`DisaggregatedSet` controller owns one Workload and compiles one
`CompositePodGroup` tree per slice, then delegates each role's LWS subtree to
the child LWS controller through the KEP-666 annotations. When the surface is
absent, or the feature gate or runtime API is unavailable, `DisaggregatedSet`
behaves as it does today.

### User Stories

#### Story 1

As an operator of a disaggregated vLLM or SGLang deployment, I want each slice,
meaning its prefill replicas together with its decode replicas, to be admitted
as a unit within one zone, so a slice never comes up with only one role
scheduled.

#### Story 2

As an operator, I want prefill to require a minimum number of complete replicas
and decode to require its own minimum, each with its own topology constraint,
so the scheduler enforces role-specific placement instead of a single flat
minimum.

### Scheduling Hierarchy

One slice compiles to a four-level tree. The shape and the policy values below
are the proposal from [#894][api894]:

```text
CompositePodGroup/ds-0                  gang(minCount=2)  topology=zone   single   # slice root
  ├── CompositePodGroup/ds-prefill      gang(minCount=P)  topology=block  single   # role
  │   ├── CompositePodGroup/ds-prefill-0            gang(minCount=2)  topology=rack  all   # replica
  │   │   ├── PodGroup/ds-prefill-0-leader          gang(minCount=1)                 all   # leaf
  │   │   └── PodGroup/ds-prefill-0-worker          gang(minCount=size-1) topology=nvlink all
  │   └── CompositePodGroup/ds-prefill-1            gang(minCount=2)  topology=rack  all
  │       ├── PodGroup/ds-prefill-1-leader          gang(minCount=1)                 all
  │       └── PodGroup/ds-prefill-1-worker          gang(minCount=size-1) topology=nvlink all
  └── CompositePodGroup/ds-decode       gang(minCount=D)  topology=block  single
      └── CompositePodGroup/ds-decode-0             gang(minCount=2)  topology=rack  all
          ├── PodGroup/ds-decode-0-leader           gang(minCount=1)                 all
          └── PodGroup/ds-decode-0-worker           gang(minCount=size-1) topology=nvlink all
CompositePodGroup/ds-1                  gang(minCount=2)  topology=zone   single   # next slice
...
```

- The slice root is a composite group over the role subtrees. Its
  `minCount` equals the number of roles, so every role in the slice is admitted
  together. Disruption is `single` so roles are disrupted independently.
- Each role is a composite group over that role's replicas. Its `minCount` is
  the per-role minimum of complete replicas.
- Each replica is a composite group over the leader and worker leaves, reusing
  the KEP-666 per-replica CPG. Disruption is `all` so a replica's pods move as
  one unit.
- The leaves are the leader and worker `PodGroup`s, the same leaves KEP-666
  materializes for a single LWS.

The role and slice levels are new. The replica and leaf levels are the
KEP-666 Phase 2 hierarchy for each child LWS.

### One Root per Slice and Revision

Each slice gets its own root `CompositePodGroup` (`ds-0`, `ds-1`, and so on),
not one shared root. Homogeneous slices reuse one configuration, not one
object. A separate root per slice preserves two `DisaggregatedSet` invariants
from [KEP-766][kep766] and [KEP-846][kep846]:

1. Scaling slice count adds or removes a root CPG. It never mutates an
   immutable `minCount` on a shared root, which would force recreating the tree
   and break the rule that changing slice count does not trigger a rollout.
2. Each slice stays an independent rolling-update and disruption domain, with
   the prefill and decode of one slice co-located.

Per the [#894][api894] consensus with the working group, the controller spawns
one independent root CPG per slice and revision.

### User-Facing API

The surface mirrors the KEP-666 `spec.scheduling` building blocks and adds a
role level. The LWS and replica levels use the composite variants; the leader
and worker levels use the pod-group variants. The exact field names follow the
upstream 1.37 types KEP-666 already embeds
(`WorkloadCompositePodGroupSchedulingPolicy`,
`WorkloadCompositePodGroupSchedulingConstraints`, and
`WorkloadCompositePodGroupDisruptionMode`). The YAML below is the proposal from
[#894][api894]:

```yaml
scheduling:
  # Slice root: the whole slice
  schedulingPolicy: { gang: {} }            # minCount derived from role count
  schedulingConstraints: { topology: [{ level: topology.kubernetes.io/zone }] }
  disruptionMode: { single: {} }

  rolePolicies:
    - name: prefill
      targetRoles: ["prefill"]
      # Role: the set of prefill replicas
      schedulingPolicy: { gang: { minCount: 2 } }   # at least 2 complete replicas
      schedulingConstraints: { topology: [{ level: topology.k8s.io/block }] }
      disruptionMode: { single: {} }
      replicaPolicy:
        # Replica: 1 leader + (size-1) workers
        schedulingPolicy: { gang: {} }              # minCount derived = 2
        schedulingConstraints: { topology: [{ level: topology.k8s.io/rack }] }
        disruptionMode: { all: {} }
        leaderPolicy:
          # Leaf
          schedulingPolicy: { gang: {} }            # minCount = 1
          disruptionMode: { all: {} }
        workerPolicy:
          # Leaf
          schedulingPolicy: { gang: {} }            # minCount = size - 1
          schedulingConstraints: { topology: [{ level: nvidia.com/nvlink-domain }] }
          disruptionMode: { all: {} }
    - name: decode
      targetRoles: ["decode"]
      schedulingPolicy: { gang: { minCount: 1 } }
      schedulingConstraints: { topology: [{ level: topology.k8s.io/block }] }
      # same replica, leader, and worker structure as prefill
```

The controller derives the root, replica, and leaf `minCount` values from role
count and LWS `size`. Users set only the role-level minimums and the topology
and disruption intent. Whether this surface is a new `spec.scheduling` field on
`DisaggregatedSet` or a different placement is one of the
[Open Questions](#open-questions).

### Notes and Caveats

- The runtime `CompositePodGroup` API is `scheduling.k8s.io/v1alpha3`, alpha,
  and off by default in 1.37. The design must no-op cleanly when it is off,
  matching the KEP-666 discovery and gating behavior.
- LWS-managed templates must not pre-set `pod.spec.schedulingGroup`. Delegation
  is through parent annotations, consistent with KEP-666.
- `minGroupCount` is an unweighted child count, so the slice root cannot encode
  a prefill-to-decode ratio. See [Non-Goals](#non-goals) and
  [Open Questions](#open-questions).

### Risks and Mitigations

- Upstream API churn. `CompositePodGroup` is alpha and its field names may
  change before Beta. Keeping the `DisaggregatedSet` surface mapped to the
  KEP-666 building blocks, rather than to raw CPG objects, limits the blast
  radius, and the whole feature stays gated.
- Getting ahead of the maintainers. An earlier attempt at this KEP (#920) was
  closed as premature. This KEP stays provisional, rides the separate nested
  LWS gate, and asks for direction on the open questions before implementation.
- Overlap with topology-aware scheduling (#892). Topology constraints attach to
  the same boundaries this KEP defines. The boundaries are defined here; the
  constraint authoring question is resolved jointly with #892.

## Design Details

### Generated Object Shapes

For a `DisaggregatedSet` with roles `prefill` and `decode` and `slices` set,
the controller owns one Workload and, per slice, one tree:

```text
DisaggregatedSet
└── Workload (owned by DisaggregatedSet)
    ├── slice CompositePodGroup ds-0 (owned by DisaggregatedSet)
    │   ├── prefill role CompositePodGroup (owned by DisaggregatedSet)
    │   │   └── per-replica CPGs and leader/worker PodGroups (owned by the prefill LWS)
    │   └── decode role CompositePodGroup (owned by DisaggregatedSet)
    │       └── per-replica CPGs and leader/worker PodGroups (owned by the decode LWS)
    └── slice CompositePodGroup ds-1 ...
```

The `DisaggregatedSet` controller owns the Workload and the slice and role
CPGs. Each child LWS continues to own its per-replica CPGs and leader/worker
leaves, following KEP-666 Phase 2.

### DisaggregatedSet as Root Controller

KEP-666 reserves Workload ownership for the root-most registered controller and
gives a child LWS two parent-linkage annotations:

- `scheduling.k8s.io/group-template-name` selects the Workload template for the
  child. In the nested path this names the LWS root composite template, so the
  child LWS materializes its own root CPG and the replica and role descendants
  below it.
- `scheduling.k8s.io/parent-compositepodgroup` names the parent CPG instance
  the LWS root attaches to. The child LWS root sets
  `parentCompositePodGroupName` from this annotation.

`DisaggregatedSet` is the root controller. It owns the Workload, creates the
slice and role CPGs, and sets both annotations on each child LWS so each role
subtree attaches under the correct role CPG. The annotations are
controller-to-controller linkage and are immutable once the child LWS scheduling
is set. A missing template, an invalid owner chain, or a missing parent CPG is
rejected, and pods are blocked until the Workload and the required parent CPG
exist.

### Example

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: disaggregated-serving
spec:
  slices: 2
  roles:
    - name: prefill
      spec:
        replicas: 2
        leaderWorkerTemplate:
          size: 4           # 1 leader + 3 workers
    - name: decode
      spec:
        replicas: 1
        leaderWorkerTemplate:
          size: 2           # 1 leader + 1 worker
  # Proposed scheduling surface (see User-Facing API)
  scheduling:
    schedulingPolicy: { gang: {} }
    schedulingConstraints: { topology: [{ level: topology.kubernetes.io/zone }] }
    disruptionMode: { single: {} }
    rolePolicies:
      - name: prefill
        targetRoles: ["prefill"]
        schedulingPolicy: { gang: { minCount: 2 } }
        schedulingConstraints: { topology: [{ level: topology.k8s.io/block }] }
        disruptionMode: { single: {} }
        replicaPolicy:
          schedulingPolicy: { gang: {} }
          schedulingConstraints: { topology: [{ level: topology.k8s.io/rack }] }
          disruptionMode: { all: {} }
          leaderPolicy:
            schedulingPolicy: { gang: {} }
            disruptionMode: { all: {} }
          workerPolicy:
            schedulingPolicy: { gang: {} }
            schedulingConstraints: { topology: [{ level: nvidia.com/nvlink-domain }] }
            disruptionMode: { all: {} }
      - name: decode
        targetRoles: ["decode"]
        schedulingPolicy: { gang: { minCount: 1 } }
        schedulingConstraints: { topology: [{ level: topology.k8s.io/block }] }
        disruptionMode: { single: {} }
        replicaPolicy:
          schedulingPolicy: { gang: {} }
          schedulingConstraints: { topology: [{ level: topology.k8s.io/rack }] }
          disruptionMode: { all: {} }
          leaderPolicy:
            schedulingPolicy: { gang: {} }
            disruptionMode: { all: {} }
          workerPolicy:
            schedulingPolicy: { gang: {} }
            schedulingConstraints: { topology: [{ level: nvidia.com/nvlink-domain }] }
            disruptionMode: { all: {} }
```

### Open Questions

These need a maintainer and working-group decision before the KEP moves from
provisional to implementable.

- Weighted ratios. `minGroupCount` cannot express a prefill-to-decode ratio.
  Keep ratio, scale, and rollout semantics out of this KEP and in a
  `DisaggregatedSet` follow-up, as KEP-666 suggests, or in scope here.
- Placement of this KEP. Keep it standalone, or fold it into [KEP-766][kep766]
  or the sub-roles work in [KEP-960][kep960].
- Authoring surface. Whether the proposed `scheduling` block is a new
  `spec.scheduling` field on `DisaggregatedSet`, and how it relates to per-role
  sub-roles ([KEP-960][kep960]).
- Overlap with #892. The same block carries topology constraints. Decide
  whether the authoring question, meaning constraints authored by
  `DisaggregatedSet`, copied onto the Workload, or derived from role policy, is
  resolved here or in #892.
- Feature gate. KEP-666 plans a separate default-off LWS gate for nested
  scheduling. Decide whether `DisaggregatedSet` integration rides that gate or
  needs its own.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes
necessary to implement this enhancement.

#### Unit Tests

- Compile the scheduling surface into the four-level tree for homogeneous
  slices, including the derived root, replica, and leaf `minCount` values.
- One-root-per-slice behavior: adding or removing a slice adds or removes a
  root CPG without mutating existing roots.
- No-op when the feature gate or the runtime `CompositePodGroup` API is off.

#### Integration Tests

- A `DisaggregatedSet` with the scheduling surface produces the expected
  Workload, slice CPGs, role CPGs, and the parent-linkage annotations on each
  child LWS.
- A role `size` or `minCount` change updates the leaf `minCount` without
  recreating the tree or triggering a rollout.

#### End-to-End Tests

- With the runtime API enabled and a gang-capable scheduler, a slice is admitted
  only when both role subtrees meet their minimums; a partial slice stays
  Pending.

### Graduation Criteria

**Provisional to Implementable**

- The open questions are resolved and the authoring surface is agreed by
  reviewers.

**Alpha**

- Behind the separate nested-scheduling LWS gate, default off.
- Role and slice levels compile to slice and role CPGs above the KEP-666
  per-replica CPGs and leader/worker leaves.
- Unit and integration coverage for compilation, one-root-per-slice behavior,
  and in-place `minCount` updates.

**Beta**

- Aligned with the `CompositePodGroup` Beta API.
- End-to-end coverage with a gang-capable scheduler.
- The topology-constraint authoring question with #892 is resolved.

## Implementation History

- 2026-10-23: Provisional KEP drafted from the direction in [#893][issue893],
  the Future DisaggregatedSet Integration section of [KEP-666][kep666], and the
  API proposal in [#894][api894].

## Drawbacks

- It adds an authoring surface tied to an alpha upstream API that may still
  change. The feature gate and the mapping to the KEP-666 building blocks limit
  the exposure.
- A flat `PodGroup` is simpler for users who do not need per-role minimums.
  Those users leave the scheduling surface unset and keep current behavior.

## Alternatives

- A flat `PodGroup` per role. Simpler, but it cannot express the slice-level
  requirement that both roles are admitted together, nor per-replica or
  per-leaf topology. It remains the behavior when the scheduling surface is
  unset.
- A flat `CompositePodGroup` with one shared root across slices. Scaling slices
  would mutate an immutable `minCount` and force recreating the tree, which
  breaks the rule that slice count changes do not trigger a rollout.
- Authoring raw `Workload` or `CompositePodGroup` objects directly. Rejected
  for this KEP; it needs a separate design, consistent with KEP-666.

[kep666]: https://github.com/kubernetes-sigs/lws/tree/main/keps/666-gang-scheduling-in-lws
[kep766]: https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet
[kep846]: https://github.com/kubernetes-sigs/lws/tree/main/keps/846-disaggregatedset-slices
[kep960]: https://github.com/kubernetes-sigs/lws/tree/main/keps/960-DisaggregatedSet-subroles
[issue893]: https://github.com/kubernetes-sigs/lws/issues/893
[issue892]: https://github.com/kubernetes-sigs/lws/issues/892
[api894]: https://github.com/kubernetes-sigs/lws/issues/894#issuecomment-5244163695
[cpg]: https://github.com/kubernetes/enhancements/issues/6012
