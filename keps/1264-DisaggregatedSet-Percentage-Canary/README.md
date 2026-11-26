# KEP-1264: DisaggregatedSet Percentage Canary

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
- [Proposed behavior](#proposed-behavior)
  - [Choose how much to replace](#choose-how-much-to-replace)
  - [Choose groups or whole slices](#choose-groups-or-whole-slices)
  - [Round to whole groups](#round-to-whole-groups)
  - [Promote or reduce the canary](#promote-or-reduce-the-canary)
  - [Scale while the canary is held](#scale-while-the-canary-is-held)
  - [Replace B with C while keeping A](#replace-b-with-c-while-keeping-a)
- [Remember which version to keep](#remember-which-version-to-keep)
- [Safety and limitations](#safety-and-limitations)
- [Upgrade and downgrade safety](#upgrade-and-downgrade-safety)
- [Status and Argo CD](#status-and-argo-cd)
- [Implementation outline](#implementation-outline)
- [Test plan](#test-plan)
- [Alternatives](#alternatives)
- [Implementation history](#implementation-history)
<!-- /toc -->

## Summary

Add `spec.rolloutStrategy.canary.percent` so an operator can try a new application version on part of a DisaggregatedSet before replacing the rest.

For example, with four Prefill groups and eight Decode groups, `canary.percent: 25` runs one Prefill and two Decode groups on the new version. The remaining groups stay on the previous version. The rollout waits there until the operator changes the percentage.

The same field applies to every role, including virtual roles (sub-roles), with either Ordinal or Hash group identity. It can also divide whole slices: eight slices of 1 Prefill + 1 Decode can run two complete slices on B and six on A at 25%. Increasing the percentage promotes the new version. Decreasing it restores more of the previous version, while its templates are retained.

## Motivation

Today a rollout keeps replacing the old version as readiness permits. Being Ready is not enough to know whether a new engine behaves well under real traffic: an operator may want to inspect errors, latency, or model output before continuing.

A percentage provides an explicit place to pause. It also lets promotion tools use one value instead of calculating separate counts for every role and updating those counts whenever an autoscaler changes demand.

This percentage controls replica groups or complete slices. A replica group contains a leader and its workers; it may use several Pods or GPUs. A 25% canary does not itself route 25% of requests to the new version. Traffic routing and application checks remain the responsibility of the deployment's existing tools.

## Proposed behavior

### Choose how much to replace

```yaml
spec:
  rolloutStrategy:
    canary:
      percent: 25
      dimension: Groups
```

The percentage is optional and accepts an integer from 0 to 100. Its default is `nil` (omitted). **Omitted or `100` means an ordinary rollout without a canary hold:** finish replacing the old version and allow its LWS objects to be cleaned up. In the examples below, **A** is the previous version kept as the **baseline**, and **B** is the new version described by the desired Pod templates.

For four Prefill groups and eight Decode groups, the completed steps are:

| `canary.percent` | A: Prefill / Decode groups | B: Prefill / Decode groups | Result |
| --- | --- | --- | --- |
| `0` | 4 / 8 | 0 / 0 | Keep the workload on A. |
| `25` | 3 / 6 | 1 / 2 | Try B on part of each role. |
| `50` | 2 / 4 | 2 / 4 | Run half of each role on B. |
| `100` or omitted | 0 / 0 | 4 / 8 | Finalize: accept B and allow A's LWS objects to be cleaned up. |

These are the counts after each step settles. During a step, some groups may still be starting or terminating. The usual rollout checks control that transition.

Changing only the percentage does not create another application revision. Set the percentage together with the template change to control how that rollout starts. If there is no previous version, initial creation starts the full desired workload regardless of the percentage.

**Reversal is supported during a partial canary, not after finalization.** Use a hold such as `90` or `99` while evaluating B if you still need to reduce its share. Once the rollout completes at `100`, lowering the percentage cannot restore retired A templates; use a Git revert for rollback.

After B finishes at `100` (or with the percentage omitted), a new C rollout uses B as its baseline, without a separate finalization edit. Set the next starting percentage together with C's templates. Leaving `100` configured does not automatically pause the next template update below 100%.

### Choose groups or whole slices

`spec.rolloutStrategy.canary.dimension` selects what the percentage divides:

| Value | Behavior |
| --- | --- |
| `Groups` (default) | Split each role or sub-role inside every slice. |
| `Slices` | Replace complete slices, in ascending slice-index order. |

For eight slices of 1 Prefill + 1 Decode, use:

```yaml
spec:
  slices: 8
  rolloutStrategy:
    canary:
      percent: 25
      dimension: Slices
```

Slices 0 and 1 roll to B; slices 2 through 7 stay on A. At 50%, slices 2 and 3 also roll to B. Returning to 25% restores those same two slices to A. Each selected slice rolls normally within its own availability and surge budgets; switching slices is not instantaneous.

Choose `Groups` (the default) or `Slices` in the manifest. There is no automatic choice based on replica counts and no internal dimension annotation.

Do not change dimension during a partial canary. Slice-count changes are also rejected during partial `Slices` canaries; finish at `100` or remove the percentage first. **Slice autoscaling is outside this proposal.** Existing role autoscaling remains supported under its existing restrictions.

### Round to whole groups

The controller calculates the split separately for each role, or each sub-role when a physical role has sub-roles. It repeats that calculation independently in each slice.

For a percentage between 1 and 99, round the new share up to a whole group, while keeping at least one group on both versions. For example, 25% of four groups is one new group; 99% of two groups is still one old and one new. Use `100` to remove the last old group.

For desired count `d >= 2` and percentage `0 < p < 100`, the exact rule is:

```text
new = min(d - 1, max(1, ceil(d * p / 100)))
old = d - new
```

A role with only one desired group cannot be split this way. In `Groups` mode, keep that whole slice on A and continue applying demand changes there. Otherwise B would lack a required role and could not serve. If a held 4P/8D workload changes to 1P/16D, it converges to 1P/16D on A rather than freezing at the previous counts. When all active roles have at least two groups again, the requested split can resume. The ordinary `Progressing` message explains this limitation; it is not reported as a successful canary hold.

In `Slices` mode, apply the same rounding rule to the number of slices. One slice cannot be divided; it stays on A while role scaling continues.

### Promote or reduce the canary

Changing `25` to `50` grows B's share and reduces A's share. Changing `50` back to `25` does the opposite: the controller starts more groups using A's existing templates and removes excess B groups.

Both directions use the existing rollout planner. For a role with four groups, moving from two A + two B to three A + one B illustrates the budget choices:

- With `maxSurge: 1` and `maxUnavailable: 0`, an extra A group can start and become Ready before a B group is removed.
- With `maxSurge: 0` and `maxUnavailable: 1`, the controller may remove a B group first to make room. That temporary reduction is explicitly allowed by the budget.

Restoring A preserves its saved templates, rollout settings, and original replica-count annotations. Newly requested groups count toward capacity usage, but cannot be counted as Ready before they are actually Ready.

At `0`, B's LWS objects remain with `spec.replicas: 0`. They can be reused if the operator raises the percentage again.

> **To roll back permanently, restore A's templates in Git.** Setting `canary.percent: 0` stops running B, but leaves B's templates desired. Raising the percentage or removing the field without restoring A's templates could deploy B again.
>
> In the same Git change, we recommend removing `canary.percent` to restore its default, `nil` (field omitted). **This means no canary hold.** For the next canary release, explicitly set a percentage such as `25` together with its new templates.

If the restored templates match the surviving baseline, A becomes the desired revision and the controller completes the return to A, even without resetting the percentage. If A has already disappeared, restoring its templates with the percentage omitted starts an ordinary rollout. Lowering the percentage alone cannot recreate a version that has been fully removed.

### Scale while the canary is held

The split follows the latest desired replica count, whether it comes from a Static role or an External scaler. For one role held at 25%:

| Change | Desired groups | A groups | B groups |
| --- | --- | --- | --- |
| Initial hold | 4 | 3 | 1 |
| Scale up | 8 | 6 | 2 |
| Scale down again | 4 | 3 | 1 |

Each role follows its own desired count. If Prefill grows while Decode stays the same size, the controller uses those current counts to coordinate progress. It does not keep judging Prefill against its size before the scale-up.

**During an active canary, `Deferred` and `Immediate` are ignored for both `scaleUpDuringRollout` and `scaleDownDuringRollout`.** Both revision shares resize to reach the requested split. This includes removing already-started B groups when demand or the percentage decreases. Otherwise a deliberately held rollout could prevent scale-down indefinitely.

Readiness and rollout safety checks still apply. The configured scaling policies resume at `100`, when the percentage is omitted, or when there is no active canary, such as initial creation or a rollout whose old history has already gone.

An existing role can reach zero and later grow again using its saved templates. At whole-set zero, keep both A and the current target's LWS objects at zero. Resuming restores the same split instead of silently promoting B. In slice mode, role resizing keeps each slice on its selected version; it does not choose a different slice cohort.

### Replace B with C while keeping A

An operator may change the canary templates again before finishing the rollout. At a 50% hold, A must remain the baseline while C replaces B in the other half.

For four Prefill and eight Decode groups:

| Stage | A: Prefill / Decode | B: Prefill / Decode | C: Prefill / Decode |
| --- | --- | --- | --- |
| B is held at 50% | 2 / 4 | 2 / 4 | 0 / 0 |
| C has replaced B | 2 / 4 | 0 / 0 | 2 / 4 |

While B is being replaced, B and C temporarily share the canary part. With demand and percentage unchanged, this replacement leaves A's LWS specs and existing Pods unchanged. B is not allowed to take over A's retained share merely because B is now also an older revision.

The replacement uses rollout budgets calculated from the full desired workload, and A's serving groups still contribute to availability. The hold is reached again when C supplies the requested canary share and the intermediate B groups have drained.

With `Slices`, the selected half rolls from B to C while the other half remains on A. Keep A's zero-replica LWS objects even in selected slices so a later reduction can restore A there.

## Remember which version to keep

The controller needs to remember: **“A is the baseline for this canary.”** Revision IDs identify the templates in each LWS, but do not record that choice.

It records the choice on A's LWS objects using this internal annotation:

```yaml
metadata:
  annotations:
    disaggregatedset.x-k8s.io/canary-stable: "true"
```

**Operators do not set this annotation.** The controller writes it before making canary scaling changes and reuses it on later reconciliations, after restarts, and when B is replaced by C. Marking A directly avoids copying the choice between successive canary versions or adding another resource to store it. When A's LWS objects are eventually deleted, their markers disappear too.

Choosing the oldest revision on every reconciliation would work when creation times remain unambiguous. The marker makes the earlier choice explicit instead of continually inferring it from timestamps, which can tie. It does not certify that A passed application checks: “baseline” means the version retained for this rollout, not a release approved by Argo CD or an operator.

For example, A has replicas and B's LWS exists at zero replicas, with the same recorded creation time. The controller can choose A because B has no replicas yet. It records A before growing B. Once both have replicas, timestamps alone would be ambiguous and the rollout would block; the marker preserves the choice already made.

The selection rules are:

1. Reuse the marked revision while its LWS templates exist, even at zero replicas.
2. Without a marker, choose the oldest revision that still has requested groups. The current implementation compares each revision's newest LWS creation time.
3. If there is no marker and the oldest times tie, or different revisions carry conflicting markers, leave workloads unchanged and explain the missing identity in the ordinary `Progressing` condition rather than guess.
4. Check that the chosen baseline still has the roles needed by the desired workload. Do not silently replace an incompatible baseline with a younger canary.

A marker cannot resolve an ambiguous first choice that has never been recorded. In that case, remove `canary.percent` and finish the existing rollout before starting another canary.

## Safety and limitations

The controller keeps the existing planner and group-assignment safeguards. `maxSurge` normally limits extra groups, and `maxUnavailable` normally limits the temporary loss of serving capacity. The [existing recovery rules](../1022-DisaggregatedSet-Scaling-During-Rollout/README.md#keep-bounded-recovery) can still make limited exceptions to get a stuck rollout moving; this feature does not add new emergency overrides.

Availability is calculated for complete revisions. A Ready Prefill group in A and a Ready Decode group in B do not form a working A or B deployment. Groups still starting, or already committed to deletion, cannot provide replacement readiness.

Ordinary replica-count changes do not stop reconciliation. Some template changes still require finishing the canary first:

| Situation | Behavior and next step |
| --- | --- |
| An active role has only one desired group at 1–99%. | In groups mode, keep the slice on A and continue scaling. Slice mode can canary complete copies instead. |
| A new External role has not received its first scaling request. | Its initial count of one may prevent splitting the slice; scaling still continues when its request arrives. |
| Roles are renamed, or ordinary roles are changed into sub-roles. | The controller cannot map the old baseline onto the new layout. Remove `canary.percent` and finish the layout change before starting another canary. |
| An existing role or sub-role reaches zero, including after a partially completed write. | Restore it from its retained LWS when demand returns. Zero replicas do not mean the template is missing. |
| All roles scale to zero during a canary. | Retain both revisions' LWS objects. Restore the same split when demand returns. |
| The selected baseline's required LWS template is actually missing. | The controller cannot invent it. Leave workloads unchanged and explain this in `Progressing`; restore the layout or finish the canary. |
| The canary finishes at `100` or with the percentage omitted. | Accept the current version and allow the old baseline's LWS objects to be cleaned up. |
| The canary was finalized and its baseline was retired. | Percentage changes alone cannot recreate deleted templates. The current revision scales normally. |

Small replica counts can differ substantially from the requested percentage because groups cannot be divided. Rollout progress also still depends on readiness, cross-role coordination, and enough schedulable capacity.

## Upgrade and downgrade safety

**Upgrade both the LWS controller and its CRDs before using canary fields.** An older controller does not enforce the hold, and older CRDs can discard unsupported fields. Either mismatch can turn an intended canary pause into a full rollout. Do not start a canary until the upgraded controller is running and the API preserves the configured `canary.percent` and, if used, `canary.dimension`.

Before downgrading to a version without this support, use the supporting controller to finish every active canary:

1. **Choose which version to keep.** To keep B, set `canary.percent: 100` and wait until all desired groups are Ready on B. To return to A, restore A's templates in Git and wait until the rollback finishes. Setting the percentage to `0` alone does not restore the desired templates.
2. **Confirm completion and remove unsupported settings.** Remove the `canary` block from Git or the other source of truth. Wait for the controller to apply that change and report `Available=True` with reason `AllRolesReady` for the current generation and replica demand.
3. Only then downgrade the controller and CRDs. Keep canary fields out of subsequent updates until both support them again.

**An explicit `100` finalizes the canary, just like omitting the percentage.** Do not downgrade while relying on a partial hold or on retained templates for a later reversal.

## Status and Argo CD

The controller reports a successful hold only when **every rolling slice** has reached the requested total and split, every requested group is Ready, intermediate canary revisions have drained, and no further rollout step is needed.

| Status | Meaning |
| --- | --- |
| `Progressing=True`, reason `CanaryHeld`, **`Available=False`** | The current generation has reached its requested partial hold. All requested groups can be Ready and serving even though the rollout is deliberately incomplete. |
| `Available=True`, reason `AllRolesReady` | The rollout has completed to the current templates, without an active canary session. |
| `Progressing=True`, reason `RolloutInProgress` | Work is still in progress, or the message explains why the requested canary cannot currently run. There is no separate `CanaryBlocked` state. |
| `MinimumReplicasAvailable` | The separate signal for whether minimum serving capacity is available. |

**A healthy partial canary has `Available=False`.** Today `Available` and `Progressing` are mutually exclusive: `Available` means that every desired group is Ready on the current templates, not simply that the application is serving. Do not page on `Available=False` alone during an intentional hold. Monitor `MinimumReplicasAvailable` for each role's configured serving-capacity floor, together with application health, error rate, and latency. A satisfied floor does not prove application health, and reaching the hold is not automatic approval to promote.

Stock Argo CD can sync the percentage field and use a configured health check to recognize these conditions. An operator or Kargo can promote through Git by changing the percentage in successive commits. If another controller writes the percentage directly, Argo CD must be configured not to overwrite it during sync.

For External scaling, checks must also use the current RoleScaler's `spec.replicas`. A scaler update does not change the DisaggregatedSet generation, so `observedGeneration` alone cannot prove that status reflects the latest demand.

The [Argo CD documentation PR](https://github.com/kubernetes-sigs/lws/pull/1310) provides configuration and examples. No Argo CD source change is required. Native Argo Rollouts `workloadRef` support, traffic weights, and application analysis are outside this proposal; any such automation must explicitly connect those tools to the percentage and status checks.

## Implementation outline

The [implementation PR](https://github.com/kubernetes-sigs/lws/pull/1307) adds the API field and validation, then converts each role's requested split into limits for the existing planner.

Normal forward planning replaces A with the canary while retaining A's required share. When A needs to grow, the same planner is used in the other direction: A provides replacement groups and canary revisions may shrink. The controller checks these plans before applying them, preserves A's historical settings, and waits when another safe step requires readiness.

The controller recalculates the split and checks progress using the latest observations. The annotation records which revision is the baseline; it does not store replica targets or the next rollout step.

A settled hold waits for DisaggregatedSet or observed workload events, without a periodic ten-second retry. Work still awaiting scheduling keeps the existing grace-period checks.

## Test plan

- Check rounding, valid percentage values, and that percentage-only changes do not create revisions.
- Exercise promotion, partial reversal, reduction to zero, re-promotion, and Git template rollback with different surge and unavailability budgets. Verify that both `100` and omission finalize, clean up A, and make B the next canary's baseline.
- Resize both shares with Ordinal and Hash groups, ordinary roles and sub-roles, and both configured scaling policies. Include asymmetric Prefill/Decode changes and controller restarts.
- Keep A's workloads, Pod identities, and sub-role assignments while B rolls to C. Check multiple historical revisions and incomplete serving revisions.
- Check baseline markers, ambiguous history, singleton roles, incompatible layouts, whole-set scale-to-zero, and every-slice hold reporting.
- Validate API persistence and webhook behavior, plus Argo CD health and field ownership.
- Exercise eight-slice promotion, reversal, A → B → C, and zero/resume. Reject unsupported dimensions and dimension/slice-count changes during partial slice canaries; allow changes when finalizing.
- Use real Pod/native-workload observations for ordinary and virtual-role canaries, with both Ordinal and Hash identity.
- Use the [harness and recorded traces](https://github.com/kubernetes-sigs/lws/pull/1309) to check real groups, readiness gates, autoscaler handoff, and successive rollout steps.

## Alternatives

[PR #1265](https://github.com/kubernetes-sigs/lws/pull/1265) proposes a separate integer partition for each role. That follows StatefulSet and LWS conventions and lets roles choose different holds. A promotion tool would need to calculate and update every partition as desired counts change.

This proposal uses one percentage and lets DisaggregatedSet calculate the counts for every role and sub-role. It is implemented directly on the existing planner and does not depend on the per-role partition API.

## Implementation history

- 2026-11-02: propose a percentage shared by all roles, as an alternative to per-role integer partitions.
- 2026-11-06: add canary reversal, proportional resizing, Git rollback guidance, and stock Argo CD configuration.
- 2026-11-06: retain A while a held canary changes from B to C.
- 2026-11-13: add whole-slice canaries and a once-per-canary auto choice; keep scaling at zero or one group and retain the baseline through scale-to-zero.
- 2026-11-24: finalize at 100%; nest settings under `canary`; retain only explicit `Groups` and `Slices` dimensions.
