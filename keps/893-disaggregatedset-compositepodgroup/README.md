# KEP-893: Design DisaggregatedSet Integration with CompositePodGroup

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Coordinated Gang Scheduling for Prefill and Decode](#story-1-coordinated-gang-scheduling-for-prefill-and-decode)
  - [Notes and Constraints](#notes-and-constraints)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [CompositePodGroup Shape for DisaggregatedSet](#compositepodgroup-shape-for-disaggregatedset)
  - [Controller Responsibilities](#controller-responsibilities)
  - [Lifecycle Management](#lifecycle-management)
  - [Interaction with Rolling Updates](#interaction-with-rolling-updates)
  - [Feature Gate](#feature-gate)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Flat PodGroup with combined minCount](#alternative-1-flat-podgroup-with-combined-mincount)
  - [Alternative 2: Separate PodGroups per role without a root group](#alternative-2-separate-podgroups-per-role-without-a-root-group)
<!-- /toc -->

## Summary

This KEP designs how `DisaggregatedSet` should integrate with the upstream
`CompositePodGroup` API (tracked in
[kubernetes/enhancements#6012](https://github.com/kubernetes/enhancements/issues/6012)).

A `CompositePodGroup` models a hierarchical scheduling unit with a root group representing
the whole serving workload and leaf groups representing individual roles (`prefill`,
`decode`). DisaggregatedSet maps naturally onto this hierarchy and should create and
manage a `CompositePodGroup` for each serving unit it owns.

This KEP is a follow-up to KEP-766 (DisaggregatedSet) and KEP-407 (Gang Scheduling in LWS)
and is closely related to KEP-892 (TAS for DisaggregatedSet).

## Motivation

A flat `PodGroup` with a single `minCount` cannot faithfully represent disaggregated
serving requirements:

- Prefill and decode pods may need different minimum availability thresholds (e.g. at least
  4 prefill pods **and** at least 8 decode pods must be scheduled before any traffic is
  routed).
- A flat group forces the operator to pick a single number, which either over-constrains
  one role or under-constrains the other.
- There is no structured way to express that the two role groups must become schedulable as
  a coordinated unit — either both roles are placed, or neither is.

`CompositePodGroup` was designed for exactly this pattern. Integrating DisaggregatedSet
with it gives operators a clean, declarative way to express cross-role gang requirements
without manual PodGroup management.

### Goals

1. Design how the DisaggregatedSet controller creates and owns a `CompositePodGroup`
   object (one per DisaggregatedSet).
2. Define the mapping from `DisaggregatedRoleSpec` fields to `PodGroupTemplate` fields
   inside the `CompositePodGroup`.
3. Specify lifecycle rules: creation on DS admission, update on template changes, deletion
   via owner references.
4. Define how rolling updates interact with `CompositePodGroup` (one CPG per revision pair
   or one CPG per DS?).
5. Provide an example manifest showing the end-to-end shape.

### Non-Goals

1. Implementing the `CompositePodGroup` API itself (upstream concern).
2. Modifying existing LeaderWorkerSet gang scheduling (KEP-407).
3. Topology constraints — covered in KEP-892.
4. HPA / autoscaling.

## Proposal

The DisaggregatedSet controller creates a `CompositePodGroup` object named after the
DisaggregatedSet (`{ds-name}`). The root `CompositePodGroupTemplate` represents the
serving unit; each role becomes a leaf `PodGroupTemplate` with its own `minCount`.

The controller sets `ownerReferences` on the `CompositePodGroup` to the DisaggregatedSet
so it is garbage-collected on deletion.

### User Stories

#### Story 1: Coordinated Gang Scheduling for Prefill and Decode

As a platform engineer, I deploy a disaggregated vLLM cluster with 4 prefill replicas and
8 decode replicas. I want the scheduler to guarantee that both role groups are placed
before any traffic is routed. With a flat PodGroup this is impossible to express cleanly.

With `CompositePodGroup` integration:

```yaml
apiVersion: scheduling.k8s.io/v1alpha3
kind: CompositePodGroup
metadata:
  name: vllm-serving
  ownerReferences:
    - kind: DisaggregatedSet
      name: vllm-serving
spec:
  compositePodGroupTemplates:
    - name: serving-root
      schedulingPolicy:
        gang:
          minGroupCount: 2     # both prefill and decode groups must be placed
      podGroupTemplates:
        - name: prefill
          schedulingPolicy:
            gang:
              minCount: 4
        - name: decode
          schedulingPolicy:
            gang:
              minCount: 8
```

The scheduler admits the serving unit only when it can place at least 4 prefill pods **and**
8 decode pods. This prevents partial admission.

### Notes and Constraints

- The `CompositePodGroup` API (upstream `scheduling.k8s.io/v1alpha3`) is under active
  development. Field names and versions in this KEP are illustrative and will be updated
  to track the upstream API as it stabilises.
- This KEP does not depend on KEP-892 but the two KEPs compose naturally: KEP-892 adds
  `topology` constraints inside each `PodGroupTemplate` while this KEP defines the
  hierarchical structure.

### Risks and Mitigations

**Risk 1**: `CompositePodGroup` upstream API is still provisional and field shapes may
change before beta.

**Mitigation**: The feature is gated behind `DisaggregatedSetCompositePodGroup` (alpha,
disabled by default). The controller detects whether the `CompositePodGroup` CRD is
installed and skips creation if not. Existing flat `PodGroup` gang scheduling (KEP-407)
continues to work unchanged.

**Risk 2**: A mismatch between the `minGroupCount` on the root group and the actual
number of role groups causes scheduling deadlocks.

**Mitigation**: The controller always sets `minGroupCount` to `len(ds.Spec.Roles)` — it
is derived automatically and never exposed as a user field. The validation webhook rejects
any manual override attempt.

**Risk 3**: During rolling updates two sets of role groups co-exist (old and new revision).
Creating a new `CompositePodGroup` per revision pair would multiply CPG objects.

**Mitigation**: See [Interaction with Rolling Updates](#interaction-with-rolling-updates)
below.

## Design Details

### CompositePodGroup Shape for DisaggregatedSet

Given a `DisaggregatedSet` with roles `[prefill, decode]`, the controller produces:

```yaml
apiVersion: scheduling.k8s.io/v1alpha3
kind: CompositePodGroup
metadata:
  name: <ds-name>
  namespace: <ds-namespace>
  ownerReferences:
    - apiVersion: disaggregatedset.x-k8s.io/v1
      kind: DisaggregatedSet
      name: <ds-name>
      controller: true
      blockOwnerDeletion: true
spec:
  compositePodGroupTemplates:
    - name: serving-root
      schedulingPolicy:
        gang:
          minGroupCount: <len(roles)>   # auto-derived, e.g. 2
      podGroupTemplates:
        - name: <role.name>             # e.g. "prefill"
          schedulingPolicy:
            gang:
              minCount: <role.spec.replicas * role.spec.leaderWorkerTemplate.size>
          # topology constraints from KEP-892 go here once both KEPs land
```

The `minCount` for each leaf group is `replicas × size` — the total number of pods that
must be scheduled for that role to be available.

### Controller Responsibilities

The controller reconciles the `CompositePodGroup` in the same reconcile loop as the
managed LeaderWorkerSets:

1. **Create**: On first reconcile after DS admission, create the `CompositePodGroup` if it
   does not exist.
2. **Update**: On any change to `roles[*].spec.replicas` or `leaderWorkerTemplate.size`,
   recompute `minCount` per leaf and patch the `CompositePodGroup`.
3. **Delete**: Handled automatically via `ownerReferences`; no explicit delete logic needed.

### Lifecycle Management

| Event | CPG action |
|-------|------------|
| DS created | CPG created with `ownerReferences` |
| DS `replicas` changed | CPG leaf `minCount` patched |
| DS role added / removed | CPG leaf list patched (add or remove entry) |
| DS deleted | CPG garbage collected |
| DS all-zero replicas | CPG leaf `minCount` set to 0 (scheduler ignores zero groups) |

### Interaction with Rolling Updates

During a DisaggregatedSet rolling update, old-revision and new-revision LWS objects
co-exist. The `CompositePodGroup` must reflect the **target** state (new revision) so
the scheduler can plan ahead for the new pods.

Strategy: **single CPG per DS, always reflecting the desired target replica count**.

- Old revision pods already have PodGroup associations from the previous reconcile cycle.
- The CPG `minCount` is updated to the new target on the first step of the rollout.
- This allows the scheduler to reserve capacity for the new pods before old pods are
  drained.

An alternative per-revision CPG strategy is discussed in
[Alternative 2](#alternative-2-separate-podgroups-per-role-without-a-root-group) and
rejected for the reasons stated there.

### Feature Gate

```go
// DisaggregatedSetCompositePodGroup enables CompositePodGroup creation for
// DisaggregatedSet objects. Requires the CompositePodGroup CRD to be installed.
DisaggregatedSetCompositePodGroup featuregate.Feature = "DisaggregatedSetCompositePodGroup"
```

The feature gate is `false` by default at alpha. When disabled, the controller does not
attempt to create or update any `CompositePodGroup` objects.

### Test Plan

#### Unit tests

- `buildCompositePodGroup`: correct `minCount` for various `replicas × size` combinations.
- `minCount` when `replicas == 0`: leaf `minCount` is 0.
- Role add / remove: CPG leaf list updated correctly.
- Feature gate disabled: CPG reconcile path skipped entirely.

#### Integration tests

- DS creation with feature gate enabled: `CompositePodGroup` created with correct
  `minGroupCount` and leaf `minCount` values.
- DS replica scale-up: CPG `minCount` patched.
- DS deletion: CPG garbage collected via owner references.
- CRD not installed: controller logs warning, continues reconcile without error.

#### e2e tests

- (Deferred until upstream `CompositePodGroup` CRD is available in a reference
  distribution.)
- Smoke test with a scheduler that supports `CompositePodGroup`: both roles scheduled
  together or not at all.

### Graduation Criteria

**Alpha (v0.9)**:
- `DisaggregatedSetCompositePodGroup` feature gate added (disabled by default).
- Controller creates / updates / inherits deletion of `CompositePodGroup`.
- Unit and integration coverage > 80%.
- Example manifest published in `site/` docs.

**Beta (v1.0)**:
- Upstream `CompositePodGroup` API reaches beta.
- Feature gate enabled by default.
- e2e coverage with at least one reference scheduler.

**Stable (v1.1)**:
- Feature gate removed.
- Stable integration with upstream API.

## Implementation History

- 2026-07-08: Initial KEP draft (tracks issue #893).

## Drawbacks

1. **Upstream API dependency**: The `CompositePodGroup` API is still evolving. Any field
   rename in the upstream API requires a corresponding update in the DisaggregatedSet
   controller.

2. **Scheduler requirement**: `CompositePodGroup` semantics are only meaningful if the
   cluster runs a scheduler that understands them. On clusters using the default
   `kube-scheduler` without extensions, the CPG object exists but has no scheduling
   effect, which may confuse operators.

## Alternatives

### Alternative 1: Flat PodGroup with combined minCount

Create a single flat `PodGroup` with `minCount = sum(role.minCount for all roles)`.

**Rejected because**: This prevents the scheduler from distinguishing which pod belongs to
which role. The scheduler cannot enforce per-role minimum counts; it only knows the global
total. If one role is fully scheduled but another is not, the flat group may still report
`minCount` satisfied.

### Alternative 2: Separate PodGroups per role without a root group

Create one `PodGroup` per role (matching the existing KEP-407 approach for LWS) but
without a root `CompositePodGroup`.

**Rejected because**: Without a root group the scheduler has no way to know that the
prefill and decode groups must be co-admitted. Each group is evaluated independently;
the scheduler can admit prefill but not decode (or vice versa), which leaves a
partially-running serving unit.
