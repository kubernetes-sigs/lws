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
    scaleDownDuringRollout: Immediate
```

Both policies allow scale-up during a rollout, within the existing rollout budgets. This option controls scale-down: `Immediate` considers reductions while old revisions remain, preferring safe old-revision drains before reducing target replicas. `Deferred`, the default, preserves existing behavior and defers demand-driven target-revision reductions until old revisions finish draining. Old replicas can still be retired under either policy. `Immediate` means considered now, not instant or unconditional deletion.

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

1. Allow operators to opt into target-revision reductions during a rollout, while preserving scale-up support under both policies.
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

Add `spec.scalingPolicy.scaleDownDuringRollout` with two values:

| Policy | Scale-up during a rollout | Scale-down during a rollout |
| --- | --- | --- |
| `Deferred` (default) | Grow the target revision toward desired replicas within rollout bounds. | Preserve existing behavior: defer demand-driven target-revision reductions until old revisions drain. Old replicas can still retire during the rollout. |
| `Immediate` | Grow the target revision toward desired replicas within rollout bounds. | Use the latest surge ceiling, preferring safe old-revision reductions; reduce excess target replicas when old revisions must remain. |

The target comes from `roles[].spec.replicas` for Static roles or `DisaggregatedSetRoleScaler.spec.replicas` for External roles. Both use the same planner. The policy is set once on the DisaggregatedSet and applies to its roles and slices.

These choices govern demand-driven scaling, not whether any replica may disappear. Existing rollout retirement and recovery of excess groups during fully renamed-role rollouts continue under both policies.

### Scale up during a rollout

Consider two roles, Prefill and Decode. Both have the following counts and budgets; all numbers below are replica groups, not individual Pods:

```text
                         Before target change
