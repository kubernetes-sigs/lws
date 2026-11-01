# KEP-1264: DisaggregatedSet Rollout Partition

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Manual canary](#story-1-manual-canary)
    - [Story 2: Progressive delivery controller](#story-2-progressive-delivery-controller)
    - [Story 3: GitOps](#story-3-gitops)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
  - [Semantics](#semantics)
  - [Planner](#planner)
  - [Propagation to LeaderWorkerSets](#propagation-to-leaderworkersets)
  - [Validation](#validation)
  - [Observability](#observability)
  - [Integration with Argo CD](#integration-with-argo-cd)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

Let a DisaggregatedSet rollout stop part of the way through. Each role's
`spec.rolloutStrategy.rollingUpdateConfiguration.partition`, which the webhook
rejects today, holds that many of the role's groups on older revisions. The
rollout advances until each role has `replicas - partition` groups on the new
revision, then waits. Lowering the partition advances it; zero completes it.

This is the StatefulSet and LeaderWorkerSet meaning of `partition`, applied to
the DisaggregatedSet's revision-per-LWS rollout. No new API field is added.

## Motivation

Rolling a new engine (image, flags, config) into a production disaggregated
inference service is risky: correctness, KV transfer between prefill and decode,
prefix-cache behavior and performance all need checking on real traffic before
the old revision is gone. The usual answer is a canary: put a few groups of the
new revision into service, check them, then continue or roll back.

A DisaggregatedSet cannot do this today. Once the template changes, the
controller rolls every role to completion; there is no point at which it holds
"one new prefill group and one new decode group" for a decision. KEP-766 lists
partition as unsupported, so the field LWS and StatefulSet users would reach for
is rejected.

The workarounds we run today (llm-d, GLM-5.3 production) show the cost:

- Two DisaggregatedSets as "stable" and "canary" slots. A script scales groups
  from one to the other and swaps the slots at the end. The router has to treat
  two objects as one service, every rollout needs a surge of a full group per
  role, and the engine template is duplicated.
- The script owns the sets' replicas and images at runtime, which conflicts with
  GitOps: Argo CD must ignore those fields, and because they sit inside the
  `spec.roles` list its `RespectIgnoreDifferences` merge replaces the whole list,
  silently dropping every other template change from git.
- Progressive-delivery tools (Argo Rollouts) cannot drive a DisaggregatedSet at
  all, so the steps run as Jobs patching objects behind the controller's back.

With a partition, one DisaggregatedSet holds both revisions, the controller keeps
its coordination and availability guarantees at every step, and an external
controller or operator only changes one integer per role.

### Goals

- Hold a rollout with a chosen number of groups per role on the new revision,
  indefinitely, at full desired capacity.
- Advance by lowering the partition; complete at zero; keep every existing
  rollout guarantee (cross-role coordination, maxSurge, maxUnavailable,
  revision completeness) at every step.
- No new API field; no change for objects without a partition.

### Non-Goals

- Rolling back by raising the partition. As with StatefulSets, a partition stops
  progress; it never moves groups back. Rollback is a template change back to
  the old revision, which reuses its LWS objects.
- A per-revision traffic split. Routing by revision stays the router's job
  (llm-d's EPP already splits by the number of groups per revision).
- Partition on roles with `subRoles` (rejected by the webhook; can follow later).
- Automatic analysis or promotion. That belongs to the tool driving the
  partition.

## Proposal

Accept `partition` on each role's rolling update configuration. During a
rollout the controller keeps up to `partition` of the role's groups on older
revisions and grows the new revision only to `replicas - partition`.

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
spec:
  roles:
  - name: prefill
    spec:
      replicas: 4
      rolloutStrategy:
        rollingUpdateConfiguration: {maxSurge: 1, maxUnavailable: 0, partition: 3}
  - name: decode
    spec:
      replicas: 8
      rolloutStrategy:
        rollingUpdateConfiguration: {maxSurge: 1, maxUnavailable: 0, partition: 7}
```

After the template changes, this settles at 1 new and 3 old prefill groups,
and 1 new and 7 old decode groups, and stays there. Setting the partitions to
`2` and `4` moves to 2/2 and 4/4; setting both to `0` completes the rollout.

### User Stories

#### Story 1: Manual canary

An operator sets the partitions to `replicas - 1`, then changes the image. One
new group per role comes up and takes its share of traffic. They check the
canary, then lower the partitions in steps, or revert the image to roll back.

#### Story 2: Progressive delivery controller

An Argo Rollouts (or Flagger, or in-house) flow runs: set partition to N-1, wait
for `status.roleStatuses[].updatedReplicas`, run analysis, set partition to N-2,
and so on. The flow only writes an integer per role; the DisaggregatedSet
controller decides which groups to add and remove and keeps capacity.

#### Story 3: GitOps

Git holds the whole DisaggregatedSet. A rollout is a commit that changes the
template; promotion lowers the partitions, by commit or by a controller that
owns only that field. See [Integration with Argo CD](#integration-with-argo-cd).

### Notes/Constraints/Caveats

- Partitions should be set consistently across roles. A revision with prefill
  groups but no decode groups serves nothing; setting `partition >= replicas`
  on one role while others advance holds that role entirely on the old
  revision, which may leave the new revision incomplete. The controller still
  keeps both revisions complete (a held old revision keeps every role it has).
- A partition set before the template changes has no effect until a rollout
  starts, which is the usual way to use it.
- The partition applies to whichever rollout is in progress, including a
  rollback. Reverting the template during a canary makes the old revision the
  target again, and a non-zero partition then holds canary groups. A rollback
  normally sets the partition to 0 in the same update.
- The hold is reported once every role has reached its partition target and
  is Ready. While a rollout with a non-zero partition is still moving toward
  that point, or is blocked by another bound (for example readiness), the
  condition stays `RolloutInProgress`.
- Scaling (`replicas` or an external scaler) during a hold uses the current
  desired count: the target may have `replicas - partition` groups.

### Risks and Mitigations

- **A forgotten partition leaves a rollout half done.** The controller records a
  `RollingUpdatePartitioned` event while it holds, and the `Progressing`
  condition stays set, so tooling that waits for completion sees it.
- **A held rollout uses both revisions' capacity.** It uses no more than the
  configured surge: the target grows only as the old revision drains, within
  `maxSurge` and `maxUnavailable`.

## Design Details

### API

No new field. `partition` already exists on the embedded LWS
`RollingUpdateConfiguration` (`*int32`, default 0). The change is in validation
and in what the DisaggregatedSet controller does with it. The rollout strategy
is excluded from the revision hash already, so changing the partition does not
start a new revision.

### Semantics

For each role with `partition = p > 0` and desired replicas `d`:

- The target revision grows to at most `max(0, d - p)` groups. Raising the
  partition does not remove groups it already has, except excess above `d`
  under `Immediate` (below).
- Older revisions together keep up to `min(p, d - t)` groups, as far as they
  still have them, where `t` is the target's groups. The active old revision
  keeps what parked revisions do not already provide. A drained group is not
  recreated. Capping the floor at `d - t` keeps the role at `d`: when the
  partition rises mid-surge (3 replicas, 2 old + 2 new, partition 1 to 2) the
  old revision drains to 1, through the normal drain budget (under
  `Immediate`, the target gives up the excess group instead).
- When `d` drops during a hold (an autoscaler, or an edit of `replicas`):
  - with `scaleDownDuringRollout: Deferred` (the default) the target revision
    is not reduced, as for any rollout, and old groups drain to `d - t`;
  - with `Immediate`, excess comes from the target first, down to
    `max(d - p, d - old)` groups (at least one for a started required role),
    as a StatefulSet removes its newest Pods first; the old revision keeps its
    held groups. Replicas 3 to 2 with partition 1 (1 old + 2 new) ends at
    1 old + 1 new instead of 0 old + 2 new.
- The partition is a growth ceiling, not a smaller phase: the fractional
  coordination window still measures each role against its whole desired
  count. With partitions 1 and 9 on two roles of 10, the second role stops at
  1 new group and the first stops within one window of it (2), not at 9.
- Everything else is unchanged: maxSurge/maxUnavailable bound each step,
  revisions retire coherently, and bootstrap surge or availability fallback
  apply only within the partition's bounds.

`p = 0` or unset keeps today's behavior exactly.

### Planner

The planner gains two bounds, both applied where the existing bounds are, so
the executor's step validation (which recomputes the plan) agrees by
construction:

- `furthestNewTargets` caps each role's growth at `d - p` (never below the
  target's current Spec), after the growth limits and before the coordination
  window, which still uses the uncapped phase targets. Bootstrap surge does not
  treat a role held by its partition as blocked, and the step validator checks
  the same ceiling.
- `furthestOldTargets` raises each role's old-revision target to the
  partition floor before the coordination window and revision-completeness
  bounds, so ordinary drains and the availability fallback both stop at it.
- Under `scaleDownDuringRollout: Immediate`, `furthestTargetScaleDownTargets`
  and its availability bounds let the target shrink to the groups the
  partition leaves it.

When no bound leaves a change, the planner returns no step and the executor
waits without recording `RevisionDrainBlocked`. It reports the slice held only
when, from its own observations, every role has exactly `d` groups across all
revisions and every group is Ready; a slice whose groups are still starting is
waiting, not held.

### Propagation to LeaderWorkerSets

Each LWS holds a single revision, so a role partition has no meaning there.
The controller strips it when creating an LWS and when syncing the rollout
strategy, leaving any LWS-level partition the controller manages unchanged.

### Validation

- `partition` must be `>= 0`.
- `partition > 0` is rejected on a role with `subRoles`.
- `partition` may exceed `replicas` (holds every group, as with StatefulSets).

### Observability

- One `RollingUpdatePartitioned` event each time the rollout reaches a hold
  (when the condition below changes to `RolloutPartitioned`), not one per
  reconcile while it lasts.
- `status.roleStatuses[].updatedReplicas` reports the new revision's groups per
  role, which is what a driver waits on between steps.
- The `Progressing` condition carries reason `RolloutPartitioned` (instead of
  `RolloutInProgress`) once every slice still rolling is held, as above, and
  the aggregated `status.roleStatuses` agree (every role at `d` groups, all
  Ready, some not yet updated), so a held rollout is distinguishable from one
  still moving. A slice that is pending or failed in the reconcile prevents it.
  Its message lists each role's groups on the current revision, for example
  `Rolling update held at the role partitions; groups on the current revision:
  prefill 1/3, decode 1/3`.

### Integration with Argo CD

Today a canary needs runtime-owned fields inside `spec.roles` (replicas and
images moved between two sets by a script). Argo CD must ignore them, and its
`RespectIgnoreDifferences` sync replaces the whole `roles` list with the live
one (a JSON merge patch cannot merge into list elements), so every other
template change from git is silently dropped. With a partition, nothing in the
template is runtime-owned, and the set can be synced from git as is.

Two ways to drive a rollout:

**1. Git drives every step.** Git holds the template and the partitions.

| Commit | Effect after sync |
|---|---|
| Change the template, set each role's `partition` to `replicas - 1` (same commit) | New revision with one group per role, held. Health: Healthy, "held at the role partitions" |
| Lower the partitions | The rollout advances to the new targets and holds again |
| Set the partitions to 0 | The rollout completes. Health: Healthy |
| Rollback: revert the commits | Old template with partition 0; the controller rolls back to the old revision, reusing its LWS objects |

No `ignoreDifferences` or special sync options are needed, and auto-sync with
self-heal is safe. The template change and the first partition must land in
one commit: a template synced with partition 0 rolls to completion. A team can
instead keep a standing partition in git (for example `replicas - 1`), which has
no effect between rollouts and makes every template change stop at one group
until someone lowers it; it is raised again after the rollout completes.

**2. A controller drives promotion.** Git holds the template; a promotion
controller (an Argo Rollouts flow, a Job run by an Argo CD sync hook, or an
operator with `kubectl`) owns the partitions.

- Enable server-side apply for the Application (`ServerSideApply=true`, with
  server-side diff) and leave `partition` out of git. `roles` is a map-list
  keyed by name, so the API server tracks each role's `partition` as a field
  owned by the promotion controller's field manager, and Argo CD's applies
  neither reset it nor report it as drift.
- Do not use `ignoreDifferences` with `RespectIgnoreDifferences` for
  `partition`: it hits the list-replacement problem above.
- Argo Rollouts cannot use a DisaggregatedSet as its workload (`workloadRef`
  supports Deployments). A Rollout can still drive one: its canary steps are
  step-plugin or analysis Job steps that set the partitions and run checks, and
  its pauses are the manual gates. The Rollout's own pods are not used.

**Autoscaled roles.** Roles with `scaling.mode: External` omit `replicas` from
the DisaggregatedSet, and the controller creates the
DisaggregatedSetRoleScaler, so Argo CD manages neither the replica count nor the
scaler. An integer partition holds a fixed number of old groups, so a driver
recomputes it when the target changes. Scale-down during a hold returns the
role to `d` under either scaling policy (see Semantics); scale-up still puts
all growth on the target revision. Proportional scale-up and percentage
partitions are left to a follow-up.

**Health.** Argo CD has no built-in health check for DisaggregatedSets. A
custom check in `argocd-cm` maps the conditions:

```yaml
resource.customizations.health.disaggregatedset.x-k8s.io_DisaggregatedSet: |
  hs = {status = "Progressing", message = "Waiting for status"}
  if obj.status ~= nil and obj.status.conditions ~= nil then
    for _, c in ipairs(obj.status.conditions) do
      if c.status == "True" and c.observedGeneration == obj.metadata.generation then
        if c.type == "Available" or (c.type == "Progressing" and c.reason == "RolloutPartitioned") then
          hs.status = "Healthy"
        elseif c.type == "Progressing" then
          hs.status = "Progressing"
        end
        hs.message = c.message
      end
    end
  end
  return hs
```

A held rollout is Healthy, with the hold in the message: it is the state the
partitions ask for, and Argo CD's built-in StatefulSet check treats a
partitioned rollout that reached its partition the same way. Reporting it as
Progressing would keep a git-driven Application from settling between steps.
When an Argo Rollouts Rollout drives the partitions, the Application shows
Suspended at each pause through the Rollout itself.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

#### Unit tests

- `pkg/controllers/disaggregatedset`: the planner holds at each partition and
  completes at zero, for symmetric and asymmetric role sizes and several
  surge/unavailable budgets, checking every step with the executor's validator
  and the availability floor; a partition at or above replicas starts nothing;
  raising a partition does not retract target groups and, mid-surge, drains
  the old revision back to desired; scale-down during a hold under both
  scaling policies; the coordination window still applies between roles with
  different partitions; parked revisions count toward the held groups; a slice
  is reported held only with every group issued and Ready, and the set only
  when every rolling slice is held; the partition is read from the role spec and is not
  propagated to the LWS on create or sync.
- `pkg/webhooks/disaggregatedset`: partition accepted, including above replicas;
  negative rejected; rejected with subRoles.

#### Integration tests

- `test/integration/webhooks`: LWS templates with a valid partition are no
  longer expected to be rejected as DisaggregatedSet role templates.

#### e2e tests

- `test/e2e/disaggregatedset`: a 3+3 set with partition 2 holds at 1+1 new
  groups after an image change, then at 2+2 with partition 1, and completes at
  partition 0.

### Graduation Criteria

Alpha with the change: the field was rejected, so no existing object uses it.
A downgrade to a controller without this change would roll held sets to
completion and reject further updates that keep a non-zero partition; set
partitions to 0 before downgrading.

## Implementation History

- 2026-10-30: KEP and implementation proposed.
- 2026-10-31: `RolloutPartitioned` condition reason implemented; the hold event
  is recorded once per hold. Tested on GKE (B200) with a P/D workload under
  load, driven by hand and by Argo CD with an Argo Rollouts Rollout.
- 2026-11-01: review fixes: the old-revision floor is capped at `d - t`;
  Immediate scale-down takes excess from the target first; the partition no
  longer changes the coordination window; a hold is reported per slice, only
  once every group is issued and Ready.

## Drawbacks

Per-role partitions need to be set consistently by the user or the driving
tool. A single set-level value would be simpler for symmetric P/D, but cannot
express different step sizes for differently sized roles.

## Alternatives

- **A set-level `spec.rolloutStrategy.partition` (one number or a fraction).**
  Easier for symmetric roles and outside the `roles` list, but a new field, and
  a single count is wrong for roles of different sizes; a fraction needs
  rounding rules per role.
- **`spec.paused`, as on Deployments.** Stops wherever the rollout happens to
  be; does not give a precise, reproducible canary size. Could be added later
  for emergencies.
- **Two DisaggregatedSets as slots, driven externally.** What users do today; see
  Motivation for why it is costly.
- **Use the LWS partition within one LWS per role (KEP-766 Alternative 4).**
  Rejected by KEP-766 for revision-aware routing; this KEP keeps the
  LWS-per-revision design and only bounds how far it goes.
