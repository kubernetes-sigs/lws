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
3. Automatically create headless Services for each role per revision

### Risks and Mitigations

**Risk**: The N-dimensional rolling update algorithm adds complexity that could lead to stuck rollouts.

**Mitigation**: The controller distinguishes a completed rollout from one that is temporarily unable to progress. If it is waiting for replicas to become Ready, it requeues and tries again. If a zero-surge rollout cannot progress and no replicas are still starting, it permits a scale-down that respects `maxUnavailable` to unblock the next step. The controller identifies revisions using the `disaggregatedset.x-k8s.io/revision` label and reconstructs rollout state from the existing LeaderWorkerSets, so reconciliation can continue after a controller restart.

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

During a rolling update, the controller replaces one set of role replicas with another. The replicas being replaced form the old side. The replacement replicas form the new side. Each role is one dimension. The old side shrinks to zero while the new side grows to its target.

Each managed LWS stores an `initial-replicas` annotation. The annotation records the baseline used by the controller if that revision becomes old. The revision selected by the current DisaggregatedSet template is the target revision. Replica-only changes and external-scaler changes keep its annotation aligned with its target replica count. After that revision's rollout completes, the annotation matches its replica count. That value becomes `initialOld` when a later rollout starts. If a new revision interrupts the rollout before completion, the annotation instead preserves the interrupted revision's intended replica count: the number of replicas it would have reached if its rollout had completed.

Suppose a rollout from revision A to revision B is interrupted by revision C. Both A and B are old while C rolls out. B was created to replace A, so their `initial-replicas` values describe the same role capacity. For each role, `initialOld` is the larger value from A and B, rather than their sum.

`oldSpec` is the total number of old replicas that are currently requested. It adds the current Specs across A and B because all of those replicas use cluster capacity until they are drained.

If an old LWS does not have a valid `initial-replicas` annotation, the controller stores its current Spec as the best available fallback before draining it.

Within each side, the planner uses discrete linear interpolation. Every role uses the same progress fraction. The resulting replica counts are rounded up to whole numbers.

Each side measures progress on its own fractional scale. For one side, `roleSizes` is the list of replica counts for its roles. `positiveRoleSizes` is the same list without roles whose replica count is zero.

```
fractionalStepCount      = max(roleSizes)
smallestReplicaFraction  = 1 / max(roleSizes)
largestReplicaFraction   = 1 / min(positiveRoleSizes)
```

`fractionalStepCount` is the number of equal intervals between the start and end of one side. A checkpoint is a reference position at an interval boundary. Checkpoint `0` is the start and checkpoint `fractionalStepCount` is the end, so there are `fractionalStepCount + 1` checkpoint positions. A checkpoint is not a reconcile iteration. The controller may wait at one checkpoint or advance across more than one interval in a reconcile.

`smallestReplicaFraction` is the distance between adjacent checkpoints. It comes from the largest role because one replica is the smallest fraction of that role. In an `8P/4D` side, Prefill is the largest role, so one Prefill replica represents `1/8` of the rollout and creates eight equal intervals. `largestReplicaFraction` is the width of the coordination window. It comes from the smallest non-zero role because one replica is the largest fraction of that role. Decode is the smallest role in this example, so one Decode replica represents `1/4` of the rollout. The controller therefore allows at most `1/4` difference between role progress.

`newStepCount` is `fractionalStepCount` calculated from the new target counts. `oldStepCount` is `fractionalStepCount` calculated from the `initialOld` counts. At checkpoint `k`, the replica count for one role is calculated with ceiling division:

```
newAtStep(k) = ceil(target * k / newStepCount)
oldAtStep(k) = ceil(initialOld * (oldStepCount - k) / oldStepCount)
```

The planner uses `leastAdvancedStep` to select the shared checkpoint from the current replica counts. It calculates each role's growth or drain progress and returns the smallest step reached by any non-empty role. The planner then calculates the replica count for every role at that checkpoint. Ceiling division keeps each old role above zero until the final checkpoint. It also prevents a smaller role from getting more than one replica's worth of progress ahead. When multiple checkpoints produce the same replica count, the controller uses the latest one.

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