historical old baseline: 4
old Spec / Ready:        4 / 4
target Spec / Ready:     2 / 1
desired replicas:        6 -> 8
maxSurge:                2 per role
maxUnavailable:          1 per role
```

Under both policies, the latest desired count makes the ordinary surge ceiling `8 + 2 = 10` and the availability floor `8 - 1 = 7` per role. Only five groups per role are Ready: four old and one target. The planner keeps all four old groups rather than making this existing shortfall worse. It does not require seven Ready groups before allowing any progress.

The controller can still grow target Spec from two to four before the pending group becomes Ready. The existing pending-work allowance permits three unready target groups in this symmetric example, so one Ready group allows up to four requested groups. Once all four target groups become Ready, the old revision can shrink from four to three, retaining `3 + 4 = 7` Ready groups. Any further growth requested in that same plan does not pay for the reduction.

This is not an unlimited scale-up. Existing old Spec consumes surge space, and new groups do not count as replacement capacity merely because their creation was requested.

### Scale down during a rollout

With `Immediate`, assume both roles have these counts:

```text
old Spec / Ready:        4 / 4
target Spec / Ready:     5 / 5
desired replicas:        8 -> 4
maxSurge:                1 per role
maxUnavailable:          1 per role
```

The latest ceiling is five groups per role, but nine have already been requested. The planner first considers reducing old replicas. Here, the complete Ready target revision can replace the old one, so the old revision can drain safely. Target Spec then converges from five to four.

If old replicas cannot safely be removed, the controller may reduce target replicas instead. This is useful when the target is above its newly requested size but an old revision still provides capacity that must be retained. The same availability checks apply to both choices.

A lower target lowers the availability floor for both old- and target-revision reductions. A higher target raises it. Neither change is permission to ignore observed availability: the controller waits when no safe reduction exists and does not count newly requested groups as Ready.

### If the target changes again

With `Immediate`, for a sequence such as `8 -> 12 -> 5 -> 9`, each reconcile uses the latest target and the state currently observed in the cluster. There is no queue of obsolete targets to finish and no old revision is scaled back up. `Deferred` retains its existing External-role target-size clamp while old replicas overlap.

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

For a role still present in the desired topology, `Immediate` uses:

```text
ordinary surge ceiling = latest desired + maxSurge
old-drain floor        = max(0, latest desired - maxUnavailable)
target-drain floor     = max(0, latest desired - maxUnavailable)
```

Both reductions protect the same desired capacity. An old revision's smaller historical size does not lower the floor, including after another old revision finishes draining. Historical counts still govern old-side fractional progress and removed-role replacement; they do not cap a current role's availability requirement.

`Deferred` keeps the default surge ceiling `max(initial old replicas, effective desired) + maxSurge`; it is not frozen at the initial old size when demand rises. Its old-drain floor is `max(0, effective desired - maxUnavailable)`. For an External role that still overlaps old replicas, its effective desired count remains `max(latest scaler target, target Spec)`, preserving the existing deferral of target reductions. Static roles keep their existing behavior without this External-only clamp.

Percentage `maxUnavailable` values for current roles continue to resolve against the latest requested replica count, not the higher effective count retained by the `Deferred` clamp. This change does not alter budget resolution or removed roles' recorded integer budgets.

If a target decrease puts current Spec above the ceiling, ordinary growth for that role stops and the planner looks for safe reductions. An existing excess is not itself an error: a target edit can make an already-issued state exceed the new limit instantly.

If observed Ready capacity is already below the floor, the planner does not require the impossible before every action. It instead prevents its own reductions from worsening that Ready deficit. Retained Ready is used when checking the proposed action so a deletion already in flight cannot pay for a second deletion.

The proportional pending-work budget remains anchored to `max(initial old replicas, latest desired replicas)`, as in the existing planner. Moving the desired target does not remove that bound.

Removed or renamed roles retain #1105's separate protection. Applicable old revisions share their old availability baseline and recorded `maxUnavailable`, with the existing minimum allowance of one for a removed role. A new role's lower target or larger `maxUnavailable` must not silently replace an old role's budget.

For a removed role, the old baseline and Ready counts both come from old revisions that share no role with the target. An old revision that still shares a target role retires through that shared role's availability checks; its removed roles cannot also provide replacement credit for unrelated old revisions. Any replicas still serving the removed role in the target revision remain protected until their replacements are Ready.

### Keep roles coordinated

The planner keeps its existing fractional coordination. For example, Prefill with eight groups and Decode with four can advance along a shared curve such as `(1,1)`, `(2,1)`, `(3,2)`. The smaller role may be one rounded step ahead, but one role cannot run arbitrarily ahead of another.

The target side uses the current desired sizes. Old-side progress remains measured from the old revision's initial sizes. A reduction in desired size must not turn that historical fraction window into a permanent barrier to releasing excess capacity.

When disjoint old roles still need replacement, target coordination uses the full desired role sizes, without subtracting Ready capacity in other old revisions or retracting already-issued target counts. Those old revisions may still serve traffic, but cannot replace progress that must come from a complete target revision.

Old-side progress compares observed Spec with the applicable historical baselines. Once any removed role has positive progress, each required target role may start its first group even if an old singleton still holds the common fraction at zero. Surge and pending-work limits still apply, and no role repeats growth while a required target role has no retained Ready group. A baseline can exceed the replicas ever issued before an interrupted rollout, so this is logical rollout progress, not proof that GPUs have physically been freed.

Once every required target role has retained Ready capacity, the replacement-growth bound for each role also credits that shared progress:

```text
f = the slowest target role's retained-Ready fraction, capped at 1
replacement-growth limit = max(limit from old-side progress, floor(desired * f) + 1)
```

This bound applies during ordinary planning, not only after a stall: the controller can request the next group before observing another decrease in old Spec. Each role's actual surge limit, projected pending-work allowance, desired count, and fractional coordination still apply. Before every required role has retained Ready capacity, the missing-role-first rule and old-side progress bound above remain in force. Replica fractions do not reserve physical capacity or guarantee that the requested group can be scheduled.

Separately, with `Immediate`, or when any role's effective desired count exceeds its historical availability baseline, an old-side reduction for a role with a positive desired count may go beyond the historical fraction window by at most the existing excess:

```text
excess = max(0, total old Spec + target Spec - policy surge ceiling)
```

The policy ceiling is `desired + maxSurge` for `Immediate` and `max(initial old replicas, effective desired) + maxSurge` for `Deferred`. This exception only corrects physical excess. It cannot authorize extra growth, reduce below that ceiling by bypassing coordination, bypass availability, or leave only some required roles in an old revision. Once excess is gone, ordinary fractional coordination applies again. Removed roles have no new replica target to correct: they keep their old fraction window, even when their saved unavailable budgets differ. This does not enable demand-driven target reductions under `Deferred`.

### Prefer old-revision reductions

For an interrupted A-to-B-to-C rollout, the executor considers whole old revisions, not unrelated per-role fragments. Fully unready revisions are considered first; within the existing preference order, newer revisions come first. If a preferred candidate is blocked, another candidate may still have a safe step. Newest-first is a preference, not a rule that forces every older revision to wait.

The planner decides the exact surviving old counts. A partial reduction keeps at least one group of every surviving required role. A planned transition to zero retires those roles together. Separate Kubernetes writes are not atomic; if an earlier partial write has already left a role at zero, a later reconcile must recover from that observed state without recreating old replicas.

The executor looks for safe ordinary old-revision progress before accepting a target-revision reduction. It does not independently remove extra old replicas beyond the planner's decision.

### Preserve availability when reducing the target revision

Under `Immediate`, target-revision scale-down reuses the old-drain availability calculation: treat the target as the revision being reduced and old revisions as unchanged capacity, using the same desired-based floor. It does not run the old rollout fraction curve backward.

An individual plan reduces either old replicas or target replicas, not both. This avoids spending the same Ready group twice. Growth of another role can accompany a reduction, but its future readiness cannot pay for that reduction.

When every old role has been renamed, the target revision replaces old capacity according to the least-ready target role's fraction. Target scale-down must preserve any replacement capacity already needed to keep those old roles above their floors. A permissive new-role budget is not permission to remove Ready target groups that are now protecting stricter old-role budgets.

During overlap, an already-created required target role keeps its first group. With no old capacity left, the target can converge exactly to the desired count, including zero.

When correcting a target revision that exceeds its latest desired count, Ordinal and Hash use the same startup-preservation rule:

```text
if target Spec <= desired:
    do not correct the target size
