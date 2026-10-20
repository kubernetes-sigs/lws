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
    - [Automatic unschedulable target recovery](#automatic-unschedulable-target-recovery)
    - [Reconcile ordering and completion](#reconcile-ordering-and-completion)
    - [Reporting availability](#reporting-availability)
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

**Mitigation**: The planner receives one active old revision, every other old revision, and the target revision. It finds the furthest replica targets that satisfy the coordination window, rollout budgets, available capacity, and revision completeness together. The executor does not repair those targets. If one old revision cannot move, it tries the next candidate. If no ordinary move exists, the planner may create one bootstrap replica for a missing required role. If the scheduler then cannot place that replica, the planner may drain old capacity without leaving a partial revision through the bounded fallback described below. After a restart, the controller reconstructs rollout state from the `disaggregatedset.x-k8s.io/revision` label on existing LeaderWorkerSets.

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

When no old revision has non-zero Spec replicas, the controller reconciles the current revision directly. This does not imply that the workloads are already stable or Ready: the controller may still need to create or scale their LWS objects.

Each managed LWS stores an `initial-replicas` annotation. While a revision is current, replica-only changes and external-scaler changes keep this value aligned with the revision's target replica count. When a newer revision makes it old, the value freezes and becomes that revision's `initialOld` baseline while the controller drains it. If another revision interrupts its rollout, the annotation preserves the replica count it was intended to reach rather than its partially created Spec.

If an old LWS does not have a valid `initial-replicas` annotation, the controller stores its current Spec as the best available fallback before draining it. An explicit annotation value of `0` is valid and is not treated as missing.

Suppose a rollout from revision A to revision B is interrupted by revision C. Both A and B are now old. The controller processes only one of them at a time. Revisions with no Ready replicas are preferred. The remaining candidates are ordered newest first. The controller asks the planner about candidates in that order and selects the first candidate with a safe executable action. A blocked B therefore does not prevent a movable A from making progress.

While B is active, A is parked and remains unchanged. Fractional planning uses B's own `initial-replicas` value as `initialOld` and B's current Spec as `activeOldSpec`. For overlapping-role replacement, usable Ready replicas in A reduce how much of C is needed during this phase. After B reaches zero, A becomes active and the controller plans the A to C phase. The controller never sums the `initial-replicas` values from A and B. Roles still required by the target use the desired replica count for availability; removed roles retain the shared historical protection described below. Selecting a smaller drain candidate cannot lower either requirement.

Parking does not remove A from safety accounting. The planner includes the Spec of every old revision when enforcing surge. It includes a revision's Ready replicas in usable serving capacity only when every role required by that revision has at least one Ready replica. The separate per-role readiness check described below also accounts for healthy roles whose counterparts are unready.

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

The controller distinguishes LWS `Spec` replicas (work already issued, including pods still starting) from `Ready` replicas (healthy whole LWS replica groups). A Ready role does not necessarily belong to a revision that can serve requests.

Spec drives the planner's progress calculation. Re-planning from Ready would request the same work again on every reconcile while a pod is starting. Ready instead controls how much additional work may be in flight, whether an old replica can be removed safely, and whether the rollout is complete.

Status counters can lag a scale-down or report terminating groups as Ready. The controller uses `replicagroups.Observe` with an uncached API reader and `Snapshot.Availability()` to obtain raw whole-group Ready capacity and retained Ready credit. Only retained credit can authorize another drain; raw readiness remains visible to protect the no-worsening floor.

The shared helper checks ownership and native acknowledgements, excludes terminating groups from retained credit, and accounts for outstanding removals for both Ordinal and Hash identity. Increasing Spec on rollback does not restore credit to terminating Pods. Safe growth need not wait for every deletion to finish.

If the LWS disappears, is replaced, starts deleting, or changes generation between discovery and observation, the executor retries without planning: unknown is not zero readiness. No downscale marker or availability summary is persisted. These ordered reads are not an atomic snapshot or a reservation against later failures.

Availability uses two separate checks. **Usable serving capacity** counts a revision's Ready replicas only when every required role has at least one Ready replica in that same revision; otherwise it contributes zero usable serving capacity for every role. **Per-role readiness** protects healthy replicas independently, including those in structurally incomplete revisions. Neither check treats roles from different revisions as a serving pair.

When a required role still present in the target has `maxUnavailable >= desired`, its policy explicitly permits that role to become entirely unavailable. An old revision containing that role may then lose serving capacity and become structurally incomplete, while every role's independent Ready-group floor remains enforced. Readiness loss alone does not grant this exception, and a removed role does not grant it on behalf of current roles. If all required current roles have positive floors, old drains continue to protect serving capacity and whole-revision completeness. Target reductions retain their existing serving and startup safeguards.

For example, a target revision with `Spec=1P/2D` and `Ready=0P/2D` contributes no usable serving capacity. Its Ready Decode replicas can satisfy the independent Decode check, but are not replacement credit for the serving-capacity check. Once the target reaches `Ready=1P/2D`, both role counts also become usable serving capacity.

#### Capacity and pending-work bounds

`MaxSurge` and `MaxUnavailable` are the per-role limits for ordinary rollout steps. A role without a `rollingUpdateConfiguration` uses `maxSurge: 0, maxUnavailable: 1`, the LeaderWorkerSet default, so a rollout never needs capacity beyond the role's replicas. A configuration whose values both resolve to 0, such as a percentage that rounds down on a small role, uses the same default. The two bounded deadlock fallbacks are described below.

For a template-only `1P/2D` update with explicit `maxSurge=0` and `maxUnavailable=1` for both roles, Prefill's floor is zero and Decode's is one. The planner may drain old `1P/2D` to `0P/1D`, then start new `1P/1D`. The old Decode remains protected until a new Decode is Ready, after which the last old group can drain and the second new Decode can start. Three equal-size group slots suffice. The permitted Prefill outage also interrupts serving capacity; it does not authorize deleting Decode's last retained Ready group. Per-role readiness continues to count incomplete revisions even if policy changes later remove the zero-floor allowance.

Roles still present in the DisaggregatedSet use its live rollout settings, including policy-only changes during a rollout. The controller records each role's resolved `maxUnavailable` in the controller-owned `disaggregatedset.x-k8s.io/max-unavailable` annotation on the DisaggregatedSet. When a role is removed or renamed, all old revisions of that role use `max(1, lastRecordedMaxUnavailable)`: larger saved allowances survive, and a zero or missing allowance becomes one so replacement can start without waiting for a scheduling failure. This is an explicit policy change: an old singleton can retire before any replacement is Ready. Percentages are resolved against the last observed desired replica count before removal; the resulting integer does not shrink as old replicas drain. Entries remain until the role's owned LWS objects are deleted. Invalid history uses the same minimum of one; the old LWS's creation-time strategy is not a reliable fallback because later policy-only changes may have reduced it. Removed names use the default `maxSurge=0`; their budgets are not transferred to differently named target roles.

For each role the ordinary planner enforces the following bounds. `target` is the effective desired count supplied to the planner. For an External role under `Deferred`, overlapping old replicas keep it at `max(latest scaler target, target Spec)`; otherwise it follows the latest requested count. Percentage `maxUnavailable` values for current roles are still resolved against the latest requested count, not this higher clamped count.

```
roleReplicaCount     = max(initialOld, target)
availabilityBaseline = max(initial-replicas across relevant non-drained old revisions)
surgeBaseline        = target (Immediate), roleReplicaCount (Deferred)
surgeCeiling         = surgeBaseline + MaxSurge
availabilityFloor    = max(0, max(target, unreplaced) - MaxUnavailable)

oldSpec + newSpec <= surgeCeiling
```

`oldSpec` includes active and parked old revisions. Existing out-of-bound Spec is never increased.

For a role still required by the target, `unreplaced` is zero and the floor is `max(0, target - MaxUnavailable)`, whether reducing old or target replicas. It is not capped by an old revision's size. Raising the target raises this requirement, but does not manufacture Ready capacity: if fewer replicas are currently Ready, a reduction must preserve that observed capacity instead of worsening the shortfall. Safe growth can continue within the surge and pending-work bounds.

`unreplaced` is normally zero. When a non-drained old revision and the positive target have **no role names in common**, per-role floors alone would permit every old role to drain before any replacement is Ready. In this case, the planner bridges the two sides with a fractional Ready floor for that revision's old roles:

```
replacementFraction   = min(1, min(targetCommittedReady[role] / target[role]))  // positive target roles
unreplaced[oldRole]    = availabilityBaseline[oldRole] - floor(availabilityBaseline[oldRole] * replacementFraction)
```

Credit uses retained Ready from the target revision, never issued Spec. If any required target role is unready, the fraction is zero. Parked revisions still contribute ordinary same-role readiness, but not cross-role replacement credit: another planner call may retire them. Spending that credit here would require protecting it across candidate changes. Instead, an interrupted rollout may wait longer or need more free capacity until the target revision itself becomes Ready.

The protected removed roles and their baselines are shared by every candidate. Removed-role baselines include only non-drained disjoint revisions; a larger overlapping revision must not inflate their replacement requirement. Switching the active candidate changes neither these shared baselines nor a current role's desired-based floor. After the last disjoint revision drains, any remaining mixed-role revision returns to ordinary overlapping-role rules, even if the target is not fully Ready.

For healthy old `A=4` replaced by `B=2,C=2`, with effective `maxUnavailable=1`, Ready `B=1,C=0` permits `A=3`; `B=1,C=1` permits `A=1`; `B=2,C=2` permits retirement. Credit rounds down and waits for the slowest required replacement role. The floor still uses the existing no-worsening and worst-case-deletion checks; it is not a promise that different role types have equal physical resource cost. Cold starts, scale-to-zero, and ordinary same-role rollouts are unchanged.

A removed role's availability baseline follows the **current set of non-drained disjoint old revisions**; it is not a fixed promise captured at the start of an interrupted rollout. A revision participates while at least one of its role Specs is non-zero. Every candidate considered from the same observation uses the same per-role maximum. Once all of an old revision's role Specs reach zero, it stops contributing to that maximum, whether or not its LWS objects have been deleted. If it carried the largest baseline, the removed role's replacement requirement may decrease. This does not lower the floor of a role still required by the target: that floor follows the desired replica count. No separate rollout-wide baseline is persisted; `initial-replicas` remains the per-revision baseline used for fractional coordination.

For example, consider A → B → C with `initial-replicas=1` for A, `initial-replicas=2` for B, a target of 2 for C, `maxSurge=1`, and `maxUnavailable=0`, identically for each role. If A, B, and C each have one Spec and one Ready replica per role, B can retire while A+C retain two Ready replicas. The next A → C phase still protects two Ready replicas, not A's smaller baseline of one. C can grow from one to two Spec replicas, but A must stay until C's second replica is Ready. An ordinary drain cannot turn the existing two Ready replicas into one merely because the active old revision changed. Completion still requires C to reach its full target in both Spec and Ready replicas.

This stronger availability requirement can delay old-revision retirement when demand increases or a rollout is interrupted. If the replacement groups cannot fit or become Ready, the rollout may need more capacity or an operator-adjusted budget; the planner does not automatically lower the floor to the size of a smaller old revision.

For target growth, parked revisions with usable serving capacity reduce the capacity needed during the current active-revision phase. However, each role required by the final target keeps a phase target of at least one replica. This lets the target revision form a complete same-revision unit instead of depending on a counterpart from a parked revision: `phaseTarget = max(currentNewSpec, target - parkedUsableReady, 1)` for required roles.

The planner also limits issued-but-unready target work while old Spec remains. Let `budgetScale` be the largest `initialOld` or target count across the roles. A raw per-role budget is projected onto that scale as:

```
projected(role, budget) = ceil(roleReplicaCount * budget / budgetScale)
pendingAllowance        = projected(role, MaxSurge + MaxUnavailable)
readinessCredit         = disjointReplacement ? floor(target * replacementFraction) : newCommittedReady
newSpec <= readinessCredit + pendingAllowance
```

This bounded window is what permits pipelining across slow pod starts. It does not grant every role the unscaled `MaxSurge + MaxUnavailable` sum. If independent pending bounds would separate role progress by more than `largestReplicaFraction`, faster roles wait at that coordination boundary.

For ordinary same-role replacement, the target revision does not need to be Ready in every role for its own committed Ready counts to limit pending work. While disjoint old capacity remains unreplaced, growth instead uses the same slowest-target Ready fraction as old removal. Otherwise, a faster role could keep requesting replicas that occupy the space needed by a slower role, while that slower role prevents further old removal. The shared fraction applies to every batch, not just the first replica of each target role. This readiness bound alone may reduce parallel startup for faster roles; it does not retract already-issued Spec, increase availability budgets, or guarantee enough physical capacity to finish the rollout. The separate surplus correction below uses observed old drain, not readiness dips, to identify excess Spec.

Disjoint target growth also waits for observed old scale-downs:

```
releasedFraction = min(max(0, oldBaseline - oldSpec) / oldBaseline)  // protected removed roles
newLimit[role]   = ceil(target[role] * releasedFraction) + MaxSurge[role]
```

The planner intersects this limit with the other growth bounds. At zero surge, it first drains old replicas, then observes the changed Spec before creating replacements. This is deletion intent, not a guarantee that terminating Pods have disappeared; the scheduler may still wait for their resources. Until every required target role has a Ready group, growth starts at most one group of each role. A larger Prefill budget therefore cannot create a second Prefill before the first Decode is Ready. The existing bootstrap exception cannot bypass the disjoint limit.

Previously issued target replicas above `newLimit` can be trimmed only while the target is incomplete and no non-drained old revision shares its roles. The trim never changes old replicas and uses the same availability and startup protection as other target reductions. Ready capacity still needed by the desired-based floor, including replacement credit for removed roles, cannot be spent again. A role with any scheduled group, Ready group, or unknown scheduling state keeps at least `min(current Spec, desired)` replicas. Only a role whose groups are all confirmed unscheduled can be reduced below that count; every required role and its last committed Ready group remain protected. Genuine excess above desired may still be removed safely. Thus another role losing readiness cannot delete needed Ready capacity or restart needed loading groups.

The correction uses observed old progress, not the first-group startup limit, and does not require an annotation, timer, or scheduling-failure trigger. Under both scaling policies, the executor first checks other old revisions for safe ordinary progress before selecting a target correction. Protecting scheduled work may leave surplus occupying capacity needed by another target role. Such a rollout waits for readiness, more cluster capacity, or an operator-adjusted budget; it does not compensate by deleting more old capacity.

These are rollout-progress bounds, not resource matching. For example, releasing one old Prefill using two GPUs and one old Decode using four GPUs can accommodate a new Prefill using four GPUs and a new Decode using two GPUs, without inferring a mapping between renamed roles. Splitting old `W=4` into `P=2,D=2` may instead need two freed groups before either complete replacement unit can serve: an old budget of one can remain blocked. Changed role ratios, larger requests, placement constraints, and small integer budgets can likewise require more capacity or a larger configured old budget. For disjoint replacements, the controller does not spend additional old availability because a new Pod is Unschedulable; the existing same-role fallback is unchanged.

The pending-readiness bound applies while any old Spec for that role overlaps the target, or while disjoint old capacity remains unreplaced as described above. Otherwise withholding target replicas cannot protect old availability: the controller may issue the rest of that role's target Spec and then waits for it to become Ready.

For an old drain, the planner assumes every removed Spec replica could have been Ready. If any surviving required role is absent or could lose its last Ready replica, the entire active revision becomes unusable for every role. Raw Ready capacity determines how much currently serving capacity must be preserved, capped at the role's availability floor. The proposed post-drain state is checked with committed Ready capacity, so replicas already pending deletion cannot be counted as survivors. This also prevents an interrupted revision from being retired while its still-running replicas are needed to hold the floor; the controller waits for pending deletions to settle or for another revision to replace that capacity.

The per-role check independently preserves each role's observed Ready count, capped at the same availability floor. Retained Ready replicas in parked and target revisions can satisfy this check even when another role in those revisions is absent or unready. These are replicas that remain after the proposed drain, not replicas being retired. The per-role check always applies. The serving-capacity check also applies unless an old revision's live role budget explicitly permits an outage as described above.

For example, suppose both old revisions have initial baselines of `1P/2D`, and the target is also `1P/2D`. Set `maxUnavailable=0` for both roles, `maxSurge=2` for Prefill and `maxSurge=1` for Decode. Assume no pending removals, so raw and retained Ready counts agree:

| Revision | Spec | Ready | Usable serving capacity |
| --- | --- | --- | --- |
| Old A | 1P/1D | 0P/1D | 0P/0D |
| Old B | 1P/1D | 0P/1D | 0P/0D |
| Target C | 1P/1D | 1P/1D | 1P/1D |

Decode has three requested replicas, exactly its surge ceiling, so C cannot grow its second Decode yet. If the independent per-role check accepted replacement credit only from fully Ready revisions, neither A nor B could retire: it would count the selected revision's Decode toward the two-replica minimum, but ignore the other old revision's Decode as a replacement. Waiting for C would not help because all its requested replicas are already Ready.

The policy instead permits A to retire as a unit while B and C retain two Ready Decode replicas. Usable serving capacity remains C's `1P/1D`, unchanged; B's Decode is not paired with C's Prefill. C can then use the freed slot for its second Decode, and B can retire once that replica becomes Ready. This deliberately allows healthy replicas in non-serving revisions to satisfy the independent per-role minimum. It avoids requiring broken old roles to recover before the replacement can finish, rather than requiring a fully serving replacement for every healthy replica removed.

Unless a live required role's budget permits an outage, revision completeness is a separate hard constraint. For required roles that are still present in the active old revision, either every role remains at one or more Spec replicas, or every role reaches zero in the same plan. When the policy permits an outage, roles can instead retire independently within their own Ready-group budgets.

#### Bootstrap surge

A rollout can reach a state in which no ordinary move is possible. For example, a `2P/5D` target with `maxSurge=0` and `maxUnavailable=1` per role can have old Spec `2P/4D`, old Ready `1P/4D`, and target Spec/Ready `0P/1D`. Both roles have positive floors. Deleting an old Prefill could remove its last Ready group, while starting target Prefill would exceed the surge ceiling. The target Decode cannot authorize another old Decode drain because a target revision with no Prefill is not usable. An interrupted rollout can reach the same state when parked old revisions consume an otherwise positive `maxSurge`.

When the ordinary constraint intersection is empty, the planner may create the first Spec replica of each missing required target role without a free configured surge slot. The role must have a positive `maxSurge` or `maxUnavailable`; a zero value for both budgets is not a valid rollout configuration. This first-group action is eligible only while the role's target Spec and ordinary replica limit are both zero.

Already-started target roles can also need one extra group per eligible role to unblock coordinated growth. This is allowed with `Immediate`, or under either policy when any role's effective desired count exceeds its historical availability baseline. The eligible role's existing target groups must all be retained Ready, its total old and target Spec must exactly equal its surge ceiling, and ordinary growth must be blocked even though its phase target requires more replicas. A fully renamed target role with no same-role old replicas cannot use this exception while disjoint old replicas remain. The extra group cannot stack while that role is over its ceiling or waiting for readiness; a later eligible step may borrow again after the earlier excess clears. This does not enable demand-driven target reductions under `Deferred`.

The executor prefers an ordinary step from any old-revision candidate over a bootstrap step. It uses bootstrap surge only when no candidate can make ordinary progress. As enough target capacity becomes Ready, the normal planner can drain old capacity, reuse the released slots, and return within the configured surge ceiling. With `Immediate`, or when any role's effective desired count exceeds its historical availability baseline, old-side fractional pacing may be bypassed only to release existing excess above that policy's surge ceiling. This prevents a bootstrap step from leaving the rollout stuck above its ceiling. The desired-based availability floor still applies, as does whole-revision completeness when required by policy; removed roles keep their historical pacing. If the bootstrap Pod is scheduled but does not become Ready and no ordinary step becomes safe, the rollout waits. Slow startup does not permit another exception.

#### Automatic unschedulable target recovery

A target Pod created through bootstrap growth can remain Pending because the old replicas occupy all suitable capacity. Creating more Pods would not help. After the scheduler has continuously reported `PodScheduled=False` with reason `Unschedulable` for one minute, the planner may instead release old capacity for that role. This choice is automatic; it does not add an API policy.

This is the final fallback. It is considered only when no ordinary or bootstrap step is available and the unschedulable role remains above its configured surge ceiling because of bootstrap growth. The selected drain must remove an old replica of that role and preserve revision completeness when required by policy. During that drain, a role may fall at most one replica below its configured availability floor, but a positive floor never falls below one. This relaxation is not cumulative. Once the excess bootstrap replica has been offset, the fallback cannot drain more capacity.

For example, assume the target is `1P/4D`, `maxSurge=1`, and `maxUnavailable=0`. Revisions A and B currently provide `A=1P/3D` and `B=1P/1D`. Revision C has `1P/1D`, but its Prefill Pod is unschedulable. The fallback may retire B. This releases B's Prefill capacity for C while temporarily reducing usable Decode capacity from four to three. A remains complete, and no later fallback may reduce Decode below three.

A merely Pending Pod, an image pull, slow readiness, or a recently reported scheduling failure does not activate the fallback. If releasing one old revision is insufficient, the rollout waits rather than relaxing the bound again.

The fallback still requires the same role on both sides. Disjoint-role replacement does not infer which old workload to evict for an unschedulable new role; it may therefore wait for additional cluster capacity to make its next fraction Ready.

#### Reconcile ordering and completion

The executor considers old revisions with no observed Ready replicas first, then the others from newest to oldest. For each candidate, the planner computes the furthest old drain and target growth allowed by all constraints. These are constraints on one result, not a sequence of recovery actions. The executor validates and applies that result unchanged; if it changes no API target, the executor tries the next candidate.

One plan may contain both an old-side drain and new-side growth. The executor applies the old drain first. It then grows the target revision. For ordinary plans, this ordering avoids a transient surge violation between API updates. A marked bootstrap plan is the documented one-replica exception. The executor does not repair or reinterpret the planner's targets.

If no candidate has an ordinary step, the executor uses the first candidate's bootstrap step, when available. If neither exists, it checks for a persistently scheduler-unschedulable target Pod and asks the planner for the bounded availability fallback. Otherwise, the rollout is temporarily blocked, not complete. The controller emits an event, requeues, and waits for readiness, deletion, capacity, or a configuration change. Completion is checked separately from the absence of a feasible step.

Interrupted rollouts mutate at most one old revision per reconcile and leave the others parked. Roles with an intended size of zero are not required. A terminating LWS contributes neither Spec nor Ready capacity.

Fully drained old revisions are cleaned continuously. If a non-zero old revision remains, all zero-Spec revisions are deleted. If every old revision is at zero while the target is not Ready, only the newest zero-Spec revision is retained as a temporary rollout marker. The marker is deleted after the target becomes Ready.

A rollout is complete only when every old role Spec is zero, every new role Spec has reached its target, and every new role has at least its target number of committed Ready replicas.

#### Reporting availability

The `Available` condition reports completion: every role and sub-role has reached its `desiredReplicas`, with all of them Ready and updated. It is False during every rollout and scale-up, including those that never drop below what the user permitted. Clients that need to know whether enough is serving, such as health checks, alerts, and parent controllers like KServe, would otherwise have to reimplement these availability rules ([#1191](https://github.com/kubernetes-sigs/lws/issues/1191)).

The controller therefore also publishes the planner's view of availability, without changing the meaning of `Available` or the planner's rules:

- `RoleStatus.minAvailableReplicas` and `SubRoleStatus.minAvailableReplicas` publish `availabilityFloor` for each current role: in each slice, `max(0, desired - MaxUnavailable)`, with `MaxUnavailable` resolved against the latest requested count exactly as for rollout steps, totalled across slices. A partitioned parent reports the sum of its children's floors. The field is unset while any contributing target is unknown. For a role still present in the target, `unreplaced` is zero, so this is the planner's floor. Removed roles have no published status, so their historical floors are not shown. Under `Deferred`, an External role's effective desired count can exceed its latest target while old replicas overlap; the planner then protects more Ready replicas than the published floor.
- `RoleStatus.availableReplicas` and `SubRoleStatus.availableReplicas` count Ready groups that can serve. As in the planner's usable capacity, Ready groups count only in a revision of a slice in which every role that revision requires has at least one Ready group. A role is required by a revision while its intended replica count, `max(Spec, initial-replicas)`, is positive; each sub-role is required by its issued or frozen sub-role count. Old revisions of removed roles take part in this check, each of their workloads counting as one unit. Ready capacity is summed across revisions and slices, so old and new groups both count while each can serve on its own. The count uses the same Ready groups as `readyReplicas` and never exceeds it.
- A new `MinimumReplicasAvailable` condition is True while every parent and child has `availableReplicas >= minAvailableReplicas`, False with the roles below their floors listed in its message, and Unknown while any floor is unknown. It is independent of the `Available`/`Progressing` pair.

Per-role floors alone cannot express that a request needs every required role: a Ready Prefill is not serving capacity while its revision's Decode has no Ready group. Counting only usable groups answers that without a separate set-level rule, and a role whose policy permits zero Ready groups still makes the condition False when it leaves its peers without usable capacity.

The floor follows the latest target, matching a Deployment's `MinimumReplicasAvailable` (`available >= desired - maxUnavailable`). A scale-up larger than `maxUnavailable` therefore reports False until enough new groups are Ready; a percentage budget scales with the target. Reporting the scale-up as satisfied would need the pre-scale-up count, which a stateless controller does not keep.

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

- Rolling update planner: constraint intersection, fractional windows, same-revision usable readiness, worst-case Ready loss, revision completeness, bootstrap surge, scheduler-unschedulable recovery, and blocked-state feasibility
- Executor: first-executable candidate selection, pending drains, slow-role readiness, scheduler-unschedulable recovery, terminating targets, External shrink, and staged interrupted rollouts
- Cleanup: repeated interrupted revisions retain at most one drained rollout marker
- Status: per-slice floor resolution, same-revision usable capacity across slices and removed roles, and `MinimumReplicasAvailable` staying True through rollouts and scale-ups within `maxUnavailable`
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
- 2026-09-28: Updated rolling updates with fractional lockstep, readiness and availability bounds, durable intended replica counts, and revision-aware constraint planning for interrupted rollouts, replacing executor recovery actions and documenting committed readiness and bounded drained-revision retention.
- 2026-10-09: Updated the rollout contract for [#1105](https://github.com/kubernetes-sigs/lws/pull/1105): shared phase-local availability baselines, retained group readiness from merged [#1137](https://github.com/kubernetes-sigs/lws/pull/1137), independent per-role replacement credit to avoid deadlocks between unready old revisions, stale scale-plan and write guards, and fractional replacement of disjoint roles. Clarified the distinction from usable same-revision serving capacity and the per-role policy trade-off.
- 2026-10-10: Retained removed roles' last observed `maxUnavailable` budgets, including resolved percentages, without changing the planner's availability rules.
- 2026-10-11: Applied the shared target Ready fraction to growth during disjoint-role replacement, so faster roles cannot keep opening new batches ahead of slower roles while old removal waits for them. Existing budgets, issued Spec, and ordinary same-role growth are unchanged.
- 2026-10-11: Gave removed roles a minimum one-group unavailable allowance, bounded disjoint growth by observed old drain, prioritized the first group of every target role, and added a stateless correction for already-issued surplus. Limited removed-role baselines to disjoint old revisions. Documented singleton availability and insufficient transition-capacity limitations.
- 2026-10-12: Base availability for roles still required by the target on desired replicas minus `maxUnavailable`, without capping it at an old revision's size. Retain no-worsening checks and removed-role historical protection; require replacement readiness before retiring the last needed old capacity across interrupted rollout phases.
- 2026-10-12: Allow the existing fully-Ready coordination bootstrap under either scaling policy when effective demand exceeds an old availability baseline. Keep its one-extra-group, readiness, surge-ceiling, and non-stacking limits without enabling demand-driven target reductions under `Deferred`.
- 2026-10-13: Let `Deferred` scale-up release safe old capacity above its policy's surge ceiling, matching coordination-bootstrap eligibility. Keep availability and revision completeness checks; do not enable demand-driven target reductions.
- 2026-10-13: Apply target availability and scheduled-startup protection to renamed-role surplus recovery under both policies. Retain safe cancellation of wholly unscheduled surplus and genuine excess above desired; document that protected startup work can delay capacity recovery.
- 2026-10-13: Prefer safe old-revision progress over permitted target reductions under both policies, including renamed-role surplus recovery.
- 2026-10-20: Published each role's availability floor (`minAvailableReplicas`) and usable Ready capacity (`availableReplicas`) in status, with a `MinimumReplicasAvailable` condition separate from rollout completion ([#1191](https://github.com/kubernetes-sigs/lws/issues/1191)). The planner's rules are unchanged.

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
