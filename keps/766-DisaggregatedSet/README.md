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
    - [Bootstrap surge](#bootstrap-surge)
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

**Mitigation**: The planner receives one active old revision, every other old revision, and the target revision. It finds the furthest replica targets that satisfy the coordination window, rollout budgets, available capacity, and revision completeness together. The executor does not repair those targets. If one old revision cannot move, it tries the next candidate. If no ordinary move exists, the planner may create one bootstrap replica for a missing required role. That replica may temporarily exceed the role's surge ceiling by one. The controller reports a temporary block only when neither ordinary nor bootstrap progress is possible. After a restart, it reconstructs rollout state from the `disaggregatedset.x-k8s.io/revision` label on existing LeaderWorkerSets.

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

Each managed LWS stores an `initial-replicas` annotation. While a revision is current, replica-only changes and external-scaler changes keep this value aligned with the revision's target replica count. When a newer revision makes it old, the value freezes and becomes that revision's `initialOld` baseline while the controller drains it. If another revision interrupts its rollout, the annotation preserves the replica count it was intended to reach rather than its partially created Spec.

If an old LWS does not have a valid `initial-replicas` annotation, the controller stores its current Spec as the best available fallback before draining it. An explicit annotation value of `0` is valid and is not treated as missing.

Suppose a rollout from revision A to revision B is interrupted by revision C. Both A and B are now old. The controller processes only one of them at a time. Revisions with no Ready replicas are preferred. The remaining candidates are ordered newest first. The controller asks the planner about candidates in that order and selects the first candidate with a safe executable action. A blocked B therefore does not prevent a movable A from making progress.

While B is active, A is parked and remains unchanged. Fractional planning uses B's own `initial-replicas` value as `initialOld` and B's current Spec as `activeOldSpec`. Ready replicas in A reduce how much of C is needed during this phase. After B reaches zero, A becomes active and the controller plans the A to C phase. The controller never combines the `initial-replicas` values from A and B.

Parking does not remove A from safety accounting. The planner includes the Spec of every old revision when enforcing surge. It includes a revision's Ready replicas in available capacity only when every role required by that revision has at least one Ready replica.

Each side measures every role's progress as a fraction. On the new side, progress is the number of replicas created divided by the role's target. On the old side, progress is the number of replicas removed divided by the role's `initialOld` value. A zero-sized role does not define progress on that side.

For one side, `roleReplicaCounts` contains every role size. A count can be zero when the whole DisaggregatedSet is scaled to zero or when a role exists on only one side of the rollout. `positiveRoleReplicaCounts` excludes those zeros because they do not define a fractional window. These two fractions describe the possible integer positions and the window width:

```
smallestReplicaFraction  = 1 / max(roleReplicaCounts)
largestReplicaFraction   = 1 / min(positiveRoleReplicaCounts)
```

`smallestReplicaFraction` comes from the largest role because one replica is the smallest possible change for that role. In an `8P/4D` side, one Prefill replica represents `1/8` of the rollout. `largestReplicaFraction` comes from the smallest role. One Decode replica represents `1/4`, so the controller permits at most `1/4` difference between the progress of the fastest and slowest roles.

The planner does not store or advance through numbered steps. It directly calculates integer replica targets inside the window. The columns below only visualize every possible position of an `8P/4D` old side. The window is frozen over positions 5 through 7 for this one observation. Roles do not need to occupy the same position; they only need to remain within the window.

```
Frozen window: steps 5 through 7

fraction removed   0 --- 1/8 --- 2/8 --- 3/8 --- 4/8 --- [5/8 --- 6/8 --- 7/8] --- 1
illustrative step  0 ---  1  ---  2  ---  3  ---  4  --- [ 5  ---  6  ---  7 ] --- 8
Prefill remaining  8 ---  7  ---  6  ---  5  ---  4  --- [ 3  --- 2*  ---  1 ] --- 0
Decode remaining   4 ---  4  ---  3  ---  3  ---  2  --- [2*  ---  1  ---  1 ] --- 0

* = current role position inside the frozen window
```

The distance between adjacent columns is `1/8`, the `smallestReplicaFraction`. The window is two columns wide, or `2/8 = 1/4`, the `largestReplicaFraction`. Decode=2 is at position 5 and defines the lower edge of this window. Prefill is at position 6, so it may advance to position 7 but no further while Decode remains at position 5. The controller recalculates the window from every new observation.

This moving window is the fractional-lockstep guarantee. Roles can move by different replica counts and can occupy different positions inside the window. If observed state is already outside the window, the planner does not reverse work. It holds the leading role and advances only lagging roles until they return to the window. For example, on a new `8P/4D` side, `6P/1D` represents 75% Prefill progress and 25% Decode progress. The gap exceeds `1/4`. The planner keeps Prefill at 6 and may grow Decode to 2, reducing the gap to `1/4`.

The fractional window does not replace rollout budgets. For each candidate revision, the planner intersects the window with monotonic growth and drain, surge, pending readiness, availability, and revision-completeness constraints. It returns the lowest feasible old Specs and highest feasible target Specs for the observed state. If no mutation is feasible, it returns no step. The per-role budgets do not provide an atomic availability guarantee across roles.

#### Issued work and available capacity

The controller distinguishes LWS `Spec` replicas (work already issued, including pods still starting) from `Ready` replicas (capacity available to serve).

Spec drives the planner's progress calculation. Re-planning from Ready would request the same work again on every reconcile while a pod is starting. Ready instead controls how much additional work may be in flight, whether an old replica can be removed safely, and whether the rollout is complete.

Status can temporarily remain higher than Spec after a scale-down. The controller does not know which replicas the LWS controller will delete. It reserves every replica above Spec before counting committed availability:

```
pendingDrain   = max(0, status.replicas - spec.replicas)
committedReady = min(spec.replicas,
                     max(0, status.readyReplicas - pendingDrain))
```

This prevents a replica already committed to deletion from authorizing another drain. The controller guarantees that a drain is safe for the snapshot it observed. It cannot prevent an unrelated pod from losing readiness after that observation.

Readiness is also revision-aware. A revision contributes its committed Ready counts only when every required role has at least one; otherwise it contributes zero for every role.

For example, a target revision with `0P/2D` Ready contributes `0P/0D` usable capacity. Its Decode replicas cannot authorize retirement of an old Prefill/Decode revision. Once the target reaches `1P/2D` Ready, both role counts become usable together.

#### Capacity and pending-work bounds

`MaxSurge` and `MaxUnavailable` remain hard, absolute per-role limits. For each role the planner enforces:

```
roleReplicaCount  = max(initialOld, target)
surgeCeiling      = roleReplicaCount + MaxSurge
availabilityFloor = max(0, min(initialOld, target) - MaxUnavailable)

oldSpec + newSpec <= surgeCeiling
```

`oldSpec` includes active and parked old revisions. Existing out-of-bound Spec is never increased.

For target growth, complete parked revisions reduce the capacity needed during the current active-revision phase: `phaseTarget = max(currentNewSpec, target - parkedUsableReady)`.

The planner also limits issued-but-unready target work while old Spec remains. Let `budgetScale` be the largest `initialOld` or target count across the roles. A raw per-role budget is projected onto that scale as:

```
projected(role, budget) = ceil(roleReplicaCount * budget / budgetScale)
pendingAllowance        = projected(role, MaxSurge + MaxUnavailable)
newSpec - newCommittedReady <= pendingAllowance
```

This bounded window is what permits pipelining across slow pod starts. It does not grant every role the unscaled `MaxSurge + MaxUnavailable` sum. If independent pending bounds would separate role progress by more than `largestReplicaFraction`, faster roles wait at that coordination boundary.

The target revision does not need to be complete for its committed Ready count to limit pending work. However, it must be complete before that Ready count can authorize an old drain. The pending-readiness bound applies while any old Spec for that role overlaps the target. Once all old Spec for the role is zero, withholding target replicas cannot protect old availability. The controller may issue the rest of that role's target Spec and then waits for it to become Ready.

For an old drain, the planner assumes every removed Spec replica could have been Ready. If any surviving required role could lose its last Ready replica, the entire active revision becomes unusable for every role. The resulting usable capacity must remain above the availability floor. If the observed state is already below its floor, the planner must not reduce usable capacity further.

Revision completeness is a separate hard constraint. For required roles that are still present in the active old revision, either every role remains at one or more Spec replicas, or every role reaches zero in the same plan. This allows ordinary partial drains and coordinated retirement without a fallback that leaves only part of a revision running.

#### Bootstrap surge

A zero-surge rollout can otherwise reach a state in which no ordinary move is possible. For example, an old `1P/5D` revision may have drained to `1P/4D` while the target revision is `0P/1D`. The old Prefill cannot retire by itself because that would leave its revision incomplete. The target Prefill cannot start without exceeding its surge ceiling. The target Decode cannot authorize another old Decode drain because a target revision with no Prefill is not usable.

When the ordinary constraint intersection is empty, the planner may treat `maxSurge: 0` as `maxSurge: 1` to create the first Spec replica of each missing required target role. This exception applies only when that role has a positive `maxUnavailable`; a zero value for both budgets is not a valid rollout configuration. Physical occupancy may exceed the configured surge ceiling by at most one replica for each such role. A role is eligible only while its target Spec and ordinary replica limit are both zero. Once that first replica has been issued, the exception cannot create another replica for the role. The controller waits for the bootstrap replica to become Ready.

The executor prefers an ordinary step from any old-revision candidate over a bootstrap step. It uses bootstrap surge only when no candidate can make ordinary progress. Once every required target role has Ready capacity, the normal planner can drain old capacity, reuse the released slots, and return within the configured surge ceiling. If a bootstrap replica cannot be scheduled or does not become Ready, the rollout remains blocked; creating additional emergency replicas cannot resolve that operational failure.

#### Reconcile ordering and completion

The executor considers old revisions with no observed Ready replicas first, then the others from newest to oldest. For each candidate, the planner computes the furthest old drain and target growth allowed by all constraints. These are constraints on one result, not a sequence of recovery actions. The executor validates and applies that result unchanged; if it changes no API target, the executor tries the next candidate.

One plan may contain both an old-side drain and new-side growth. The executor applies the old drain first. It then grows the target revision. For ordinary plans, this ordering avoids a transient surge violation between API updates. A marked bootstrap plan is the documented one-replica exception. The executor does not repair or reinterpret the planner's targets.

If no candidate has an ordinary step, the executor uses the first candidate's bootstrap step, when available. Otherwise, the rollout is temporarily blocked, not complete. The controller emits one event, requeues, and waits for readiness, deletion, capacity, or a configuration change. Completion is checked separately from the absence of a feasible step.

Interrupted rollouts mutate at most one old revision per reconcile and leave the others parked. Roles with an intended size of zero are not required. A terminating LWS contributes neither Spec nor Ready capacity.

Fully drained old revisions are cleaned continuously. If a non-zero old revision remains, all zero-Spec revisions are deleted. If every old revision is at zero while the target is not Ready, only the newest zero-Spec revision is retained as a temporary rollout marker. The marker is deleted after the target becomes Ready.

A rollout is complete only when every old role Spec is zero, every new role Spec has reached its target, and every new role has at least its target number of committed Ready replicas.

### Example: Pipelining an 8P/4D Rollout

Consider a template-only update with `MaxSurge=2` and `MaxUnavailable=2`. The largest role creates a `1/8` progress grid:

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
| 1           | 6 | 2 | 2 | 2 |
| 2           | 4 | 1 | 4 | 3 |
| 3           | 2 | 1 | 6 | 4 |
| 4           | 0 | 0 | 8 | 4 |

This is one possible sequence when readiness catches up between observations. It is not a promise that every cluster exposes exactly these states. Readiness, API observations, and interrupted updates may introduce additional reconciles.

The pending window changes the slow-start case materially. After observation 1, suppose the new `2P/2D` has `Ready=0P/0D`. The pending allowance is `4P/2D`, derived from `MaxSurge + MaxUnavailable`. Decode has reached its allowance, but the controller may issue two more Prefill replicas. The target reaches a Spec of `4P/2D` without waiting for the first batch to become Ready.

`MaxUnavailable` independently sets availability floors of `6P/2D`. If the remaining old revision has `Ready=6P/2D` while the new revision is still unusable, no old replica may drain because both roles are at their floors. As the new revision becomes complete and Ready, its capacity permits further old replicas to drain.

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

- Rolling update planner: constraint intersection, fractional windows, same-revision usable readiness, worst-case Ready loss, revision completeness, bootstrap surge, and blocked-state feasibility
- Executor: first-executable candidate selection, pending drains, slow-role readiness, terminating targets, External shrink, and staged interrupted rollouts
- Cleanup: repeated interrupted revisions retain at most one drained rollout marker
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
- 2026-09-28: Made rollout planning revision-aware, replaced executor recovery actions with one constraint-based calculation, and documented committed readiness and bounded drained-revision retention.

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