else if every target group is confirmed unscheduled:
    allow the existing correction toward desired + maxSurge - old Spec
else:
    do not reduce the target below its final desired count

always keep the existing availability and first-group safeguards
```

A group has started scheduling when any of its Pods has a node assigned (`spec.nodeName`), even if it is still Pending or loading a model. A partially scheduled group therefore protects the role's startup progress too. Missing or incomplete observations do not prove that all groups are unscheduled; they use the same conservative bound. This information comes from the existing replica-group observation, without additional API reads. When every expected group and member Pod is present and unassigned, this check does not wait for controller status to catch up. Availability is still checked separately before any reduction.

For example, per role, old Spec and Ready are 4, target Spec is 5 with no Ready groups, desired is 4, `maxSurge` is 1, and `maxUnavailable` is 0. If any target Pod is assigned to a node, reduce the target to 4, not 1. This temporarily retains 8 total groups above the new ceiling of 5, preserving the startup work needed for the final workload. If all target groups are confirmed unscheduled, the existing correction can reduce the target to 1 while the old revision continues serving. The new revision later grows to its final 4 as the old revision drains.

In a mixed target, this deliberately keeps some unscheduled groups too. It avoids separate Ordinal and Hash victim-selection rules. The protection applies to reductions below the final target: genuinely excess replicas above that target can still be removed, including scheduled ones. Observing an unscheduled Pod does not reserve it; the scheduler may assign it after the observation and before native deletion. This rule avoids knowingly discarding needed startup work, not all possible scheduling races.

This changes ordinary opt-in target-size corrections only. It does not add reductions when target Spec is already at or below desired, change the default policy, or alter #1105's separate correction of surplus during fully renamed-role rollouts.

### Keep bounded recovery

This policy retains the existing bounded recovery rules rather than promising that every cluster configuration can progress:

- Only after ordinary progress is blocked, the planner may issue the existing bootstrap surge for a missing required target role. Each eligible, already-started role may also receive one extra group when `Immediate` is enabled or any role's effective desired count exceeds its historical availability baseline. This includes `Deferred` scale-up and addresses coordination stalls exposed by the stronger floor, even when all existing groups are Ready. The eligible role's existing target groups must all be retained Ready, and its total Spec must exactly equal its policy's surge ceiling. Other disjoint history does not disable this exception for a role that still has same-role old replicas; a fully renamed target role with no same-role old replicas cannot use it while disjoint old replicas remain. The allowance cannot be stacked while that role is already over its ceiling or waiting for readiness. The matching old-side excess correction above lets safe retirement clear the borrowed slot instead of getting stuck behind historical pacing; neither rule enables demand-driven target reductions under `Deferred`.
- The existing same-role scheduler fallback remains a last resort after ordinary and bootstrap progress are unavailable. It requires a persistent scheduler `Unschedulable` condition, an over-ceiling target group, and an old group of that role that can release capacity. It adds at most one to the unavailable allowance without taking a positive availability floor to zero. Slow startup alone does not qualify.
- Fully renamed roles start missing required target roles before repeated growth, use the old-side progress and retained-Ready bounds above, and retain correction of already-issued target surplus. Renaming does not add a new rule that deletes another old replica just because a new Pod is unschedulable.

Growth credited to retained Ready capacity is not a permanent reservation. If a required target role later loses all retained Ready groups, the existing disjoint-surplus repair can reclaim extra target groups, including Ready ones, where that repair is permitted. It preserves the last retained Ready group of each role that still has one and the protected old availability floor. This recovery is separate from demand-driven target-size correction and can therefore retract work protected from an ordinary demand decrease. The controller recomputes the bounds from current observations; it does not persist a ledger of earlier readiness credit.

These exceptions remain explicit planner decisions validated by the executor. They do not turn `maxSurge` into a guarantee that every possible cluster-capacity deadlock will be resolved automatically.

### How one reconcile works

```text
read latest desired replicas and the scaling policy
observe old and target Spec, observed Ready, retained Ready,
and whether all target groups are confirmed unscheduled

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

