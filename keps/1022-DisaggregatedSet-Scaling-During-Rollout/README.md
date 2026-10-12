# KEP-1022: DisaggregatedSet Scaling During Rolling Updates

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Current behavior](#current-behavior)
  - [Goals](#goals)
  - [Non-goals](#non-goals)
- [Proposal](#proposal)
  - [Opt in](#opt-in)
  - [Scale up during a rollout](#scale-up-during-a-rollout)
  - [Scale down during a rollout](#scale-down-during-a-rollout)
  - [If the target changes again](#if-the-target-changes-again)
- [Design details](#design-details)
  - [Observe requested and retained capacity separately](#observe-requested-and-retained-capacity-separately)
  - [Use the latest target for capacity bounds](#use-the-latest-target-for-capacity-bounds)
  - [Keep roles coordinated](#keep-roles-coordinated)
  - [Prefer old-revision reductions](#prefer-old-revision-reductions)
  - [Preserve availability when reducing the target revision](#preserve-availability-when-reducing-the-target-revision)
  - [Keep bounded recovery](#keep-bounded-recovery)
  - [How one reconcile works](#how-one-reconcile-works)
  - [Completion and status](#completion-and-status)
  - [API and compatibility](#api-and-compatibility)
  - [Test plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [End-to-end tests](#end-to-end-tests)
  - [Graduation criteria](#graduation-criteria)
- [Risks](#risks)
- [Alternatives](#alternatives)
- [Implementation history](#implementation-history)
<!-- /toc -->

## Summary

DisaggregatedSet workloads can take minutes to become Ready. Demand can change while a rolling update is still replacing the old application revision. Today, the rollout can continue using a higher replica count than the operator or autoscaler now wants.

This KEP adds an opt-in policy:

```yaml
spec:
  scalingPolicy:
    duringRollout: AdvanceRollout
```

With this policy, the current rollout follows the latest replica targets. More capacity goes into the target revision. When less capacity is needed, the controller prefers removing old replicas; if that cannot make progress safely, it can also reduce the target revision. Omitting the policy keeps the existing behavior, named `RolloutCoupled`.

This extends the planner/executor design from [PR #907](https://github.com/kubernetes-sigs/lws/pull/907) and retains the availability safeguards in [PR #1105](https://github.com/kubernetes-sigs/lws/pull/1105). It uses the shared replica-group observer from [PR #1137](https://github.com/kubernetes-sigs/lws/pull/1137). It does not replace the planner or introduce a second scaling controller.

## Motivation

An operator or autoscaler should be able to respond to changed demand without waiting for an entire rolling update to finish. A scale-down is especially important for accelerator workloads, where unused replicas can retain expensive capacity.

### Current behavior

The planner introduced by PR #907 already distinguishes requested replicas (`Spec`) from Ready replicas. It can issue bounded growth while earlier groups are still starting; there is no global requirement to wait for every target replica to become Ready.

However, target-revision growth is normally one-way during a rollout. For an External role with overlapping old replicas, [KEP-849](/keps/849-DisaggregatedSet-HPA) also clamps the effective rollout target:

```text
effective target = max(latest scaler target, target revision Spec)
```

A lower scaler target therefore does not remove already-created target replicas until old capacity is gone. The historical rollout bounds are useful for a fixed-size replacement, but can hold onto excess replicas after demand falls.

### Goals

1. Allow operators to opt into applying higher and lower targets during a rollout.
2. Put added capacity directly into the target revision, never grow an old revision.
3. Prefer safe old-revision reductions before target-revision reductions.
4. Preserve availability, bounded pending work, and coordination between roles.
5. Converge to the latest target once targets and readiness stabilize and sufficient capacity exists.
6. Recompute decisions from observed state so controller restarts do not lose progress.

### Non-goals

1. Changing how HPA or KEDA computes or stabilizes a target.
2. Making replica or scaling-policy changes create application revisions.
3. Supporting External scaling with `spec.slices > 1`; KEP-849 excludes that combination.
4. Implementing subroles or changing native LWS group identity.
5. Guaranteeing that a new workload fits on the cluster or overriding scheduling constraints to make it fit.

## Proposal

### Opt in

Add `spec.scalingPolicy.duringRollout` with two values:

| Policy | Behavior |
| --- | --- |
| `RolloutCoupled` (default) | Keep the existing rollout behavior, including deferred target-revision scale-down. |
| `AdvanceRollout` | Use the latest targets for capacity bounds and allow safe target-revision scale-down while the rollout is active. |

The target comes from `roles[].spec.replicas` for Static roles or `DisaggregatedSetRoleScaler.spec.replicas` for External roles. Both use the same planner. The policy is set once on the DisaggregatedSet and applies to its roles and slices.

### Scale up during a rollout

Consider two roles, Prefill and Decode. Both have the following counts and budgets; all numbers below are replica groups, not individual Pods:

```text
                         Before target change
old Spec / Ready:        4 / 4
target Spec / Ready:     2 / 1
desired replicas:        6 -> 8
maxSurge:                2 per role
maxUnavailable:          1 per role
```

The latest desired count makes the ordinary surge ceiling `8 + 2 = 10` and the availability floor `8 - 1 = 7` for each role. Only five groups per role are Ready, so raising the target does not authorize removing those Ready groups.

The controller can nevertheless create more target groups before the pending one becomes Ready. The existing pending-work allowance still applies: in this symmetric example it permits three unready target groups, so target Spec can advance from two to four. Readiness and capacity then determine the next step.

This is not an unlimited scale-up. Existing old Spec consumes surge space, and new groups do not count as replacement capacity merely because their creation was requested.

### Scale down during a rollout

Again, assume both roles have these counts:

```text
old Spec / Ready:        4 / 4
target Spec / Ready:     5 / 5
desired replicas:        8 -> 4
maxSurge:                1 per role
maxUnavailable:          1 per role
```

The latest ceiling is five groups per role, but nine have already been requested. The planner first considers reducing old replicas. Here, the complete Ready target revision can replace the old one, so the old revision can drain safely. Target Spec then converges from five to four.

If old replicas cannot safely be removed, the controller may reduce target replicas instead. This is useful when the target is above its newly requested size but an old revision still provides capacity that must be retained. The same availability checks apply to both choices.

A lower target is permission to use the lower target's availability floor, not permission to ignore availability. The controller may wait when a safe reduction does not exist.

### If the target changes again

For a sequence such as `8 -> 12 -> 5 -> 9`, each reconcile uses the latest target and the state currently observed in the cluster. There is no queue of obsolete targets to finish and no old revision is scaled back up.

The existing initial-replica annotation follows the resolved target while a revision is current and freezes when that revision becomes old. This preserves the old revision's rollout reference without introducing a persisted scaling plan.

## Design details

### Observe requested and retained capacity separately

The controller keeps three different facts:

- `Spec`: groups already requested, including groups still starting.
- Observed Ready: groups currently Ready, including Ready groups already selected for removal.
- Retained Ready: Ready groups that remain after already-issued deletion decisions.

The shared replica-group observer computes the last two counts from Pods and native LWS workload state. `min(status.readyReplicas, spec.replicas)` is not sufficient: after a downscale, the specific group selected for deletion may be Ready while another retained group is not.

Serving capacity requires a complete same-revision set: a Ready Prefill from revision A and a Ready Decode from revision B cannot be combined into a complete revision. The planner also protects each role's own Ready replicas independently, so an unready Decode does not authorize deleting healthy Prefill replicas.

The #1105 read and write guards remain in use. Planning that can change capacity uses fresh, batched observations; unknown or mismatched observations cause a retry. Before a scale write, the executor checks that the LWS identity, generation, and Spec still match the observation. Settled reconciles that need no scale action can continue to use informer data.

### Use the latest target for capacity bounds

For a role still present in the desired topology, `AdvanceRollout` uses:

```text
ordinary surge ceiling = latest desired + maxSurge
availability floor     = max(0, latest desired - maxUnavailable)
```

If a target decrease puts current Spec above the ceiling, ordinary growth for that role stops and the planner looks for safe reductions. An existing excess is not itself an error: a target edit can make an already-issued state exceed the new limit instantly.

If observed Ready capacity is already below the floor, the planner does not require the impossible before every action. It instead prevents its own reductions from worsening that Ready deficit. Retained Ready is used when checking the proposed action so a deletion already in flight cannot pay for a second deletion.

The proportional pending-work budget remains anchored to `max(initial old replicas, latest desired replicas)`, as in the existing planner. Moving the desired target does not remove that bound.

Removed or renamed roles retain #1105's separate protection. Applicable old revisions share their old availability baseline and recorded `maxUnavailable`, with the existing minimum allowance of one for a removed role. A new role's lower target or larger `maxUnavailable` must not silently replace an old role's budget.

### Keep roles coordinated

The planner keeps its existing fractional coordination. For example, Prefill with eight groups and Decode with four can advance along a shared curve such as `(1,1)`, `(2,1)`, `(3,2)`. The smaller role may be one rounded step ahead, but one role cannot run arbitrarily ahead of another.

The target side uses the current desired sizes. Old-side progress remains measured from the old revision's initial sizes. A reduction in desired size must not turn that historical fraction window into a permanent barrier to releasing excess capacity.

Therefore, with `AdvanceRollout` only, an old-side reduction for a role with a positive desired count may go beyond the historical fraction window by at most the existing excess:

```text
excess = max(0, total old Spec + target Spec - desired - maxSurge)
```

This exception only corrects physical excess. It cannot authorize extra growth, reduce below the latest ceiling by bypassing coordination, bypass availability, or leave only some required roles in an old revision. Once excess is gone, ordinary fractional coordination applies again. Removed roles have no new replica target to correct: they keep their old fraction window, even when their saved unavailable budgets differ.

### Prefer old-revision reductions

For an interrupted A-to-B-to-C rollout, the executor considers whole old revisions, not unrelated per-role fragments. Fully unready revisions are considered first; within the existing preference order, newer revisions come first. If a preferred candidate is blocked, another candidate may still have a safe step. Newest-first is a preference, not a rule that forces every older revision to wait.

The planner decides the exact surviving old counts. A partial reduction keeps at least one group of every surviving required role. A planned transition to zero retires those roles together. Separate Kubernetes writes are not atomic; if an earlier partial write has already left a role at zero, a later reconcile must recover from that observed state without recreating old replicas.

The executor looks for safe ordinary old-revision progress before accepting a target-revision reduction. It does not independently remove extra old replicas beyond the planner's decision.

### Preserve availability when reducing the target revision

Target-revision scale-down reuses the old-drain availability calculation: treat the target as the revision being reduced and old revisions as unchanged capacity. It does not run the old rollout fraction curve backward.

An individual plan reduces either old replicas or target replicas, not both. This avoids spending the same Ready group twice. Growth of another role can accompany a reduction, but its future readiness cannot pay for that reduction.

When every old role has been renamed, the target revision replaces old capacity according to the least-ready target role's fraction. Target scale-down must preserve any replacement capacity already needed to keep those old roles above their floors. A permissive new-role budget is not permission to remove Ready target groups that are now protecting stricter old-role budgets.

During overlap, an already-created required target role keeps its first group. With no old capacity left, the target can converge exactly to the desired count, including zero.

### Keep bounded recovery

This policy retains the existing bounded recovery rules rather than promising that every cluster configuration can progress:

- Only after ordinary progress is blocked, the planner may issue the existing bootstrap surge for a missing required target role. With `AdvanceRollout`, a Ready role blocked by coordination at its latest ceiling may also need one such group. The allowance cannot be stacked while that role is already over its ceiling or waiting for readiness.
- The existing same-role scheduler fallback remains a last resort after ordinary and bootstrap progress are unavailable. It requires a persistent scheduler `Unschedulable` condition, an over-ceiling target group, and an old group of that role that can release capacity. It adds at most one to the unavailable allowance without taking a positive availability floor to zero. Slow startup alone does not qualify.
- Fully renamed roles keep #1105's bounded startup behavior: start missing required target roles before repeated growth, use observed reductions in old Spec to bound zero-surge growth, and retain safe correction of already-issued target surplus. Renaming does not add a new rule that deletes another old replica just because a new Pod is unschedulable.

These exceptions remain explicit planner decisions validated by the executor. They do not turn `maxSurge` into a guarantee that every possible cluster-capacity deadlock will be resolved automatically.

### How one reconcile works

```text
read latest desired replicas and the scaling policy
observe old and target Spec, observed Ready, and retained Ready

for old revisions in preference order:
    ask the planner for a safe step using all revisions' capacity
    prefer ordinary old progress over target reduction or recovery

if no old replicas remain:
    plan exact target convergence

validate the chosen step
apply its reductions before its growth
reconcile again from observed state
```

The executor uses the same observed revision objects throughout the decision. A concurrent target edit triggers another reconcile; a stale LWS observation causes the write guard to reject that write and retry. There is no transaction across all LWS objects.

### Completion and status

With `AdvanceRollout`, completion requires, for every role:

```text
old Spec == 0
target Spec == latest desired
target retained Ready == latest desired
```

The exact Spec check matters: `target Spec >= desired` would call a reduced rollout complete while excess target groups remain. A desired count of zero must also result in target Spec zero.

RoleScaler status continues to report observed replicas across revisions, so it may temporarily exceed desired replicas while groups terminate. The controller continues reconciling while target convergence or rollout readiness is incomplete.

### API and compatibility

Add an optional `DisaggregatedSetScalingPolicy` to `DisaggregatedSetSpec`, containing the `duringRollout` enum. The CRD defaults that field to `RolloutCoupled`. An omitted policy, an empty policy, and explicit `RolloutCoupled` all preserve default behavior; only explicit `AdvanceRollout` enables the new behavior.

Replica counts and this policy remain outside the application revision hash. Changing either does not create another revision. Switching back to `RolloutCoupled` stops further opt-in reductions; it cannot undo reductions already applied.

The controller and CRD must both support the new field before it is used. Remove the opt-in before downgrading to a version without it. The child workloads remain ordinary LWS objects and no separate scaling-operation state needs migration.

### Test plan

[X] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Unit tests

Use planner transitions, with target or readiness changes between reconciles, and check safety after each action:

- Omitted, empty, and explicit default policies preserve existing behavior.
- Scale-up continues within bounds while target Spec exceeds Ready.
- Scale-down handles desired counts above, equal to, and below current target Spec, including zero.
- A safe old-revision step is preferred to target scale-down, including when another old candidate is blocked.
- Target scale-down remains safe when old capacity cannot absorb the reduction.
- Old and target reductions cannot double-spend Ready capacity; proposed growth cannot fund a reduction.
- Incomplete revisions, healthy individual roles, pending deletions, and already-degraded availability retain #1105's protections.
- Target correction preserves already-used replacement capacity after all old roles are renamed.
- Moving targets can release physical excess without bypassing availability or splitting old revisions.
- Opposite role changes, unequal role sizes, interrupted rollouts, and repeated edits converge when valid targets stabilize and groups become Ready.
- Bootstrap and scheduler fallback remain bounded; defaults and renamed-role safeguards do not regress.
- Replica and policy edits do not change the application revision hash.

Keep validation tests separate from planner expectations where practical, so they can reject unsafe hand-written steps as well as verify planner-produced ones.

#### Integration tests

- Change a RoleScaler target during a rollout and verify the latest target reaches the planner only through the selected policy's rules.
- Verify Static roles use the same policy.
- Verify all downscale writes happen before growth writes in the chosen step.
- Verify exact completion and continued reconciliation when only target excess remains.
- Verify stale observations reject scale writes and do not weaken availability accounting.

#### End-to-end tests

Use a disposable cluster, direct replica changes, and deterministically delayed readiness:

1. Compare `RolloutCoupled` and `AdvanceRollout` while decreasing a target below already-created target Spec.
2. Under `AdvanceRollout`, increase targets while some target groups are not Ready; verify another bounded batch can be requested.
3. Release readiness and verify old-first progress followed by exact convergence.
4. Check the configured budgets, required-role completeness, and retained readiness throughout the run, not only its final counts.

### Graduation criteria

This behavior graduates with DisaggregatedSet and `DisaggregatedSetRoleScaler`. Alpha requires moving-target unit coverage and deterministic rollout-time scaling tests. Beta requires production feedback, especially on target oscillation and explaining blocked rollout steps to operators.

## Risks

**Autoscaler feedback:** status includes old and target capacity, including surge. Rapidly changing targets can cause churn. This policy does not replace autoscaler stabilization windows.

**Wasted startup work:** a target reduction can remove recently started target groups. Preferring safe old-revision progress reduces that cost but cannot eliminate it when targets change substantially.

**More possible transitions:** target Spec is no longer monotonic. Reusing availability checks, separating old and target reductions, and testing repeated edits are necessary to keep that behavior safe.

**Insufficient cluster capacity:** a valid replica plan does not guarantee schedulability. Budgets and bounded recovery may leave a rollout waiting; operators remain responsible for capacity and workload resource changes.

## Alternatives

**Always enable scaling during rollout.** This would silently change existing workloads' response to downscales. An explicit policy preserves compatibility.

**Only support scale-up.** The existing planner already supports bounded growth, but that leaves excess target capacity until the rollout finishes.

**Let the executor remove extra replicas independently.** This would duplicate or bypass the planner's availability and revision checks. The planner should remain the only authority for rollout targets.

**Restart the rollout on each target change.** This would create churn under HPA and discard useful progress. Recomputing from observed state is simpler and restart-safe.

## Implementation history

- 2026-09-02: Initial KEP draft.
- 2026-10-12: Align the proposal with the opt-in implementation, shared replica-group observations, and current rollout-availability safeguards. Clarify candidate selection, bounded recovery, target reductions, and exact completion.
