# KEP-948: DisaggregatedSet External Scaling for Multi-Slice Sets

<!--
This KEP lifts the KEP-849 alpha restriction that a DisaggregatedSet with any
scaling.mode: External role must have spec.slices == 1, by defining aggregate
semantics for DisaggregatedSetRoleScaler across slices.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: One HPA per role across a rack-partitioned fleet](#story-1-one-hpa-per-role-across-a-rack-partitioned-fleet)
    - [Story 2: Mixed static and external roles in a multi-slice set](#story-2-mixed-static-and-external-roles-in-a-multi-slice-set)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Replica Scope: Aggregate](#replica-scope-aggregate)
  - [Distribution Across Slices](#distribution-across-slices)
  - [Example: Scaling Under Uneven Slice Load](#example-scaling-under-uneven-slice-load)
  - [Where Resolution Happens](#where-resolution-happens)
  - [Scaler Status Across Slices](#scaler-status-across-slices)
  - [No-Shrink Guard Per Slice](#no-shrink-guard-per-slice)
  - [Seeding](#seeding)
  - [Changing the Slice Count](#changing-the-slice-count)
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
  - [Alternative 4: Capacity-weighted distribution](#alternative-4-capacity-weighted-distribution)
<!-- /toc -->

## Summary

[KEP-849](/keps/849-DisaggregatedSet-HPA) added per-role external scaling to
DisaggregatedSet through the auto-created `DisaggregatedSetRoleScaler` CRD,
but its alpha scope is restricted to single-slice sets: the webhook rejects
`spec.slices > 1` while any role has `scaling.mode: External`. This KEP lifts
that restriction by defining the multi-slice semantics KEP-849 deliberately
left open. The scaler's `spec.replicas` becomes an **aggregate** count across
all slices, the controller distributes it deterministically among slices, the
scaler's aggregate `status.replicas` and `status.selector` keep their existing
shape, and the mid-rollout no-shrink guard is evaluated per `(slice, role)` so
independently rolling slices cannot clamp each other. One scaler and one HPA
per role continue to serve the whole set regardless of slice count.

## Motivation

Slices ([KEP-846](/keps/846-disaggregatedset-slices)) and per-role external
scaling ([KEP-849](/keps/849-DisaggregatedSet-HPA)) are both merged, but they
are mutually exclusive today. That exclusion cuts against the deployments that
want both the most: slices exist to map complete P/D topologies onto expensive
accelerator domains (an NVL72 rack, an NVLink domain, a zone), and precisely
those deployments have the strongest incentive to autoscale a role like
prefill against traffic instead of pinning replicas statically.

KEP-849 enumerated three open questions for the multi-slice case and shipped
alpha with `slices == 1` rather than prejudge them:

1. What does `scaler.spec.replicas` mean when there are N slices?
2. What should `status.selector` match?
3. How does the no-shrink guard behave when each slice rolls on its own clock?

This KEP answers all three. The current implementation is already most of the
way to the aggregate model: the controller passes one shared scaler map into
every slice's reconcile, and scaler status is already summed across all slices
and revisions with a slice-agnostic selector. What is missing is a defined
distribution of the desired count among slices and a per-slice guard.

### Goals

- Allow `spec.slices > 1` together with `scaling.mode: External` roles.
- Define `scaler.spec.replicas` as the total desired count for the role across
  all slices, with a deterministic, stable distribution among slices.
- Keep HPA's ratio math consistent: the pods matched by `status.selector`, the
  count reported in `status.replicas`, and the value written to
  `spec.replicas` all use the same unit (LWS groups across the whole set).
- Make the rolling-update no-shrink guard correct under concurrent per-slice
  rollouts by evaluating it per `(slice, role)`.
- Define behavior when `spec.slices` changes while a role is externally
  scaled.

### Non-Goals

1. **Per-slice scalers or per-slice HPAs.** One scaler per role remains the
   only shape. See [Alternatives](#alternative-1-per-slice-scaler-crs).
2. **Autoscaling the slice count.** A DS-level `/scale` mapped to
   `spec.slices` is a separate feature (KEP-849, Alternative 2) and is
   complementary to per-role scaling, not part of this KEP.
3. **Heterogeneous slices.** Slices are identical copies by design (KEP-846),
   so the distribution assumes uniform slice capacity.
4. **Coupling HPA writes to the old-revision drain schedule.** This remains
   future work as described in KEP-849's Rolling Update Interaction section.

## Proposal

Make the existing `DisaggregatedSetRoleScaler` slice-aware without changing
its API surface. An external autoscaler keeps writing a single number through
`/scale`. The DisaggregatedSet controller interprets that number as the
role's total across slices, splits it with a fixed distribution function, and
feeds each slice's share into that slice's independent reconcile loop as the
role's target. Observed state flows back the way it already does: the
controller sums replicas across all slices and revisions into
`status.replicas`, and `status.selector` continues to match one leader pod
per group across the whole set.

The webhook rejection of `spec.slices > 1` with External roles is removed.

### User Stories

#### Story 1: One HPA per role across a rack-partitioned fleet

An inference platform runs a DisaggregatedSet with 8 slices, one per NVL72
rack, with `placementPolicy` confining each slice to its rack. The operator
sets `scaling.mode: External` on the prefill role and creates a single HPA
(or KEDA ScaledObject) targeting `<ds>-prefill`. As traffic grows, HPA raises
the aggregate count and the controller spreads the new prefill groups across
the racks. Adding a ninth rack later means raising `spec.slices` and nothing
else: no new HPA, no new scaler, no autoscaling reconfiguration.

#### Story 2: Mixed static and external roles in a multi-slice set

A wide expert-parallel deployment runs 2 slices with a fixed decode topology
(`Static`, sized to the model's parallelism) and a traffic-proportional
prefill tier (`External`). Decode stays at its per-slice count from
`spec.roles[].spec.replicas` while prefill floats between an HPA minimum and
maximum across both slices. A rolling update in slice 0 does not block or
distort scaling decisions applying to slice 1.

### Notes/Constraints/Caveats

- `spec.roles[].spec.replicas` is a per-slice count for Static roles, while
  `scaler.spec.replicas` is a total for External roles. These units differ by
  design: the scaler is written by autoscalers whose mental model is "total
  desired capacity", while inline replicas describe the shape of one slice.
  The webhook already warns that inline replicas are ignored for External
  roles, which keeps the two from being confused on the same role.
- If the aggregate count is smaller than the slice count, some slices run
  zero replicas of the role. See
  [Risks and Mitigations](#risks-and-mitigations).

### Risks and Mitigations

**A slice can hold zero replicas of an External role.** With `R < S`, the
distribution leaves `S - R` slices without the role, and a slice without
prefill (for example) cannot serve end to end on its own. Mitigation: this is
documented, and the recommended configuration is HPA `minReplicas >= slices`
so every slice keeps at least one group. We deliberately do not hard-enforce
a per-slice minimum, see
[Alternative 3](#alternative-3-enforce-a-minimum-of-one-replica-per-slice).

**Rebalancing on slice-count changes.** For an External role the total is
owned by the autoscaler, so raising `spec.slices` redistributes the same
total over more slices instead of multiplying capacity the way it does for
Static roles. This asymmetry is documented (see
[Changing the Slice Count](#changing-the-slice-count)). It follows directly
from the aggregate contract and matches operator expectations for
autoscaled fleets: capacity follows load, layout follows topology.

**Transient overshoot during concurrent rollouts.** The per-slice no-shrink
guard can hold a mid-rollout slice above its share while HPA scales down,
so the realized total may briefly exceed `spec.replicas`. This is bounded by
the in-flight new-revision counts, converges when rollouts complete, and is
the same behavior single-slice alpha already has, applied per slice. HPA
tolerates observed counts above desired by design.

## Design Details

### Replica Scope: Aggregate

`scaler.spec.replicas` is the desired total number of LWS groups for the role
summed over all slices. KEP-849 already identified this as the strongest
shape for hardware-partitioned deployments: autoscaling configuration does
not grow with the fleet, HPA makes one decision over one metric pool, and
distribution is well defined because slices are uniform copies (KEP-846).

### Distribution Across Slices

The controller splits the aggregate `R` over `S` slices with a fixed
function:

```
distribute(R, S, i) = floor(R / S) + 1  if i < R mod S
                      floor(R / S)      otherwise
```

Slice `i`'s target for the role is `distribute(R, S, i)`. Properties:

- **Deterministic and stateless.** The target depends only on `(R, S, i)`,
  never on reconcile history, so every slice's independent reconcile computes
  a consistent view with no cross-slice coordination.
- **Balanced.** Slice targets differ by at most one.
- **Monotone.** Increasing `R` never decreases any slice's target, and
  decreasing `R` never increases one. Scale-ups land on the lowest-indexed
  slices first and scale-downs come off the highest-indexed slices first,
  mirroring KEP-846's slice scale-down semantics (highest slices removed
  first).

### Example: Scaling Under Uneven Slice Load

A DisaggregatedSet with `slices: 3` runs an External prefill role at an
aggregate of 5 (distributed 2/2/1) and a Static decode role with
`replicas: 5` per slice (5/5/5, 15 total). Suppose slice 1's two prefill
groups run hot while the rest of the fleet idles. HPA sees the role's
fleet-wide average cross its target and raises the aggregate from 5 to 6,
which distributes to 2/2/2. The new group lands on slice 2, the slice that
was carrying the remainder deficit, not on the hot slice 1.

This is usually fine, but note what the model is actually promising. The
new capacity is not placed to relieve slice 1 directly. Slices are a
deployment and placement construct, and request routing (the llm-d EPP, or
any slice-blind balancer) treats the role's pods as one flat pool. With
one more group in the pool, the balancer spreads the same traffic over 6
groups instead of 5, and slice 1's pressure drains through routing rather
than through placement. The aggregate model assumes load balancing across
slices redistributes load onto new capacity faster than any per-slice
placement decision could.

That assumption can break when routing is intentionally sticky. Prefix
cache aware routing keeps sending requests that share a prompt prefix to
the pods that already hold the warm cache, which is exactly the slice that
is already hot. The new group on slice 2 then absorbs little of the load,
since the router keeps preferring the warm cache over the idle capacity,
and the hot slice stays hot until the cache ages out or the router's load
signal overrides affinity. Scaling up can even reinforce the pattern by
leaving the affinity target unchanged while the fleet average (HPA's
signal) drops back under target. The aggregate model accepts this
limitation: placing capacity on the hot slice would require per-slice
scaling (see [Alternative 1](#alternative-1-per-slice-scaler-crs)) plus
slice-aware routing, and the cleaner mitigation lives in the router, which
should cap prefix affinity when its target saturates.

### Where Resolution Happens

Today the executor resolves an External role's target by reading
`scaler.spec.replicas` directly inside each slice's planner state, which is
exactly the per-role-applied-per-slice footgun if the restriction were
naively lifted. This KEP moves resolution up into the controller: before the
per-slice loop, the controller computes a per-`(slice, role)` target map,
applying `distribute()` for External roles and the inline per-slice
`spec.replicas` for Static roles. Each slice's executor then receives plain
resolved targets and no longer consults scalers at all. Rolling-update
percentage budgets (`maxSurge`, `maxUnavailable`) are computed per slice
against that slice's distributed target, consistent with their existing
per-slice meaning for Static roles.

### Scaler Status Across Slices

Unchanged, and this is the decisive argument for aggregate scope: the merged
implementation already writes the aggregate. `status.replicas` sums observed
groups across all slices and revisions, and `status.selector`
(`disaggregatedset.x-k8s.io/name=<ds>,disaggregatedset.x-k8s.io/role=<role>,leaderworkerset.sigs.k8s.io/worker-index=0`)
matches one leader per group in every slice. HPA's ratio math stays
self-consistent in multi-slice sets for the same reason it does during
multi-revision rollouts: the value HPA writes, the count it reads, and the
pods its selector matches all count LWS groups over the whole serving fleet.

### No-Shrink Guard Per Slice

The guard keeps its KEP-849 purpose: while a role's old revision is still
draining, the new-revision target must not shrink below what is already in
flight. With slices it is evaluated inside each slice's reconcile against
that slice's own state: slice `i` clamps `distribute(R, S, i)` to its current
new-revision replica count while it has old revisions present. Because each
slice's executor already runs on per-slice state, the clamp in one slice
cannot see (and therefore cannot distort) another slice's rollout. A slice in
steady state follows its distributed share immediately.

### Seeding

Seeding generalizes from KEP-849 with the slice count in place of 1:

- **Fresh External role:** seed `spec.replicas = S` (one group per slice), so
  every slice serves from the start and vanilla HPA can bootstrap without
  scale-from-zero support.
- **Static to External flip:** seed with the observed total, the sum of
  current LWS replicas for the role across all slices and revisions, so the
  flip does not resize a running fleet.

### Changing the Slice Count

For External roles the autoscaler owns the total, so slice-count changes are
pure layout changes:

- **Scale up (`S -> S'`)**: new slices are created and the next reconcile
  redistributes the same `R` over `S'` slices. Some existing slices scale
  down by at most the rebalance delta while new slices fill. Total capacity
  is unchanged until the autoscaler reacts to the metric.
- **Scale down**: the highest slices' resources are deleted (KEP-846
  semantics), and redistribution over the remaining slices raises their
  targets to absorb `R`. The set self-heals to the full aggregate without
  waiting for the autoscaler.

This differs from Static roles, where changing `slices` multiplies total
capacity. The difference is documented in the DisaggregatedSet docs.

### Validation Changes

- Remove the webhook rejection of `spec.slices > 1` while any role has
  `scaling.mode: External`.
- Keep the existing warning that inline `spec.replicas` is ignored for
  External roles.

No API fields are added or changed on DisaggregatedSet or
DisaggregatedSetRoleScaler.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

#### Unit tests

- `distribute()` properties: balance, monotonicity under changing `R` and
  `S`, zero-replica slices when `R < S`, `R = 0`.
- Per-`(slice, role)` target resolution mixing Static and External roles.
- No-shrink clamp applied against one slice's in-flight count without
  affecting siblings.
- Seed computation: fresh role seeds to `S`, flip seeds to the observed
  cross-slice total.

#### Integration tests

- Webhook accepts `spec.slices > 1` with External roles on create and on
  update in both directions (raising slices on an External set, flipping a
  role to External on a multi-slice set).
- Scaler status aggregates across slices with the slice-agnostic selector.

#### e2e tests

- `/scale` write against a 2-slice set distributes across slices
  (`R = 3, S = 2` yields 2 and 1) and `status.replicas` converges to `R`.
- HPA loop drives an External role on a multi-slice set via a synthetic
  metric.
- Rolling update in one slice while `/scale` writes land: the rolling slice
  holds its floor, the steady slice tracks its share, totals converge after
  the rollout.
- Slice scale-up and scale-down under an active scaler: rebalance on scale-up,
  absorption on scale-down.

### Graduation Criteria

**Alpha (v0.X)**:
- Restriction removed, aggregate distribution implemented behind no new API.
- Per-`(slice, role)` no-shrink guard.
- Seeding generalized to the slice count.
- Test coverage per plan above.
- Documentation: aggregate semantics, `minReplicas >= slices` guidance, and
  the Static vs External slice-count-change asymmetry.

**Beta**:
- Production feedback from multi-slice autoscaled deployments incorporated.
- Follow KEP-849's beta items where they intersect (metrics, drain coupling).

**Stable**:
- Proven stability across autoscalers (HPA v2, KEDA, custom) on multi-slice
  sets.
- No open bugs on the multi-slice scaler pathway.

## Implementation History

- 2026-07-29: KEP drafted, following the merge of the KEP-849 implementation
  ([#922](https://github.com/kubernetes-sigs/lws/pull/922)) with the
  single-slice alpha restriction in place.

## Drawbacks

- The aggregate contract makes `slices` behave differently for External roles
  (layout only) than for Static roles (capacity multiplier). The asymmetry is
  inherent to letting an autoscaler own the total and must be carried by
  documentation.
- Distribution adds a controller-owned decision (which slice gets the
  remainder) that is invisible in the API. The fixed lowest-index-first rule
  keeps it predictable, but operators wanting explicit per-slice counts must
  use Static roles.

## Alternatives

### Alternative 1: Per-slice scaler CRs

One `DisaggregatedSetRoleScaler` per `(DS, role, slice)`, each a clean
`/scale` target with a slice-filtered selector. Most Kubernetes-idiomatic in
isolation, but it forces `roles x slices` scalers and HPAs kept in sync (16
objects per role change for an 8-rack fleet), splits one capacity decision
into N uncoordinated loops fed by N thinner metric pools, and makes adding a
slice an autoscaling-config change. It would also change the meaning of
today's scaler name and selector. Rejected for operational cost, KEP-849
already leaned away from it for the same reason.

### Alternative 2: Per-role value applied per slice

Interpret `scaler.spec.replicas` as a per-slice count, consistent with
`spec.roles[].spec.replicas`. This is the UX footgun KEP-849 called out (HPA
writes N and gets `N x slices` pods) and it breaks HPA's math: the selector
and `status.replicas` are aggregate, so HPA would divide by the fleet-wide
count while its writes get multiplied. Making status per-slice instead is not
possible with a single scaler. Rejected.

### Alternative 3: Enforce a minimum of one replica per slice

Have the controller floor each slice's share at 1 (or reject `R < S` at the
`/scale` boundary) so every slice always serves. This silently inflates the
autoscaler's decision (HPA writes R, gets S), which breaks the `/scale`
contract and shows up as permanent desired-vs-observed drift, and rejecting
subresource writes breaks HPA's loop outright. The topology constraint
belongs in the autoscaler's own configuration (`minReplicas >= slices`), so
this stays a documented recommendation.

### Alternative 4: Capacity-weighted distribution

Distribute proportionally to per-slice capacity signals instead of evenly.
Slices are uniform replicas of the same role topology by KEP-846's design, so
there is no capacity signal to weight by. If heterogeneous slices ever become
a feature, distribution can be revisited then.
