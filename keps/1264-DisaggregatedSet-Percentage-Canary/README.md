# KEP-1264: DisaggregatedSet Percentage Canary

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
- [Proposal](#proposal)
  - [Replica counts and readiness](#replica-counts-and-readiness)
  - [Scaling and interrupted rollouts](#scaling-and-interrupted-rollouts)
  - [Rollback and explicit limitations](#rollback-and-explicit-limitations)
  - [Status and progressive-delivery tools](#status-and-progressive-delivery-tools)
- [Implementation](#implementation)
- [Test plan](#test-plan)
- [Alternatives](#alternatives)
- [Implementation history](#implementation-history)
<!-- /toc -->

## Summary

Add an optional `spec.rolloutStrategy.canaryPercent` to hold a DisaggregatedSet rollout at one percentage across all roles and sub-roles. It supports both Ordinal and Hash groups. This is an independent alternative to per-role integer partitions, built directly on the existing DisaggregatedSet planner. It does not enable the unsupported `spec.roles[].spec.rolloutStrategy.rollingUpdateConfiguration.partition` field.

## Motivation

A new inference engine should be able to serve a small amount of real traffic before the full workload is replaced. Today a DisaggregatedSet rollout keeps advancing as readiness permits. Operators need a deliberate hold, then promotion after manual checks or automated analysis.

One percentage avoids making the promotion tool calculate and continuously update a separate integer for every role, virtual role, and autoscaler target. This controls replica groups, not traffic weights or GPU capacity; the routing layer still decides where requests go.

## Proposal

```yaml
spec:
  rolloutStrategy:
    canaryPercent: 25
```

The field is an optional integer from 0 to 100. Omitted or 100 means no canary hold. Set 0 together with a template change to retain the baseline; raising the percentage advances the rollout and lowering it restores the baseline share while compatible baseline groups survive. Changing the percentage does not change the revision hash. Initial creation without an old revision still starts the full desired workload.

### Replica counts and readiness

For each logical role, independently in each slice, let `d` be its resolved desired group count and `p` the percentage. During a partial hold (`0 < p < 100`, `d >= 2`):

```text
new = min(d - 1, max(1, ceil(d * p / 100)))
old = d - new
```

The new count rounds up but keeps at least one group on each revision. At 25%, four groups become three old and one new; eight groups become six old and two new. For a physical role containing sub-roles, the formula applies to each sub-role, not the parent's aggregate. At 0%, all desired groups remain old.

The planner receives an internal old-group hold derived from this formula. Existing availability budgets, revision completeness, and group assignment still apply. A hold is not declared until every rolling slice has no further step and every requested group is Ready at the desired total and requested split. The cross-role coordination window can delay a faster role. The settled replica share follows the formula; it is not a traffic split or a guarantee of application quality.

### Scaling and interrupted rollouts

A percentage recalculates both shares when desired replicas change. A 25% hold at four groups is three old plus one new; increasing desired to eight becomes six old plus two new. Decreasing back to four returns to three old plus one new, under either ordinary scaling policy.

Before normal forward planning, the executor restores a missing baseline share by using the existing planner in reverse: the baseline supplies replacement capacity and canary revisions may drain. Both directions enforce readiness, availability, and surge checks. For zero-surge rollouts, this can spend configured unavailability to make room; with surge, restoration can grow before it drains.

The oldest revision with surviving groups is the baseline. Before the first partial scaling step, the controller records the choice on its LWS objects using the internal boolean annotation `disaggregatedset.x-k8s.io/canary-stable`. The recorded baseline is reused across reconciles, later canary templates, and controller restarts; B → C must not transfer the baseline from A to B. The marker disappears with A's LWS objects when A is fully promoted away.

The baseline's layout is validated after selection: the controller must not silently choose a younger compatible canary when the oldest survivor is incompatible. With no recorded baseline, equally old surviving revisions make the choice ambiguous; conflicting markers also report `CanaryBlocked`. Set `100` to finish the current rollout before starting another canary rather than guessing a baseline.

The baseline receives restored capacity with its historical templates, rollout budgets, and frozen initial-replica annotations unchanged. This ordering rule does not determine that it is the last known-good release; application analysis remains external. Loading groups count as requested capacity. Serving readiness is calculated per complete revision before aggregation: a Prefill in one revision and a Decode in another do not form a serving revision.

An interrupted canary keeps the baseline separate from revisions being tested. If A and B are held at 50% and the templates change to C at the same percentage, A keeps its 50% share and B rolls to C within the remaining share. For four Prefill / eight Decode, this changes `A=2/4, B=2/4, C=0/0` into `A=2/4, B=0/0, C=2/4`. With demand and percentage unchanged, this replacement does not scale or recreate A's LWS or Pods. A and B are not interchangeable parts of an aggregate old quota.

During B → C, the canary share temporarily contains both B and C. The planner coordinates their replacement using budgets calculated from full workload demand; baseline A's serving readiness contributes to the availability floor. Temporary excess is bounded by surge, and B only drains when the remaining serving capacity permits it. The hold becomes settled once the latest canary C supplies the whole requested canary share.

**During an active canary hold, the `Deferred` / `Immediate` settings for `scaleUpDuringRollout` and `scaleDownDuringRollout` are ignored: both revisions resize to preserve the requested percentage.** Started canary groups may therefore be removed when demand or the percentage decreases, even with `Deferred`. Readiness, availability, and surge checks still apply. An intentionally held rollout might never finish, so deferring these changes until completion would prevent proportional resizing. The configured scaling policies resume at `canaryPercent: 100` or when the field is removed; rollouts without an active percentage hold retain their existing behavior.

While held, coordination uses today's desired shares. After Prefill grows from four to twenty while Decode stays at eight, judging Prefill against its original four groups can prevent Decode from reaching its requested share. Using current demand keeps both roles progressing toward the same percentage. The usual readiness checks still limit every deletion. Promotion to 100 releases the baseline hold and completes the rollout to the current templates.

### Rollback and explicit limitations

Lowering the percentage restores the old share and reduces the new share while compatible old history remains. For four Prefill and eight Decode groups, `50 → 25 → 0` changes the settled new counts from `2/4` to `1/2` to `0/0`. At zero, actual target LWS objects and their intended-replica annotations remain, with `spec.replicas: 0`; a subsequent promotion can grow them again.

> **Zero percent is not a durable rollback.** The desired templates still describe the new revision. Raising the percentage or removing the field can deploy those templates again. **Restore the stable templates in Git** to make the rollback durable. If they match the surviving baseline, that baseline becomes the target and the rollout converges fully to it without a percentage hold. Resetting `canaryPercent` to `100` in the same change is recommended to avoid retaining a hold for later releases and to allow an ordinary rollout when the former baseline has gone. Normal rollout safety checks apply.

- A positive one-group logical role cannot be split while keeping both revisions within desired. Report `CanaryBlocked` until every affected role has at least two groups or the percentage reaches 100. Scaling a held role from two to one can therefore retain excess groups until one of those actions is taken. Zero percent can hold a singleton entirely old.
- A fresh `External` role starts at one until its RoleScaler receives a scaling request. Initialize each active role or sub-role to at least two before requesting a positive partial canary.
- Replenishment requires the recorded baseline to have the same active logical-role layout and surviving groups for every active role. A younger compatible revision does not replace an incompatible baseline. Renames, ordinary-role/sub-role conversion, and positive↔zero leaf changes are not mapped onto history. In particular, scaling one required role or sub-role to zero while peers remain positive reports `CanaryBlocked`; finish the layout change at 100. Scaling the whole set to zero remains allowed.
- Percentage changes alone never recreate fully drained history. A completed rollout is not rewound by adding or lowering the percentage; without surviving old groups, the target converges normally. Restoring a surviving baseline's templates is different: that revision becomes the target, the hold is bypassed, and the set converges fully to it.
- Integer rounding can make actual shares differ significantly for small roles: 99% of two groups remains one old plus one new until promotion to 100.

### Status and progressive-delivery tools

A settled hold reports current-generation `Progressing=True`, reason `CanaryHeld`, with a message containing the requested percentage and observed new/desired role counts. `MinimumReplicasAvailable` remains the independent availability signal. Completion remains `Available=True`, reason `AllRolesReady`. `CanaryBlocked` explains why the split cannot currently be achieved and what action is required.

Argo CD can recognize these conditions through a custom health check; Git-driven promotion only changes the top-level percentage field. Kargo can automate this workflow by writing and syncing successive percentages in Git, then verifying the requested release and hold. Alternatively, an external controller can own the live percentage while Argo CD owns templates, with explicit field ownership so sync does not overwrite promotion. This does not make DisaggregatedSet a native Argo Rollouts `workloadRef`: an integration must bridge the APIs. Traffic routing and analysis metrics are separate concerns; Ready groups alone do not prove that an engine passed analysis.

Verification of `External` roles must use each current RoleScaler's `spec.replicas`. A RoleScaler update does not change the DisaggregatedSet generation; `observedGeneration` alone therefore cannot establish that status includes the latest desired counts.

A companion documentation PR will provide the complete health and optional manual-action configuration for stock Argo CD, along with field-ownership rules. No Argo CD source patch is required.

## Implementation

- Validate and preserve the new percentage field in the API and CRD. Per-role LWS partition rejection remains unchanged.
- Derive internal `CanaryOldReplicas` limits in the existing executor; do not send a partition to child LWS resources.
- Bound target growth and old drain by the hold; resize both revisions during a canary independently of ordinary Deferred/Immediate policy.
- Reuse the planner in reverse to restore baseline capacity and safely reduce the canary share; calculate shares from current demand and serving readiness per complete revision.
- Keep the baseline separate when canary templates change again: replace B with C within the canary share without consuming A's retained groups.
- Record the baseline on its LWS objects before partial scaling; reuse the marker across restarts and block ambiguous or conflicting history.
- Bypass the percentage hold when the surviving baseline is itself the desired target, allowing a Git template rollback to converge fully.
- Preserve target LWS resources and intended counts at zero percent for later promotion.
- Report a hold only after all rolling slices settle and all groups are Ready. Reuse the existing planner, observer, executor, and sub-role assignment logic.

## Test plan

- Percentage rounding, initial creation, unchanged revision hashing, zero/100 controls, forward and reverse changes, and no revival of fully drained history.
- Held progress and completion with different surge/unavailability budgets; validate every simulated plan and availability floor.
- Ordinal/Hash and Deferred/Immediate growth/downscale; asymmetric Prefill growth and downscale followed by full promotion.
- Real observer and assignment paths for Ordinal/Hash virtual roles, preserving old annotations and checking actual group labels.
- A → B → C at a fixed percentage, unchanged baseline group/LWS/Pod identities, multiple canary revisions, complementary incomplete revisions, singleton and layout restrictions, settled status, and every-slice readiness.
- API-server/webhook persistence and range validation; unchanged rejection of unsupported per-role partitions.
- Git → Argo CD promotion and external automation comparisons, including analysis failure, reversal, and resync while held.

## Alternatives

Per-role integer partitions, proposed in [PR #1265](https://github.com/kubernetes-sigs/lws/pull/1265), reuse the familiar StatefulSet/LWS count. They let roles choose different fixed holds, but promotion tools must calculate and track each count as demand changes, and virtual roles need their own treatment. This proposal adapts the bounded-hold arithmetic without depending on that API or its commits. Only the percentage field is exposed by this alternative.

## Implementation history

- 2026-11-02: work-in-progress independent proposal for comparing set-wide percentage canaries with per-role integer partitions.
- 2026-11-06: make active canaries reversible and resize both revisions proportionally under either scaling policy; document durable rollback and stock Argo CD ownership workflows.
- 2026-11-06: retain the baseline when a held canary receives another template update; roll B to C within the canary share.
