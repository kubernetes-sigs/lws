# KEP-948: DisaggregatedSet External Scaling for Multi-Slice Sets

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Aggregate Scope and Distribution](#aggregate-scope-and-distribution)
  - [Per-Slice Resolution and the No-Shrink Guard](#per-slice-resolution-and-the-no-shrink-guard)
  - [Seeding](#seeding)
  - [Changing the Slice Count](#changing-the-slice-count)
  - [Example: Scaling Under Uneven Slice Load](#example-scaling-under-uneven-slice-load)
  - [Validation Changes](#validation-changes)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Per-slice scaler CRs](#alternative-1-per-slice-scaler-crs)
  - [Alternative 2: Per-role value applied per slice](#alternative-2-per-role-value-applied-per-slice)
  - [Alternative 3: Enforce a minimum of one replica per slice](#alternative-3-enforce-a-minimum-of-one-replica-per-slice)
<!-- /toc -->

## Summary

[KEP-849](/keps/849-DisaggregatedSet-HPA) added per-role external scaling through the auto-created `DisaggregatedSetRoleScaler` CRD, but restricted alpha to single-slice sets: the webhook rejects `spec.slices > 1` while any role has `scaling.mode: External`. This KEP lifts that restriction by defining the multi-slice semantics KEP-849 deferred: `scaler.spec.replicas` is an aggregate count across slices that the controller splits deterministically, scaler status keeps its existing aggregate shape, and the mid-rollout no-shrink guard is evaluated per slice. One scaler and one HPA per role serve the whole set regardless of slice count. No API fields are added or changed.

## Motivation

Slices ([KEP-846](/keps/846-disaggregatedset-slices)) map complete role topologies onto accelerator domains, and those are deployments that want to autoscale roles like prefill against traffic. Today the two features are mutually exclusive. KEP-849 deferred three questions rather than prejudge them:

1. What does `scaler.spec.replicas` mean with N slices?
2. What should `status.selector` match?
3. How does the no-shrink guard behave when slices roll on independent clocks?

The merged implementation is already most of the way to an aggregate model: one shared scaler map feeds every slice's reconcile, and scaler status is already summed across slices with a slice-agnostic selector. What is missing is a defined split of the desired count and a per-slice guard.

### Goals

- Allow `spec.slices > 1` together with External roles, with a deterministic distribution of the scaler total among slices.
- Keep HPA's ratio math consistent: the pods the selector matches, the status count, and the written value all use the same unit, LWS groups across the whole set.
- Keep the no-shrink guard correct under concurrent per-slice rollouts.

### Non-Goals

1. Per-slice scalers or per-slice HPAs (see Alternatives).
2. Autoscaling the slice count (KEP-849, Alternative 2).
3. Coupling HPA writes to the old-revision drain schedule (future work per KEP-849).

## Proposal

Keep the scaler API unchanged. The controller interprets `scaler.spec.replicas` as the role's total across slices, splits it with a fixed function before the per-slice reconcile loop, and feeds each slice its share as an ordinary replica target. Observed state flows back the way it already does. The webhook rejection is removed.

### Risks and Mitigations

- **Zero-replica slices.** With a total below the slice count, some slices run none of the role. Documented, with the recommendation to set HPA `minReplicas >= slices`. Hard-enforcing a floor would break the `/scale` contract (see Alternative 3).
- **Slice-count changes rebalance rather than multiply.** The autoscaler owns the total for External roles, so raising `slices` spreads the same total over more slices, while Static roles multiply. Documented.
- **Transient overshoot.** A mid-rollout slice holds its guard floor while steady slices shed, so the realized total can briefly exceed the written value. Bounded by in-flight counts, converges at rollout completion, and is the single-slice alpha behavior applied per slice.
- **Sticky routing.** Aggregate scope assumes slice-blind load balancing spreads load onto new capacity wherever it lands (see the worked example).

## Design Details

### Aggregate Scope and Distribution

`scaler.spec.replicas` is the desired total of LWS groups summed over all slices. Inline `spec.roles[].spec.replicas` stays a per-slice count for Static roles: the units differ because autoscalers reason about total desired capacity while inline replicas describe the shape of one slice. The controller splits the total `R` over `S` slices:

```
distribute(R, S, i) = floor(R / S) + 1  if i < R mod S
                      floor(R / S)      otherwise
```

Properties: stateless (a pure function of `(R, S, i)`, so concurrently reconciling slices always agree), balanced (shares differ by at most one), and monotone (raising `R` never lowers any slice's share). The remainder sits on the lowest indices so slice scale-down, which removes the highest slices first per KEP-846, disturbs the smallest shares.

`status.replicas` and `status.selector` are unchanged. They already aggregate across all slices and revisions, which is the same shape under aggregate scope: HPA's ratio math stays consistent for the same reason it already does across revision mixes during a rollout.

### Per-Slice Resolution and the No-Shrink Guard

Replica resolution moves from the executor into the controller. Before the per-slice loop, every role's target is resolved per slice (`distribute()` for External roles, the inline per-slice count for Static roles), and the executor consumes only resolved targets. Lifting the restriction without this step would apply the total once per slice, N x slices pods.

The no-shrink guard keeps its KEP-849 purpose, evaluated per slice: a slice with old revisions still draining floors its own distributed share at its own in-flight new-revision count. Each slice reconciles on its own state, so one slice's rollout can neither clamp nor distort a sibling. Percentage rollout budgets (`maxSurge`, `maxUnavailable`) compute against the slice's resolved target.

### Seeding

- Fresh External role: seed `spec.replicas` to the slice count, one group per slice, so every slice serves from the start and vanilla HPA can bootstrap via `minReplicas`.
- Static to External flip: seed to the observed total across slices and revisions, so the flip does not resize a running fleet.

### Changing the Slice Count

- Scale up: the same total is redistributed over more slices on the next reconcile.
- Scale down: the highest slices are deleted (KEP-846 semantics) and the remaining slices scale up to absorb the total without waiting on the autoscaler.

### Example: Scaling Under Uneven Slice Load

With `slices: 3`, an External prefill role at 5 (2/2/1) and a Static decode role at 5 per slice, suppose slice 1's prefill runs hot. HPA sees the fleet-wide average cross its target and writes 6, distributed 2/2/2: the new group lands on slice 2, not the hot slice. This is usually fine. Routing (the llm-d EPP, or any slice-blind balancer) treats the role as one flat pool, so the balancer spreads the same traffic over 6 groups and slice 1's pressure drains through load balancing rather than placement.

The caveat is intentionally sticky routing. Prefix cache aware routing keeps sending prefix-sharing requests to the pods with the warm cache, which is the slice that is already hot, so the new group elsewhere absorbs little and the hotspot can persist while the fleet average drops back under target. Fixing that through placement would require per-slice scaling (Alternative 1). The cleaner mitigation lives in the router, which should cap prefix affinity when its target saturates.

### Validation Changes

Remove the webhook rejection of `spec.slices > 1` while any role is External. The warning that inline replicas are ignored for External roles stays.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Unit tests

- `distribute()` properties: balance, monotonicity, `R < S`, `R = 0`.
- Per-slice resolution mixing Static and External roles, per-slice guard clamping, and seed computation.

#### Integration tests

- Webhook accepts `spec.slices > 1` with External roles on create and both update directions.

#### e2e tests

- A `/scale` write distributes across slices and status converges to the total.
- An HPA loop drives an External role on a multi-slice set.
- Mid-rollout scale-down: the rolling slice holds its floor, steady slices track their shares, totals converge after the rollout.
- Slice scale-up and scale-down under an active scaler.

### Graduation Criteria

**Alpha**: restriction removed and the semantics above implemented behind no new API, with test coverage per the plan and documentation covering the `minReplicas >= slices` guidance and the Static vs External slice-count asymmetry.

**Beta**: production feedback from multi-slice autoscaled deployments, following KEP-849's beta items where they intersect.

**Stable**: proven across autoscalers (HPA v2, KEDA, custom) with no open bugs on the multi-slice scaler pathway.

## Implementation History

- 2026-07-29: KEP drafted after the KEP-849 implementation ([#922](https://github.com/kubernetes-sigs/lws/pull/922)) merged with the single-slice restriction. A prototype of this design was implemented and validated end to end on a live cluster: HPA-driven scale up and down, slice-count changes under an active scaler, and per-slice guard behavior under a mid-rollout scale-down.

## Drawbacks

- `slices` behaves differently for External roles (layout only) than for Static roles (capacity multiplier), an asymmetry documentation must carry.
- Remainder placement is a controller-owned decision invisible in the API. The fixed lowest-index rule keeps it predictable, and operators who need explicit per-slice counts can use Static roles.

## Alternatives

### Alternative 1: Per-slice scaler CRs

One scaler per `(DS, role, slice)` is more Kubernetes-idiomatic in isolation but forces `roles x slices` scalers and HPAs kept in sync, splits one capacity decision into N uncoordinated loops over thinner metric pools, and turns adding a slice into an autoscaling configuration change. Rejected for operational cost.

### Alternative 2: Per-role value applied per slice

Consistent with inline replicas, but HPA writes N and gets N x slices pods, and the aggregate selector and status would have HPA divide by fleet-wide counts while its writes get multiplied. Rejected.

### Alternative 3: Enforce a minimum of one replica per slice

Flooring each share at 1 silently inflates the autoscaler's decision into permanent desired-vs-observed drift, and rejecting `/scale` writes breaks HPA's loop outright. The topology constraint belongs in autoscaler configuration (`minReplicas >= slices`), so it stays a documented recommendation.
