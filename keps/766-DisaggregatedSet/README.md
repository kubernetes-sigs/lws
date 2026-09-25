# KEP-766: DisaggregatedSet

<!--
This KEP proposes adding DisaggregatedSet as a higher-level API for managing
disaggregated inference workloads using LeaderWorkerSet as the underlying
workload primitive.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [DisaggregatedSet API](#disaggregatedset-api)
  - [N-Dimensional Rolling Update Algorithm](#n-dimensional-rolling-update-algorithm)
    - [Issued work and available capacity](#issued-work-and-available-capacity)
    - [Capacity and pending-work bounds](#capacity-and-pending-work-bounds)
    - [Reconcile ordering and completion](#reconcile-ordering-and-completion)
  - [Example: Pipelining an 8P/4D Rollout](#example-pipelining-an-8p4d-rollout)
  - [Service Orchestration](#service-orchestration)
  - [Controller Architecture](#controller-architecture)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Extend LeaderWorkerSet with Multi-Template Support](#alternative-1-extend-leaderworkerset-with-multi-template-support)
  - [Alternative 2: Helm Chart or Kustomize Overlay](#alternative-2-helm-chart-or-kustomize-overlay)
  - [Alternative 3: External Controller Without CRD](#alternative-3-external-controller-without-crd)
  - [Alternative 4: Use LWS Partition Field Instead of Multiple LWS per Revision](#alternative-4-use-lws-partition-field-instead-of-multiple-lws-per-revision)
<!-- /toc -->

## Summary

This KEP proposes adding `DisaggregatedSet` as a new Custom Resource Definition (CRD) to the LeaderWorkerSet (LWS) project. DisaggregatedSet is a higher-level abstraction that orchestrates multiple LeaderWorkerSets with coordinated lifecycle management, specifically designed for disaggregated inference architectures where multiple roles (e.g., "prefill" and "decode") run on separate infrastructure.

Disaggregated serving is an optimization for LLM inference workloads that takes advantage of the fact that the phases of inference (prefill and decode) have different computational characteristics. State-of-the-art LLM serving frameworks such as [vLLM](https://github.com/vllm-project/vllm) and [SGLang](https://github.com/sgl-project/sglang) support this optimization.

DisaggregatedSet simplifies the deployment of disaggregated LLM inference by:
- Managing multiple LeaderWorkerSets (2-10 roles) as a single logical unit
- Providing coordinated N-dimensional rolling updates across all roles

## Motivation

Currently, deploying disaggregated inference workloads requires users to manually create and coordinate multiple separate LeaderWorkerSets. This leads to several challenges:

1. **Operational complexity**: Users must manually ensure all roles are updated together and handle failure scenarios across multiple resources.

2. **Rolling update coordination**: There is no built-in mechanism to coordinate rolling updates across roles, risking service disruption if one role is updated without the others.

3. **Service lifecycle**: Users must manually manage Services and ensure they only route traffic when all roles are ready.

4. **Configuration drift**: Without a unified resource, role configurations can drift apart, leading to subtle incompatibilities.

### Goals

1. **Unified Management**: Provide a single CRD that manages multiple LeaderWorkerSets (2-10 roles) as a cohesive unit.

2. **Coordinated Rolling Updates**: Implement an N-dimensional rolling update algorithm that advances roles in fractional lockstep, limits the difference in progress between roles, and respects per-role surge and availability constraints.

3. **Stateless Controller**: Design the controller to derive all state from observed resources, enabling safe restarts at any point.

### Non-Goals

1. **Auto-scaling support**: HPA/VPA integration or automatic scaling based on inference load metrics is out of scope.

2. **Multi-cluster federation**: Managing DisaggregatedSets across multiple Kubernetes clusters is not addressed.

3. **Custom workload backends**: Supporting backends other than LeaderWorkerSet (e.g., StatefulSet, Deployment) is not planned.

4. **Traffic management / routing**: Integration with service meshes or ingress controllers for traffic splitting during rollouts is out of scope.

## Proposal

We propose adding a new CRD called `DisaggregatedSet` that acts as a higher-level controller over LeaderWorkerSet resources. The DisaggregatedSet controller will:

1. Create and manage multiple LeaderWorkerSets (one per role, 2-10 roles supported)
2. Coordinate rolling updates using an N-dimensional algorithm
3. ~~Automatically create headless Services for each role per revision~~ (removed, see [Service Orchestration](#service-orchestration))

### Risks and Mitigations

**Risk**: The N-dimensional rolling update algorithm adds complexity that could lead to stuck rollouts.

**Mitigation**: The controller distinguishes a completed rollout from one that is temporarily unable to progress. The planner considers both requested and Ready replicas, so it returns only a step that is currently safe to execute. If readiness, surge, or availability leaves no valid step, the controller requeues and tries again instead of manufacturing progress outside those limits. The controller identifies revisions using the `disaggregatedset.x-k8s.io/revision` label and reconstructs rollout state from the existing LeaderWorkerSets, so reconciliation can continue after a controller restart.

**Risk**: Adding a new CRD increases the API surface and maintenance burden.

**Mitigation**: DisaggregatedSet is additive and does not modify existing LeaderWorkerSet behavior. Users who don't need disaggregated inference can continue using LeaderWorkerSet directly.

## Design Details

### DisaggregatedSet API

```go
// DisaggregatedSet is the Schema for the disaggregated sets API
type DisaggregatedSet struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   DisaggregatedSetSpec   `json:"spec"`
    Status DisaggregatedSetStatus `json:"status,omitempty"`
}

// DisaggregatedSetSpec defines the desired state of DisaggregatedSet
// +kubebuilder:validation:XValidation:rule="self.roles.all(r, self.roles.filter(s, s.name == r.name).size() == 1)",message="role names must be unique"
// +kubebuilder:validation:XValidation:rule="self.roles.all(r, r.replicas == 0) || self.roles.all(r, r.replicas > 0)",message="replicas must be zero for all roles or non-zero for all roles"
type DisaggregatedSetSpec struct {
    // Roles defines the list of roles (at least 2 required, maximum 10).
    // Each role has a unique name and its own configuration.
    // +kubebuilder:validation:MinItems=2
    // +kubebuilder:validation:MaxItems=10
    // +required
    Roles []DisaggregatedRoleSpec `json:"roles"`
}

// DisaggregatedRoleSpec defines the configuration for a disaggregated role.
type DisaggregatedRoleSpec struct {
    // Name is the unique identifier for this role.
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=63
    // +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
    // +required
    Name string `json:"name"`

    // LeaderWorkerSetTemplateSpec is embedded inline to inherit LWS template fields.
    // Note: RolloutStrategy.Type must be RollingUpdate (or empty) and
    // RolloutStrategy.RollingUpdateConfiguration.Partition must not be set.
    // DisaggregatedSet handles rollouts across roles and does not propagate
    // RolloutStrategy to the underlying LWS resources.
    leaderworkerset.LeaderWorkerSetTemplateSpec `json:",inline"`
}

// LeaderWorkerSetTemplateSpec describes the data a LeaderWorkerSet should have when created
// from a template. This type needs to be added to the LWS API (similar to PodTemplateSpec).
type LeaderWorkerSetTemplateSpec struct {
    // Metadata for the LWS CR. Labels and annotations are propagated to the LWS ObjectMeta.
    // Useful for Kueue integration (kueue.x-k8s.io/queue-name) and exclusive-topology
    // scheduling (leaderworkerset.sigs.k8s.io/exclusive-topology).
    // +optional
    metav1.ObjectMeta `json:"metadata,omitempty"`

    // Spec defines the LeaderWorkerSet configuration.
    // +optional
    Spec leaderworkerset.LeaderWorkerSetSpec `json:"spec,omitempty"`
}
```

**Naming Convention**: LeaderWorkerSets are named `{disaggregatedset-name}-{revision}-{role}` where:
- `revision` is a truncated hash of all role templates (ensures coordinated updates)
- `role` is the role name (e.g., `prefill`, `decode`)

**Labels**: The following labels are applied to managed LeaderWorkerSets:
- `disaggregatedset.x-k8s.io/name`: DisaggregatedSet name
- `disaggregatedset.x-k8s.io/role`: Role name (e.g., `prefill`, `decode`)
- `disaggregatedset.x-k8s.io/revision`: Template hash

### N-Dimensional Rolling Update Algorithm

During a rolling update, the controller replaces one revision with the target revision. The revision currently being replaced is the active old revision. Its replicas form the old side of the fractional plan, and the target revision's replicas form the new side. Each role is one dimension. The active old revision shrinks to zero while the target revision grows.

When no old revision is serving, the controller reconciles the current revision directly. This does not imply that the workloads are already stable or Ready: the controller may still need to create or scale their LWS objects.

Each managed LWS stores an `initial-replicas` annotation. While a revision is current, replica-only changes and external-scaler changes keep this value aligned with the revision's target replica count. When a newer revision makes it old, the value freezes and becomes that revision's `initialOld` baseline while the controller drains it. The LWS is deleted after it reaches zero and the target revision is Ready. If another revision interrupts its rollout, the annotation preserves the replica count it was intended to reach rather than its partially created Spec.

If an old LWS does not have a valid `initial-replicas` annotation, the controller stores its current Spec as the best available fallback before draining it. An explicit annotation value of `0` is valid and is not treated as missing.

Suppose a rollout from revision A to revision B is interrupted by revision C. Both A and B are now old. The controller processes only one of them at a time. A revision with no Ready replicas is selected first because removing it cannot reduce serving capacity. Otherwise, the newest old revision is selected, so B is drained before A.

While B is active, A is parked and remains unchanged. Fractional planning uses B's own `initial-replicas` value as `initialOld` and B's current Spec as `activeOldSpec`. Ready replicas in A reduce how much of C is needed during this phase. After B reaches zero, A becomes active and the controller plans the A to C phase. The controller never combines the `initial-replicas` values from A and B.

Parking does not remove A from safety accounting. `oldSpec` and `oldReady` sum the observed values across A and B because both revisions still occupy capacity and may serve traffic. Surge and availability limits use those aggregate values and C's full desired target.

Within each side, the planner uses discrete linear interpolation. Every role uses the same progress fraction. The resulting replica counts are rounded up to whole numbers.

Each side measures progress on its own fractional scale. For one side, `roleReplicaCounts` is the list of replica counts for its roles. A count can be zero when the whole DisaggregatedSet is scaled to zero, or when a role exists on only one side of the rollout because it was added or removed. `positiveRoleReplicaCounts` excludes those zero counts because they do not define a fractional window.

```
fractionalStepCount      = max(roleReplicaCounts)
smallestReplicaFraction  = 1 / max(roleReplicaCounts)
largestReplicaFraction   = 1 / min(positiveRoleReplicaCounts)
```

`fractionalStepCount` is the number of equal fractional steps between the start and end of one side. Fractional step `0` is the start and fractional step `fractionalStepCount` is the end, so there are `fractionalStepCount + 1` positions. A fractional step is not a reconcile iteration. The controller may remain at one fractional step or advance across more than one fractional step in a reconcile.

`smallestReplicaFraction` is the distance covered by one fractional step. It comes from the largest role because one replica is the smallest fraction of that role. In an `8P/4D` side, Prefill is the largest role, so one Prefill replica represents `1/8` of the rollout and creates eight equal fractional steps. `largestReplicaFraction` is the width of the coordination window. It comes from the smallest non-zero role because one replica is the largest fraction of that role. Decode is the smallest role in this example, so one Decode replica represents `1/4` of the rollout. The controller therefore allows at most `1/4` difference between role progress.

`newStepCount` is `fractionalStepCount` calculated from the new target counts. `oldStepCount` is `fractionalStepCount` calculated from the `initialOld` counts. At fractional step `k`, the replica count for one role is calculated with ceiling division:

```
newAtStep(k) = ceil(target * k / newStepCount)
oldAtStep(k) = ceil(initialOld * (oldStepCount - k) / oldStepCount)
```

The planner uses `leastAdvancedStep` to select the shared fractional step from the current replica counts. It calculates each role's growth or drain progress and returns the smallest step reached by any non-empty role. The planner then calculates the replica count for every role at that fractional step. Ceiling division keeps each old role above zero until the final fractional step. It also prevents a smaller role from getting more than one replica's worth of progress ahead. When multiple fractional steps produce the same replica count, the controller uses the latest one.

The following diagram shows every old-side step from the intended replica counts to zero. Each column is one fractional step. The coordination window is frozen over steps 5 through 7 for illustration. Roles do not need to occupy the same step; they only need to remain within the same window.

```
Frozen window: steps 5 through 7

fraction removed   0 --- 1/8 --- 2/8 --- 3/8 --- 4/8 --- [5/8 --- 6/8 --- 7/8] --- 1
fractional step    0 ---  1  ---  2  ---  3  ---  4  --- [ 5  ---  6  ---  7 ] --- 8
Prefill remaining  8 ---  7  ---  6  ---  5  ---  4  --- [ 3  --- 2*  ---  1 ] --- 0
Decode remaining   4 ---  4  ---  3  ---  3  ---  2  --- [2*  ---  1  ---  1 ] --- 0

* = current role position inside the frozen window
```

The distance between adjacent columns is `1/8`, the `smallestReplicaFraction`. The window is two columns wide, or `2/8 = 1/4`, the `largestReplicaFraction`. Decode=2 maps to step 5 and defines the window. Prefill is at step 6, so it may advance to step 7 but no further until Decode advances. The window is then recalculated.

This moving window is the fractional-lockstep guarantee. Roles can move by different replica counts, and they do not have to occupy the same fractional step. A planner step starting inside the window remains inside it. If observed state is already outside the window, the planner does not reverse applied work. It keeps the leading role at its current replica count and advances only lagging roles until they return to the window. For example, on a new `8P/4D` side, `6P/1D` represents 75% Prefill progress and 25% Decode progress. The gap exceeds the `1/4` window. The planner does not reduce Prefill from 6. It holds Prefill at 6 and allows Decode to grow to 2, bringing Decode to 50% and the gap back to `1/4`.

The fractional window does not replace rollout budgets. `MaxSurge` and `MaxUnavailable` remain hard limits for each role and may permit less movement than the window. Per-role limits can trim different parts of a candidate step, so the planner reapplies the window after applying those limits. If no mutation is currently safe, the controller waits for observed state to change. The per-role budgets do not provide an atomic availability guarantee across roles.

#### Issued work and available capacity

The controller deliberately distinguishes the desired replica count in the LWS Spec from its Ready status:

```
Spec  = work already issued to the cluster, including pods still starting
Ready = work that has completed startup and is available to serve
```

Spec drives the planner's progress calculation. Re-planning from Ready would reissue the same fractional step on every reconcile while a pod is starting. Ready instead controls how much additional work may be in flight, whether an old replica can be removed safely, and whether the rollout is complete.

Status can temporarily remain higher than Spec after a scale-down. The controller therefore counts only committed availability:

```
committedReady = min(status.readyReplicas, spec.replicas)
```

This prevents a terminating replica from authorizing another drain.

#### Capacity and pending-work bounds

`MaxSurge` and `MaxUnavailable` remain hard, absolute per-role limits. For each role the planner enforces:

```
roleReplicaCount  = max(initialOld, target)
surgeCeiling      = roleReplicaCount + MaxSurge
availabilityFloor = max(0, min(initialOld, target) - MaxUnavailable)

oldSpec + newSpec                    <= surgeCeiling
oldCommittedReady + newCommittedReady >= availabilityFloor
```

The proportional planner can intentionally use less than those raw limits to keep differently sized roles moving together. Let `budgetSteps` be the larger of the old and new side step counts. A raw per-role budget is projected onto the shared fraction scale as:

```
projected(role, budget) = ceil(roleReplicaCount * budget / budgetSteps)
```

The same projection defines the maximum new work allowed to be issued but not yet Ready:

```
pendingAllowance = projected(role, MaxSurge + MaxUnavailable)
newSpec - newCommittedReady <= pendingAllowance
```

This bounded window is what permits pipelining across slow pod starts. It does not grant every role the unscaled `MaxSurge + MaxUnavailable` sum. If independent pending bounds would separate role progress by more than `largestReplicaFraction`, faster roles wait at that coordination boundary.

#### Reconcile ordering and completion

One plan may contain both an old-side drain and new-side growth. Before returning the step, the planner limits old targets using committed Ready capacity and limits new targets using the surge and pending-readiness ceilings. It reapplies the coordination window after independent limits trim role targets. The executor then applies the floor-safe old drain before growing the new revision, so the two API updates cannot create a transient surge violation.

The safety limits may reduce both proposed targets back to their current Spec values. In that case, the plan contains no mutation. This means the rollout is temporarily blocked, not complete. The controller requeues and waits for readiness, capacity, or a configuration change. Completion is checked separately from the absence of a safe next step.

Interrupted rollouts process one old revision at a time and leave the others unchanged. A revision with no Ready replicas is removed first because it contributes no serving capacity. Otherwise, the newest old revision is selected, so the oldest revision is replaced last. A revision is retired as soon as all of its role Specs are zero. Stale status from its terminating pods neither blocks the next revision nor contributes availability.

The controller does not intentionally remove the last replica of one role while another role in the same old revision remains. It retires all roles together when safe. Otherwise, it first tries a partial drain that leaves at least one replica of every role, then replacement growth within the hard limits. If neither is possible, it requeues and emits an event.

A rollout is complete only when every old role Spec is zero, every new role Spec has reached its target, and every new role has at least its target number of committed Ready replicas.

### Example: Pipelining an 8P/4D Rollout

Consider a template-only update with `MaxSurge=2` and `MaxUnavailable=2`. Both sides have eight steps:

```
smallestReplicaFraction = 1/8
largestReplicaFraction  = 1/4
pendingAllowance(P)     = ceil(8 * 4 / 8) = 4
pendingAllowance(D)     = ceil(4 * 4 / 8) = 2
availabilityFloor(P/D)  = 6/2
surgeCeiling(P/D)       = 10/6
```

If every issued replica becomes Ready before the next observation, the Spec trajectory can be:

| Observation | Old P | Old D | New P | New D |
|-------------|------:|------:|------:|------:|
| Initial     | 8 | 4 | 0 | 0 |
| 1           | 6 | 3 | 2 | 1 |
| 2           | 4 | 2 | 4 | 2 |
| 3           | 2 | 1 | 6 | 3 |
| 4           | 0 | 0 | 8 | 4 |

The observations are planner fractional steps, not a promise that every cluster will expose exactly this sequence. Readiness, API observations, and interrupted updates may introduce additional reconciles.

The pending window changes the slow-start case materially. After observation 1, suppose the new `2P/1D` has `Ready=0P/0D`. Its pending work is therefore `2P/1D`, below the `4P/2D` pending allowance derived from `MaxSurge + MaxUnavailable`. The controller may issue another `2P/1D`, reaching a new Spec of `4P/2D` without waiting for the first batch to become Ready.

`MaxUnavailable` independently sets availability floors of `6P/2D`. If the remaining old revision has `Ready=6P/3D` while the new revision is still unready, no old Prefill may drain because Prefill is already at its floor. At most one old Decode may drain because Decode has one Ready replica above its floor. As new replicas become Ready, the Ready totals rise and permit further old replicas to drain.

A narrow coordination window still permits pipelining when roles advance together. For example, two roles of 20 replicas produce a window width of `1/20`. With `MaxSurge=2` and `MaxUnavailable=2`, a second `2/2` batch can still be issued while the first `2/2` batch is unready. The window prevents one role from moving too far ahead of the other; it does not require every issued batch to become Ready before more work is issued.

### Service Orchestration

> **Removed.** The controller used to create a headless Service per `(revision, role)`,
> named `{disaggregatedset-name}-{revision}-{role}-prv`, selecting that revision's pods
> for the role. Its only purpose was to let a custom load balancer count pods per revision
> through native EndpointSlices and gate traffic during a rollout. llm-d now implements
> revision gating with Kubernetes watches and label selectors over the Pods directly, so
> these Services carried complexity (and a DNS-1035 name budget) for no consumer, were
> never part of the supported surface (hence the `-prv` suffix), and could not be extended
> to upcoming features such as virtual roles. They are no longer created.
>
> Services left behind by an earlier controller are not deleted; they are garbage collected
> with the LeaderWorkerSet that owns them, or can be deleted by hand. Consumers that need
> per-revision, per-role Pod discovery should select on the
> `disaggregatedset.x-k8s.io/{name,role,revision,slice}` labels the controller stamps on
> every managed Pod.

### Controller Architecture

The controller is stateless: all state is derived from observed resources. The `initial-replicas` annotation preserves each revision's intended replica target across rolling updates. Owner references on managed LeaderWorkerSets ensure proper garbage collection.

### Test Plan

[X] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

#### Unit tests

- Rolling update planner: step computation, edge cases, constraint violations
- Executor: Spec/Ready separation, pending bounds, availability-safe drains, slow-role readiness, coordinated retirement, and staged interrupted rollouts
- API validation: role count, unique names, replica constraints

#### Integration tests

- DisaggregatedSet creation creates LeaderWorkerSets for all roles
- Template update triggers coordinated rolling update
- Interrupted rollout resumes correctly after controller restart
- Deletion cascades to owned resources

### Graduation Criteria

**Alpha (v0.1)**:
- DisaggregatedSet CRD with validation
- LeaderWorkerSet creation and ownership
- N-dimensional rolling update algorithm
- Comprehensive test coverage (>80%)
- Documentation and examples

**Beta (v0.2)**:
- Production usage feedback incorporated
- Metrics for observability

**Stable (v1.0)**:
- Performance optimization for large deployments
- Proven stability in production environments

## Implementation History

- 2026-03-05: Initial KEP draft
- 2026-03-22: Updated to reflect N-dimensional roles API
- 2026-03-23: Renamed "phase" to "role" throughout for semantic clarity
- 2026-09-25: Updated the rolling-update contract to cover fractional lockstep, readiness and availability bounds, staged interrupted rollouts, and durable intended replica counts.

## Drawbacks

1. **Increased Complexity**: Adding another CRD increases the API surface and learning curve for new users.

2. **Controller Overhead**: Managing an additional controller layer adds some overhead, though minimal for typical deployment sizes.

## Alternatives

### Alternative 1: Extend LeaderWorkerSet with Multi-Template Support

Instead of a new CRD, extend LeaderWorkerSet to support multiple pod templates with labels distinguishing "roles" (prefill vs decode).

**Rejected because**:
- Significantly changes LeaderWorkerSet semantics
- Makes the core LWS controller more complex
- Rolling update coordination would be harder to implement within a single resource

### Alternative 2: Helm Chart or Kustomize Overlay

Provide a Helm chart that creates multiple coordinated LeaderWorkerSets.

**Rejected because**:
- No runtime coordination for rolling updates
- Users would need to implement their own update strategy
- Services cannot be conditionally created based on workload readiness

### Alternative 3: External Controller Without CRD

Build an external controller that watches LeaderWorkerSets with specific labels and coordinates them.

**Rejected because**:
- Poor user experience (no single resource to manage)
- Harder to discover and use
- State management would be complex without a CRD

### Alternative 4: Use LWS Partition Field Instead of Multiple LWS per Revision

Instead of creating separate LeaderWorkerSets per revision (resulting in up to 2N LWS during updates for N roles), use the LWS `partition` field to perform in-place updates within a single LWS per role.

**How it would work**:
- DisaggregatedSet creates exactly N LWS: one per role
- Rolling updates manipulate the `partition` field on each LWS to progressively update groups
- Groups with ordinal `>= partition` get the new template; groups `< partition` remain on old

**Why we chose multiple LWS per revision instead**:

1. **Revision-aware traffic routing**: DisaggregatedSet is designed for disaggregated inference, where a load balancer must route requests to backends whose counterparts are on the **same revision**. With separate LWS (and Service) per revision, each pod's revision is explicit via labels (`disaggregatedset.x-k8s.io/revision`). The load balancer can count backends per revision across all role pools and distribute traffic proportionally. With partition-based updates, pods within the same LWS have different templates based on ordinal, making revision-aware routing significantly more complex.

2. **LWS as a read-only resource**: Treating LWS as a read-only resource (similar to how Deployment treats ReplicaSet) makes more sense for this use case. During a coordinated rollout, you want to update roles at different paces depending on the step—it's a tied update across N dimensions. This level of control is difficult to achieve with partition, which operates on a single LWS independently.

3. **Ops observability**: Separate LWS per revision is simpler for ops observability. You can see directly at which stage your update is, since you can see the version right away during updates (e.g., "old-prefill: 2 replicas, new-prefill: 3 replicas") rather than inspecting partition boundaries within a single LWS.

**Trade-offs acknowledged**:
- **Resource overhead**: Up to 2N LWS exist during updates vs. N. However, LWS is a lightweight coordination resource; the actual pod count remains the same.
- **Complexity**: The N-dimensional rolling update algorithm is more complex than coordinating N partition values. However, this complexity is encapsulated in the DisaggregatedSet controller.

**Potential LWS improvements that could enable partition-based approach**:
- Pod-level revision labels (independent of LWS name) would help with traffic routing
- Revision-aware service selectors at the LWS level
- See also: [#710](https://github.com/kubernetes-sigs/lws/issues/710) for related discussion on revision tracking
