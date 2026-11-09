---
title: "Role Scaling and Autoscaling"
linkTitle: "Scaling and Autoscaling"
weight: 40
aliases:
  - /docs/concepts/disaggregatedset/scaling-during-rollout/
description: >
  Set replica targets for individual roles, connect an autoscaler, and control scaling during a rollout.
---

Roles scale independently: for example, prefill can grow in response to request queues
while decode responds to active batches and KV-cache usage. A replica is a **group**
of Pods, not necessarily one Pod. All Pods in a group must be Ready for that group to
count as Ready.

## How It Works

Each role has a desired replica count. The source of that count depends on its scaling mode:

| Mode | Target source | Write to |
| --- | --- | --- |
| `Static` (default) | Your manifest | `roles[].spec.replicas`, per slice |
| `External` | HPA, KEDA, or a custom scaler | The RoleScaler's `/scale` endpoint |

**DisaggregatedSetRoleScaler (DSRS)** gives an external autoscaler a stable target.
When you enable `scaling.mode: External`, the DisaggregatedSet controller creates
and maintains a DSRS named `<disaggregatedset-name>-<role-name>`. You do not create
this resource yourself.

{{< ds-role-scaler >}}

The autoscaler writes the desired count to the DSRS. The DisaggregatedSet controller
then manages the child LeaderWorkerSets needed to reach it, including rollout and
drain. The DSRS name stays the same while child names change with each revision, so
the autoscaler does not need to be retargeted after a pod-template update.

## Scaling during a rollout

A rolling update replaces revision A with B. Demand can change while both are running:
changing the replica target adjusts capacity without creating another revision.
The same scaling policies apply to **Static and External roles**, with either
Ordinal or Hash group identities.

### Choose a scaling policy

Scale-up and scale-down have separate settings under `spec.scalingPolicy`. Both default to `Deferred`:

| Setting | `Deferred` (default) | `Immediate` |
| --- | --- | --- |
| `scaleUpDuringRollout` | Grow B in normal rollout batches, waiting for readiness when needed. | Request enough B groups to fill the desired count after subtracting groups still requested in A. |
| `scaleDownDuringRollout` | Keep requested B groups until old revisions drain. A can still retire. | Reduce safely now, preferring A before B. |

To enable both Immediate behaviors:

```yaml
spec:
  scalingPolicy:
    scaleUpDuringRollout: Immediate
    scaleDownDuringRollout: Immediate
```

You can enable either independently. Choosing Immediate scale-up does not enable Immediate scale-down.

The two illustrations below each use roughly 44 seconds of playback for readability.
Their recorded timestamps are unchanged, but they do not share a real-time playback scale.

### When demand increases

Only B grows; A is never scaled back up. The scale-up policy determines whether new groups must wait behind a batch that is still starting.

For example, a role has **4 Ready groups in A** and **1 loading group in B**. Desired replicas increase from **4 to 10**, with `maxSurge: 1` and `maxUnavailable: 0`:

- **Deferred** keeps B at 1 for now: the existing group must become Ready before more groups are requested.
- **Immediate** grows B to **6 = 10 desired − 4 in A**, without waiting for the loading group. A keeps its four Ready groups.

Immediate counts requested groups, including those still loading, across **all** old revisions. A readiness drop alone does not request more groups. If old groups later retire safely, B can fill the newly available gap even without another replica-count change.

This extra growth reaches the desired count; it does not raise `maxSurge` or permit extra old-group deletions. Normal rollout growth may still use the allowed surge, and renamed-role safeguards still apply. Immediate can start more groups sooner, but cannot make them schedulable or Ready sooner.

{{< canvas-animation data="/animations/scaling-up.json" title="Deferred versus Immediate scale-up from 4 to 10 groups" >}}
Two recorded Kubernetes runs show how each policy grows B after demand rises from 4 to 10 groups per role. Model-loading readiness is controlled, and playback is compressed.
{{< /canvas-animation >}}

### When demand decreases

The scale-down policy controls reductions independently of the scale-up policy. **Deferred** waits until old groups are gone before reducing B, which can temporarily keep more resources in use. **Immediate** tries to free resources sooner, removing old groups first; it can also remove excess B groups when safe.

The animation below reduces demand from **37 to 20 Prefill groups** and **19 to 10 Decode groups** while B is still loading. It compares the two **scale-down** policies using autoscaler requests, with both rollout budgets set to 10%. Scale-up remains Deferred in both examples.

{{< canvas-animation data="/animations/scaling-down.json" title="Deferred versus Immediate scale-down during a rollout" >}}
Two recorded Kubernetes runs with controlled model-loading readiness. Panels are aligned at the replica change; each shows its own observed state. Playback is compressed.
{{< /canvas-animation >}}

**Immediate does not mean instant removal.** Both scale-down policies follow your rollout budgets, so extra groups may need to stay while replacements become Ready. Freeing resources earlier does not necessarily make the whole rollout finish sooner.

During scale-down, the difference depends on how far the update has progressed; sometimes both policies can make the same changes. See [policy details and manual scaling](../#scaling-during-a-rollout) for more.

## Configuration Example

### 1. Enable External Scaling on the Role

In your `DisaggregatedSet` manifest, set `scaling.mode: External` on the role(s) you wish to autoscale:

{{< include file="examples/disaggregatedset/role-scaler/external-scaling.yaml" lang="yaml" >}}

### 2. Create an HPA Targeting the Role Scaler

Target the automatically created `DisaggregatedSetRoleScaler` by name (`<ds-name>-<role-name>`):

{{< include file="examples/disaggregatedset/role-scaler/hpa.yaml" lang="yaml" >}}

### 3. (Optional) KEDA ScaledObject Example

For event-driven autoscaling based on Prometheus metrics (such as vLLM queue depth or TTFT):

{{< include file="examples/disaggregatedset/role-scaler/keda-scaledobject.yaml" lang="yaml" >}}

## Observe capacity

The DisaggregatedSet reports each role's target in `status.roleStatuses[].desiredReplicas`,
alongside `replicas`, `readyReplicas`, and `updatedReplicas`. For an External role,
`desiredReplicas` is unset until its scaler exists.

`Available` is False until a scale-up completes. To tell whether the role still has
enough serving capacity while new groups start, read `MinimumReplicasAvailable`, or
compare `availableReplicas` with `minAvailableReplicas` in the role's status. See
[Watch availability during a rollout](../rollout-strategy/#watch-availability-during-a-rollout).

## External scaling constraints

- **One slice:** External role scaling requires `spec.slices: 1`, or omission of
  `spec.slices` (which defaults to 1). Admission rejects `slices > 1` when any role
  uses `scaling.mode: External`.
- **Rollout budgets:** Percentages in the role's `rolloutStrategy.rollingUpdateConfiguration`
  resolve against the scaler's target. Because that target is unknown at admission,
  validation rejects `maxSurge` and `maxUnavailable` only when both are zero at every
  size, such as `0` and `0%`.
- **Generated names:** With the default `groupIdentity`, the DisaggregatedSet and
  External role names can be at most 31 characters combined. For example, a
  24-character DisaggregatedSet name leaves room for the role `prefill`.

The name limit reserves space for an 8-digit group index, the same space a
`groupIdentity: Hash` role reserves for its group key. An External role at that
limit can reach 100 million groups; beyond that, new groups' Pods cannot be created
because their `controller-revision-hash` label would be too long.

Existing External roles with the default `groupIdentity` that were admitted under
the earlier name limit retain that limit on updates. Admission warns when their
names cannot hold an 8-digit group index. Deleting and recreating the DisaggregatedSet
applies the 31-character limit, so shorten longer names before recreating it.