With `Immediate`, completion requires, for every role:

```text
old Spec == 0
target Spec == latest desired
target retained Ready == latest desired
```

The exact Spec check matters: `target Spec >= desired` would call a reduced rollout complete while excess target groups remain. A desired count of zero must also result in target Spec zero.

RoleScaler status continues to report observed replicas across revisions, so it may temporarily exceed desired replicas while groups terminate. The controller continues reconciling while target convergence or rollout readiness is incomplete.

### API and compatibility

Add an optional `DisaggregatedSetScalingPolicy` to `DisaggregatedSetSpec`, containing `scaleDownDuringRollout` of type `RolloutScaleDownPolicy`. The enum values are `Deferred` and `Immediate`; the CRD defaults the field to `Deferred`. An omitted policy, an empty policy, and explicit `Deferred` all preserve default behavior. Both policies permit scale-up and old-revision retirement; only explicit `Immediate` enables the new demand-driven target reductions during overlap.

Replica counts and this policy remain outside the application revision hash. Changing either does not create another revision. Switching back to `Deferred` stops further opt-in reductions; it cannot undo reductions already applied or disable existing rollout recovery.

The controller and CRD must both support the new field before it is used. Remove the opt-in before downgrading to a version without it. The child workloads remain ordinary LWS objects and no separate scaling-operation state needs migration.

### Test plan

[X] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Unit tests

Use planner transitions, with target or readiness changes between reconciles, and check safety after each action:

- Omitted, empty, and explicit `Deferred` policies preserve existing behavior; the API server defaults the new field and rejects invalid enum values.
- Scale-up continues within bounds under both policies while target Spec exceeds Ready; the floor follows desired replicas, and an existing Ready shortfall must not worsen.
- Interrupted same-role rollouts retain the desired-based floor after the larger old revision drains, rather than lowering it to a smaller remaining old baseline.
- Scale-down handles desired counts above, equal to, and below current target Spec, including zero.
- A safe old-revision step is preferred to target scale-down, including when another old candidate is blocked.
- Target scale-down remains safe when old capacity cannot absorb the reduction.
- Ordinal and Hash preserve the final target size when any target member is assigned to a node; entirely unscheduled targets retain bounded cancellation. Mixed, partially scheduled, and incomplete observations use the protective bound, and the executor rejects a step that bypasses it.
- Old and target reductions cannot double-spend Ready capacity; proposed growth cannot fund a reduction.
- Incomplete revisions, healthy individual roles, pending deletions, and already-degraded availability retain #1105's protections.
- Target correction preserves already-used replacement capacity after all old roles are renamed.
- Moving targets can release physical excess without bypassing availability or splitting old revisions.
- Disjoint replacement uses full target sizes in mixed histories and makes bounded progress before another old Spec reduction; shared-role bootstrap remains available, and later target readiness loss preserves the surplus-repair availability safeguards.
- Scale-up under `Deferred` can use fully-Ready coordination bootstrap and release safe old capacity above its policy's ceiling when a role's effective desired count exceeds its historical baseline, without stacking extra groups or permitting target reductions.
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