This moving window is the fractional-lockstep guarantee. Roles can move by different replica counts, and their API updates are not atomic. Readiness, surge, and availability limits may make the executable part of the window smaller. To keep a zero-surge rollout moving, the controller may sometimes drain one old role beyond the normal old-side window, but that drain must still stay above the role's availability floor. `MaxSurge` and `MaxUnavailable` are enforced independently for each role. They do not provide an atomic availability guarantee across roles.

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

`MaxSurge` and `MaxUnavailable` remain hard, absolute per-role limits. For each role the executor enforces:

```
roleSize          = max(initialOld, target)
surgeCeiling      = roleSize + MaxSurge
availabilityFloor = max(0, min(initialOld, target) - MaxUnavailable)

oldSpec + newSpec                    <= surgeCeiling
oldCommittedReady + newCommittedReady >= availabilityFloor
```

The proportional planner can intentionally use less than those raw limits to keep differently sized roles moving together. Let `budgetSteps` be the larger of the old and new side step counts. A raw per-role budget is projected onto the shared fraction scale as:

```
projected(role, budget) = ceil(roleSize * budget / budgetSteps)
```

The same projection defines the maximum new work allowed to be issued but not yet Ready:

```
pendingAllowance = projected(role, MaxSurge + MaxUnavailable)
newSpec - newCommittedReady <= pendingAllowance
```

This bounded window is what permits pipelining across slow pod starts. It does not grant every role the unscaled `MaxSurge + MaxUnavailable` sum. If independent pending bounds would separate role progress by more than `largestReplicaFraction`, faster roles wait at that coordination boundary.

#### Reconcile ordering and completion

One plan may contain both an old-side drain and new-side growth. The executor applies the floor-safe old drain first and then grows the new revision, so the two API updates cannot create a transient surge violation. If readiness or capacity prevents either mutation, the controller requeues rather than mistaking the no-op for completion.

Interrupted rollouts drain old revisions newest first. A revision is retired as soon as all of its role Specs are zero; stale status from its terminating pods neither blocks the next older revision nor contributes availability.

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

The observations are planner checkpoints, not a promise that every cluster will expose exactly this sequence. Readiness, API observations, and interrupted updates may introduce additional reconciles.

The pending window changes the slow-start case materially. After observation 1, suppose the new `2P/1D` is still unready. The next reconcile may still issue up to `4P/2D` because that is the proportional pending allowance. It may drain only availability that is actually committed; it cannot count those unready replicas, or stale Ready status above an already-reduced Spec, toward the floor. Once Ready advances, pending capacity opens and the pipeline continues.

### Service Orchestration

Headless Services are automatically created for each role per revision. This allows load balancers (e.g., llm-d) to count pods per revision across all roles and route traffic proportionally during rolling updates.

- **Naming**: `{disaggregatedset-name}-{revision}-{role}-prv` (e.g., `my-llm-abc12345-prefill-prv`)
- **Selector**: Selects pods from the specific revision's LeaderWorkerSet for that role
- **Cleanup**: Owned by the corresponding LeaderWorkerSet and garbage collected with it

### Controller Architecture

The controller is stateless: all state is derived from observed resources. The `initial-replicas` annotation preserves each revision's replica target across rolling updates. Owner references on managed LeaderWorkerSets and Services ensure proper garbage collection.

### Test Plan

[X] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

#### Unit tests

- Rolling update planner: step computation, edge cases, constraint violations
- Executor: Spec/Ready separation, pending bounds, availability-safe drains, slow-role readiness, coordinated retirement, and newest-first retirement
- Service manager: creation conditions, cleanup logic
- API validation: role count, unique names, replica constraints

#### Integration tests

- DisaggregatedSet creation creates LeaderWorkerSets for all roles
- Template update triggers coordinated rolling update
- Headless Services created for each role
- Interrupted rollout resumes correctly after controller restart
- Deletion cascades to owned resources

### Graduation Criteria

**Alpha (v0.1)**:
- DisaggregatedSet CRD with validation
- LeaderWorkerSet creation and ownership
- N-dimensional rolling update algorithm
- Automatic headless Service creation
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
- 2026-09-14: Updated the rolling-update contract to cover fractional lockstep, readiness and availability bounds, how old revisions are removed, and how an interrupted rollout remembers the replica counts it was meant to reach.

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
