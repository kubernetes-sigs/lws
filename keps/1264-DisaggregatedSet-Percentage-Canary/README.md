# KEP-1264: DisaggregatedSet Percentage Canary

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
- [Proposed behavior](#proposed-behavior)
  - [Choose how much to replace](#choose-how-much-to-replace)
  - [Choose groups or whole slices](#choose-groups-or-whole-slices)
  - [Round to whole groups](#round-to-whole-groups)
  - [Promote the canary or roll back through Git](#promote-the-canary-or-roll-back-through-git)
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

The same field applies to every role, including virtual roles (sub-roles), with either Ordinal or Hash group identity. It can also divide whole slices: eight slices of 1 Prefill + 1 Decode can run two complete slices on B and six on A at 25%. Increasing the percentage promotes the new version. Decreases during a partial canary are rejected; rollback uses the previous templates from Git.

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

**Promotion only moves forward.** During a partial canary, `25 → 50` is allowed, but `50 → 25` and `25 → 0` are rejected. Keep the percentage unchanged to pause promotion. To roll back, restore A's templates in Git and remove the percentage in the same change.

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

Slices 0 and 1 roll to B; slices 2 through 7 stay on A. At 50%, slices 2 and 3 also roll to B. Returning to 25% is rejected. Each selected slice rolls normally within its own availability and surge budgets; promotion is not instantaneous.

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

A role starting with only one admitted group cannot be split this way. In `Groups` mode, keep that whole slice on A until every active role can run on both versions. Demand changes still follow the selected scaling policies. Otherwise B would lack a required role and could not serve.

**Do not tear down an already-running canary just because one role scales from two groups to one.** Keep its last A group and its last canary-side group. For example, at 50%, A and B each have 1 Prefill + 4 Decode. An Immediate Prefill request of one keeps those same groups; Decode does not restart. Returning to two requires no new groups. Other roles continue following their own scaling policies, and loading groups are protected too: retention is based on issued groups, not readiness.

This keeps **one extra group per affected role or sub-role in each slice**, potentially for the whole partial canary. It consumes the first configured surge slot. With `maxSurge: 0`, keeping the pair is an explicit one-group exception to the new desired-plus-surge ceiling. The existing bounded bootstrap recovery may still temporarily borrow a further slot during B → C replacement; retention does not grant a separate protected copy of every old canary revision.

The requested count remains one in the spec and status; observed counts show the extra group. `Progressing` explains the delayed final reduction, and `status.canary.settled` stays false rather than claiming an exact hold. Demand returning to two removes the excess. An admitted zero request, promotion to `100`, or omitting the percentage releases retention. Deferred scale-down still queues reductions as usual. Starting a new canary at one does not create a second copy automatically; use an autoscaler minimum of two if the canary must start and settle without this limitation.

In `Slices` mode, apply the same rounding rule to the number of slices. One slice cannot be divided; it stays on A while role scaling continues.

### Promote the canary or roll back through Git

Changing `25` to `50` grows B's share and reduces A's share within the rollout budgets. Admission rejects percentage decreases during a partial canary. This follows the forward-only intent of partitioned rollouts, but rejects decreases explicitly instead of accepting a setting that cannot undo completed updates.

Start at `0` to keep B's LWS objects at zero before the first promotion. Once promotion starts, returning to `0` is not an abort operation.

> **To roll back, restore A's templates in Git and remove `canary.percent` in the same change.** Removing the percentage alone would finish deploying B. With A's templates restored, it starts an ordinary rollout back to A, subject to readiness and availability budgets. For the next canary release, set an initial percentage together with its new templates.

If the restored templates match the surviving baseline, A becomes the desired revision and the controller completes the return to A. If A has already disappeared, its restored templates recreate it through an ordinary rollout.

### Scale while the canary is held

During a partial canary, **only `Immediate` applies demand changes in its direction**. This applies to both Static replicas and External scaler requests. The two settings are independent:

| Scale up | Scale down | Demand changes during the canary |
| --- | --- | --- |
| Deferred | Deferred | Keep the last admitted counts. |
| Immediate | Deferred | Apply increases; queue decreases. |
| Deferred | Immediate | Queue increases; apply decreases. |
| Immediate | Immediate | Apply both directions. |

With Immediate in both directions, one role held at 25% behaves as follows:

| Change | Desired groups | A groups | B groups |
| --- | --- | --- | --- |
| Initial hold | 4 | 3 | 1 |
| Scale up | 8 | 6 | 2 |
| Scale down again | 4 | 3 | 1 |

Each role/sub-role has its own admitted count. With Deferred scale-up, a request from 4 to 8 stays at 3 A + 1 B. Promoting from 25% to 50% still proceeds, producing 2 A + 2 B. Likewise, changing B's templates to C continues within that admitted size. Deferred does not freeze rollout progress.

The controller records admitted counts in the internal `disaggregatedset.x-k8s.io/canary-admitted-replicas` annotation on A's LWS objects. The first record uses A's saved replica targets, not a sum that includes temporary surge. Later Immediate changes update the record before planning; it survives restarts and B → C. Status is not used as planner state.

Changing a Deferred direction to Immediate admits the **latest** request, not every intermediate autoscaler value. A demand of zero follows the same rule: Deferred scale-down queues it; Immediate can drain both sides to zero. Immediate scale-up can resume the same split from their retained templates. Unknown scaler targets pause reconciliation; they are not treated as zero.

Readiness, availability budgets, and safe deletion still govern admitted changes. At `100` or omission, ordinary rollout policies resume and the latest demand can be applied; finalization removes the saved canary counts. Initial creation without an older revision also uses ordinary policies. **Outside a partial canary, Deferred scale-up retains its existing readiness-paced behavior**, not this demand hold.

In slice mode, the same role-demand rules apply within each slice without changing its selected revision. Slice-count autoscaling remains outside this proposal. Queued demand remains visible in role status as the requested count; a hold with unapplied demand is not reported as a settled `CanaryHeld` step.

### Replace B with C while keeping A

An operator may change the canary templates again before finishing the rollout. At a 50% hold, A must remain the baseline while C replaces B in the other half.

For four Prefill and eight Decode groups:

| Stage | A: Prefill / Decode | B: Prefill / Decode | C: Prefill / Decode |
| --- | --- | --- | --- |
| B is held at 50% | 2 / 4 | 2 / 4 | 0 / 0 |
| C has replaced B | 2 / 4 | 0 / 0 | 2 / 4 |

While B is being replaced, B and C temporarily share the canary part. With demand and percentage unchanged, this replacement leaves A's LWS specs and existing Pods unchanged. B is not allowed to take over A's retained share merely because B is now also an older revision.

The replacement uses rollout budgets calculated from the full desired workload, and A's serving groups still contribute to availability. The hold is reached again when C supplies the requested canary share and the intermediate B groups have drained.

With `Slices`, the selected half rolls from B to C while the other half remains on A. Keep A's zero-replica LWS objects even in selected slices to preserve the same baseline identity throughout the canary.

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

The controller keeps the existing planner and group-assignment safeguards. `maxSurge` normally limits extra groups, and `maxUnavailable` normally limits the temporary loss of serving capacity. Retaining the last pair at demand one is the explicit capacity exception described above. The [existing recovery rules](../1022-DisaggregatedSet-Scaling-During-Rollout/README.md#keep-bounded-recovery) can still make limited exceptions to get a stuck rollout moving; there is no new scheduler-failure-triggered override.

Availability is calculated for complete revisions. A Ready Prefill group in A and a Ready Decode group in B do not form a working A or B deployment. Groups still starting, or already committed to deletion, cannot provide replacement readiness.

Ordinary replica-count changes do not stop reconciliation. Some template changes still require finishing the canary first:

| Situation | Behavior and next step |
| --- | --- |
| An active role starts with one desired group at 1–99%. | In groups mode, keep the slice on A and continue scaling. Slice mode can canary complete copies instead. |
| A running groups canary scales an active role down to one. | Keep its final A and canary-side groups, with one extra group and an explanatory status. Do not restart unrelated roles. |
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

1. **Choose which version to keep.** To keep B, set `canary.percent: 100` and wait until all desired groups are Ready on B. To return to A, restore A's templates in Git, remove the percentage, and wait until the rollback finishes.
2. **Confirm completion and remove unsupported settings.** Remove the `canary` block from Git or the other source of truth. Wait for the controller to apply that change and report `Available=True` with reason `AllRolesReady` for the current generation and replica demand.
3. Only then downgrade the controller and CRDs. Keep canary fields out of subsequent updates until both support them again.

**An explicit `100` finalizes the canary, just like omitting the percentage.** Do not downgrade while relying on a partial hold.

## Status and Argo CD

The controller reports a successful hold only when **every rolling slice** has reached the requested total and split, every requested group is Ready, intermediate canary revisions have drained, and no further rollout step is needed.

Tools can read `status.canary` without parsing condition messages or inspecting child annotations:

```yaml
status:
  canary:
    percent: 25
    dimension: Groups
    baselineRevisions: [revision-a]
    targetRevision: revision-b
    settled: true
```

`percent` is the observed request, not a measured traffic weight. `baselineRevisions` contains the distinct recorded baselines across slices and stays empty until their markers have been observed; status never guesses a baseline. `settled` means the same as `CanaryHeld`, not application approval. Replica counts remain in `status.roleStatuses` and its sub-role entries. This section is absent without an active partial canary, including initial creation without history, a Git revert to the baseline templates, and finalization at `100` or omission. It is informational: planning does not depend on status writes.

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

The controller recalculates the split from admitted demand and checks progress using the latest observations. The baseline marker identifies A; a separate per-role count map records admitted demand during the canary. Neither stores the next rollout step.

A settled hold waits for DisaggregatedSet or observed workload events, without a periodic ten-second retry. Work still awaiting scheduling keeps the existing grace-period checks.

## Test plan

- Check rounding, valid percentage values, and that percentage-only changes do not create revisions.
- Exercise forward promotion from zero and Git template rollback with different surge and unavailability budgets. Reject percentage decreases. Verify that both `100` and omission finalize, clean up A, and make B the next canary's baseline.
- Resize both shares with Ordinal and Hash groups, ordinary roles and sub-roles, and both configured scaling policies. Include asymmetric Prefill/Decode changes and controller restarts.
- Keep A's workloads, Pod identities, and sub-role assignments while B rolls to C. Check multiple historical revisions and incomplete serving revisions.
- Check baseline markers, ambiguous history, singleton roles, incompatible layouts, whole-set scale-to-zero, and every-slice hold reporting. Exercise all four scaling-policy combinations, restarts, coalesced requests, policy switches, and promotion/B → C with queued demand.
- Validate API persistence and webhook behavior, plus Argo CD health and field ownership.
- Exercise eight-slice promotion, A → B → C, and zero-demand/resume. Reject unsupported dimensions and dimension/slice-count changes during partial slice canaries; allow changes when finalizing.
- Use real Pod/native-workload observations for ordinary and virtual-role canaries, with both Ordinal and Hash identity.
- Repeat 2 ↔ 1 scaling without replacing retained Pods. Check loading groups, growing peers, restart recovery, zero demand, finalization, and B → C with zero surge or zero unavailability; protect only the last canary-side group, not every intermediate revision.
- Use the [harness and recorded traces](https://github.com/kubernetes-sigs/lws/pull/1309) to check real groups, readiness gates, autoscaler handoff, and successive rollout steps.

## Alternatives

[PR #1265](https://github.com/kubernetes-sigs/lws/pull/1265) proposes a separate integer partition for each role. That follows StatefulSet and LWS conventions and lets roles choose different holds. A promotion tool would need to calculate and update every partition as desired counts change.

This proposal uses one percentage and lets DisaggregatedSet calculate the counts for every role and sub-role. It is implemented directly on the existing planner and does not depend on the per-role partition API.

## Implementation history

- 2026-11-26: propose percentage-based canaries for groups or whole slices, with forward-only promotion, completion at 100%, Git rollback, Immediate-only demand changes during partial holds, baseline retention across template updates and scale-to-zero, protection against 2 ↔ 1 autoscaling restarts, and stock Argo CD integration.