1. Compare `Deferred` and `Immediate` while decreasing a target below already-created target Spec.
2. Under both policies, increase targets while some target groups are not Ready; verify another bounded batch can be requested while preserving observed Ready capacity until the higher floor can be met.
3. Release readiness and verify old-first progress followed by exact convergence.
4. Check the configured budgets, required-role completeness, and retained readiness throughout the run, not only its final counts.

### Graduation criteria

This behavior graduates with DisaggregatedSet and `DisaggregatedSetRoleScaler`. Alpha requires moving-target unit coverage and deterministic rollout-time scaling tests. Beta requires production feedback, especially on target oscillation and explaining blocked rollout steps to operators.

## Risks

**Autoscaler feedback:** status includes old and target capacity, including surge. Rapidly changing targets can cause churn. This policy does not replace autoscaler stabilization windows.

**Wasted startup work:** prefer safe old-revision progress and avoid ordinary target corrections below the final desired count once any target group has started scheduling. Genuine final excess can still be removed, and a Pod may become scheduled after observation but before deletion.

**Temporary excess capacity:** preserving startup work can leave old plus target Spec above the latest surge ceiling, potentially for a long time if startup stalls. Mixed or incompletely observed targets also retain unscheduled groups. New growth remains bounded; the excess is resolved as readiness permits old-revision retirement.

**Slower old-revision retirement:** a higher desired count raises the availability floor even when old revisions were smaller. Bounded target growth can continue, but needed old capacity stays until replacements become Ready. If replacements cannot fit, more cluster capacity or an operator-adjusted rollout budget may be required.

**More possible transitions:** target Spec is no longer monotonic. Reusing availability checks, separating old and target reductions, and testing repeated edits are necessary to keep that behavior safe.

**Insufficient cluster capacity:** a valid replica plan does not guarantee schedulability. Budgets and bounded recovery may leave a rollout waiting; operators remain responsible for capacity and workload resource changes.

## Alternatives

**Always enable immediate scale-down during rollout.** This would silently change existing workloads' response to downscales. An explicit policy preserves compatibility.

**Only support scale-up.** The existing planner already supports bounded growth, but that leaves excess target capacity until the rollout finishes.

**Let the executor remove extra replicas independently.** This would duplicate or bypass the planner's availability and revision checks. The planner should remain the only authority for rollout targets.

**Restart the rollout on each target change.** This would create churn under HPA and discard useful progress. Recomputing from observed state is simpler and restart-safe.

## Implementation history

- 2026-09-02: Initial KEP draft.
- 2026-10-12: Align the proposal with the opt-in implementation, shared replica-group observations, and current rollout-availability safeguards. Clarify candidate selection, bounded recovery, target reductions, and exact completion.
- 2026-10-12: Preserve scheduled target startup during demand decreases with one rule for Ordinal and Hash. Use complete Pod observations for below-target cancellation; document temporary surge excess and observation limits.
- 2026-10-12: Rename the proposed API to `scaleDownDuringRollout: Deferred | Immediate`. Clarify that both policies allow scale-up and old-revision retirement, cap the old-drain availability floor at its historical baseline, and retain the desired-based floor when reducing the target.
- 2026-10-12: Keep full target sizes for disjoint replacement coordination, let progress in one old role start each target role once, and allow the next bounded group using retained target readiness. Preserve shared-role bootstrap in mixed histories and document that readiness loss can reclaim extra target groups through existing surplus recovery.
- 2026-10-12: Use the same old revisions for removed-role baselines and readiness accounting, including the replacement credit retained during target scale-down.
- 2026-10-12: Use the desired-based availability floor for both old and target reductions of current roles, replacing the historical cap. Preserve observed Ready capacity during scale-up, keep the floor across interrupted rollout phases, and retain removed-role history and replacement protection.
- 2026-10-12: Extend fully-Ready coordination bootstrap to `Deferred` when effective demand exceeds an old availability baseline, preserving all existing bounds. Clarify that percentage budgets still use the latest requested count rather than the External-role clamp.
- 2026-10-13: Let `Deferred` scale-up release safe old capacity above its policy's surge ceiling, matching coordination-bootstrap eligibility. Keep availability and revision completeness checks; do not enable demand-driven target reductions.
