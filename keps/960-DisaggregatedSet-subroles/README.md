# KEP-960: DisaggregatedSet Sub-Roles

<!--
This KEP proposes partitioning a DisaggregatedSet role's homogeneous LWS replica
groups into independently scalable, dynamically labelled sub-roles.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Example](#example)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
    - [Validation](#validation)
  - [Labels, Identity, and Scaler Names](#labels-identity-and-scaler-names)
  - [Controller and Assignment](#controller-and-assignment)
  - [Rolling Updates](#rolling-updates)
  - [Scaler Semantics](#scaler-semantics)
  - [Routing and Deprecated PRV Services](#routing-and-deprecated-prv-services)
  - [Status, Slices, and Compatibility](#status-slices-and-compatibility)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration and e2e tests](#integration-and-e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Model sub-roles as parent roles](#model-sub-roles-as-parent-roles)
<!-- /toc -->

## Summary

This KEP adds optional `subRoles` to a DisaggregatedSet role. Sub-roles divide the
homogeneous replica groups of one role into independently scalable routing pools while
retaining one shared `LeaderWorkerSet` configuration and one physical LeaderWorkerSet
per parent role, slice, and revision.

For example, a `decode` role can contain `short-context` and `long-context` sub-roles.
Both run the same pod template, but llm-d can route requests using a controller-managed
Pod label. Each sub-role has a static replica target or a
`DisaggregatedSetRoleScaler`. The physical parent replica target is their sum:

```text
decode replicas = short-context replicas + long-context replicas
```

Sub-role membership is a routing assignment of an interchangeable LWS replica group,
not a workload configuration. The controller changes it by updating Pod labels, without
restarting the group or rolling the LWS template.

## Motivation

A single decode pool can suffer from head-of-line blocking and scheduling bubbles when
heterogeneous requests share one serving queue. Short and long context requests, for
example, may need separate queues despite using the same model, image, resources, and
LWS topology.

Today operators must represent these pools as separate DisaggregatedSet roles or LWS
objects. That duplicates configuration and makes their combined capacity implicit.
Sub-roles represent the actual distinction: one configured role with several routing and
scaling partitions.

The current minimum of two parent roles is unnecessary for this API. Its purpose was to
keep `DisaggregatedSet` semantically tied to a disaggregated topology, but it complicates
the API without providing a useful invariant. This KEP permits a single parent role and
a single sub-role. Besides simplifying validation, this gives users a migration path:
start with a one-role DisaggregatedSet as a standard deployment, introduce one sub-role,
add further routing pools over time, and later extend it into a fully disaggregated
topology by adding parent roles.

### Goals

1. Add optional, named sub-roles that always share their parent LWS configuration.
2. Allow Static or External replica control independently for every sub-role.
3. Set the physical parent replica count to the sum of effective sub-role targets.
4. Assign LWS groups using a mutable, controller-managed Pod label.
5. Preserve assignments where possible and reconstruct them after Pod or controller
   restarts.
6. Preserve coordinated rollout safety while tracking sub-role availability.
7. Make the label usable by Kubernetes-aware routers such as llm-d.
8. Permit one parent role and one sub-role so the topology can be extended incrementally.
9. Always expose parent-role and sub-role revision (`-prv`) Services during their
   deprecation period.

### Non-Goals

1. **Different sub-role templates.** Images, arguments, resources, placement, LWS group
   size, and rollout settings remain parent-level. A configuration difference requires
   another parent role.
2. **Request classification.** The controller labels groups; a router decides which
   sub-role should serve a request.
3. **Built-in autoscaling.** HPA, KEDA, or another `/scale` writer remains responsible
   for External targets.
4. **Simultaneous parent and child scale control.** Sub-role targets are authoritative
   whenever `subRoles` is present.
5. **Multi-slice External scaling.** This follows the initial KEP-849 restriction until
   aggregate versus per-slice scaler semantics are defined.

## Proposal

Add `subRoles` to `DisaggregatedRoleSpec`. A role without sub-roles behaves exactly as it
does today. When sub-roles are present:

- there is at least one uniquely named entry;
- replica ownership moves from the parent to its sub-roles;
- each sub-role resolves a Static or External desired count;
- the parent LWS target is the sum of those counts; and
- every physical LWS group receives one sub-role label.

The controller deliberately does not create one LWS per sub-role. Sub-roles are lighter
weight routing partitions inside one homogeneous physical role.

### Example

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: my-model
spec:
  roles:
    - name: decode
      subRoles:
        - name: short-context
          scaling:
            mode: External
        - name: long-context
          replicas: 2
      spec:
        leaderWorkerTemplate:
          size: 1
          restartPolicy: RecreateGroupOnPodRestart
          workerTemplate:
            metadata:
              labels:
                llm-d.ai/role: decode
            spec:
              containers:
                - name: vllm
                  image: example.com/vllm:latest
                  ports:
                    - name: http
                      containerPort: 8000
                  resources:
                    limits:
                      nvidia.com/gpu: "1"
```

The controller creates `my-model-decode-short-context`. If that scaler requests five
replicas, the single decode LWS target is `5 + 2 = 7`.

### Risks and Mitigations

**Recreated Pods initially lack their dynamic label.** The controller watches managed
Pods and fills assignment deficits. Unlabelled groups are not selected by sub-role-aware
routing.

**StatefulSet scale-down chooses high ordinals, not a requested sub-role.** Before
scaling down, the allocator swaps labels between interchangeable groups so the groups
that will be removed represent the planned sub-role drain.

**Parent and child targets could conflict.** Parent scaling is rejected when sub-roles
are present and parent `spec.replicas` is ignored. The webhook warns when an explicit
parent replica value greater than one is observed; the inherited LWS default prevents a
strict absence check.

**Per-child rollout budgets could exceed the parent envelope.** `maxSurge` and
`maxUnavailable` remain parent settings and are enforced after child counts are
aggregated.

**Routers observe label changes eventually.** Different router replicas may briefly
disagree on membership, but both destinations have identical runtime configuration.
This affects only temporary request distribution.

**Service-only clients may observe an incomplete revision.** The deprecated parent-role
and sub-role `-prv` Services are no longer a cross-role readiness signal. Native llm-d
routing watches Ready Pods and applies revision gating itself.

## Design Details

### API

```go
// DisaggregatedSubRoleSpec defines a routing and scaling partition within a
// configuration-identical parent role.
type DisaggregatedSubRoleSpec struct {
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=63
    // +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
    // +required
    Name string `json:"name"`

    // Static desired group count. Defaults to 1 in the controller when
    // scaling is Static or omitted. Must be absent for External scaling.
    // +optional
    // +kubebuilder:validation:Minimum=0
    Replicas *int32 `json:"replicas,omitempty"`

    // Omitted or Static uses Replicas; External uses a role scaler.
    // +optional
    Scaling *RoleScaling `json:"scaling,omitempty"`
}

type DisaggregatedRoleSpec struct {
    Name string `json:"name"`

    // +optional
    // +listType=map
    // +listMapKey=name
    // +kubebuilder:validation:MinItems=1
    // +kubebuilder:validation:MaxItems=32
    SubRoles []DisaggregatedSubRoleSpec `json:"subRoles,omitempty"`

    // Existing KEP-849 field. Must be absent when SubRoles is present.
    Scaling *RoleScaling `json:"scaling,omitempty"`

    leaderworkerset.LeaderWorkerSetTemplateSpec `json:",inline"`
}
```

Desired replicas resolve as follows:

```text
if scaling.mode == External:
    desired = scaler.spec.replicas
else if replicas is set:
    desired = replicas
else:
    desired = 1

parentDesired(role) = sum(desired(subRole) for subRole in role.subRoles)
```

Percentage rollout budgets are calculated against `parentDesired`, not the ignored or
defaulted parent `spec.replicas`. A zero sub-role does not force its siblings to zero.

#### Validation

`spec.roles` changes from `MinItems=2` to `MinItems=1`, and `subRoles`, when present, may
contain one entry. No aggregate role-count validation is applied. The existing maximum
of 10 parent roles remains, and each parent may define at most 32 sub-roles.

External sub-roles cannot set `replicas`. Parent `scaling` is forbidden and parent
`spec.replicas` has no effect when `subRoles` is present. The system sub-role label is
reserved and cannot appear in user Pod templates.

### Labels, Identity, and Scaler Names

The controller applies:

```go
SubRoleLabelKey = "disaggregatedset.x-k8s.io/subrole"
```

An assigned Pod retains its parent role and gains a sub-role:

```yaml
disaggregatedset.x-k8s.io/name: my-model
disaggregatedset.x-k8s.io/role: decode
disaggregatedset.x-k8s.io/subrole: short-context
disaggregatedset.x-k8s.io/revision: abc12345
leaderworkerset.sigs.k8s.io/group-index: "4"
```

The assignment identity is `(LWS UID, group-index)` because old and new revisions can
both contain group index zero. The leader's assignment is authoritative and is mirrored
to the other Pods in its group.

External sub-role scalers are named `<ds>-<role>-<subrole>` and carry both role labels.
Admission rejects names exceeding the Kubernetes limit and collisions between generated
role and sub-role scaler names. When sub-roles exist, no parent scaler is created.

### Controller and Assignment

The reconciler watches Pods through a mapping from the existing
`disaggregatedset.x-k8s.io/name` label to the parent DisaggregatedSet. It receives Pod
`get`, `list`, `watch`, and `patch` permissions and patches only the sub-role label.
Converged assignments produce no writes.

Internally, planning roles use a structured identity:

```go
type RoleKey struct {
    Role    string
    SubRole string // empty for an unpartitioned role
}
```

Reconciliation follows this flow:

```text
Static/scaler child targets
          |
          v
Expanded planner roles: decode/short, decode/long
          |
          v
Physical aggregation: decode = short + long
          |
          v
LWS scale and group-label assignment
```

For each `(slice, revision, parent role)`, the assignment algorithm:

1. Preserves valid assignments up to each desired count.
2. Treats excess assignments as surplus and missing labels as unassigned.
3. Assigns available groups to the largest positive `desired - assigned` deficit.
4. Breaks ties by sub-role spec order, then group index.
5. Patches only changed labels.

New scale-up groups receive no sub-role traffic until assigned. Before scale-down, label
swaps prepare the high ordinals that LWS will remove; the LWS replica write occurs on a
later reconcile after the assignment is observed.

No separate assignment CRD is needed. Live labels provide stickiness, and the desired
counts reconstruct missing labels after a Pod restart.

### Rolling Updates

The planner operates on expanded roles so availability is visible per sub-role. The
executor aggregates the resulting counts before writing the single parent LWS scale
field and enforces the parent surge and unavailability envelope.

At rollout start, the old LWS snapshots both its aggregate initial replicas and its
sub-role distribution, for example:

```yaml
disaggregatedset.x-k8s.io/initial-subrole-replicas: >-
  {"short-context":5,"long-context":2}
```

New groups are assigned according to the planned new-revision vector. Before old groups
are removed, their high-ordinal labels are prepared for the planned drain. A
target-positive sub-role remains represented; target-zero sub-roles do not block
readiness or completion.

Sub-role target or membership changes do not change the revision hash because the
parent LWS template is unchanged. Parent template changes continue to trigger the
normal coordinated rollout.

### Scaler Semantics

KEP-849's `DisaggregatedSetRoleScaler` is extended to External sub-roles:

- `spec.replicas` is the desired assigned group count.
- `status.replicas` is the assigned leader count across live revisions.
- `status.selector` selects one leader per assigned group using set, role, sub-role, and
  `worker-index=0` labels.
- Owner-reference events enqueue the DisaggregatedSet, and removed or Static sub-roles
  lose their scalers.

On a running role, new scalers are seeded from the deterministic initial assignment so
enabling sub-roles preserves total capacity. For a fresh role, each External scaler is
seeded at one replica for vanilla-HPA bootstrap; scale-to-zero-aware autoscalers may
subsequently write zero.

### Routing and Deprecated PRV Services

llm-d's Kubernetes discovery watches selected Pods and refreshes endpoint metadata on
label updates. A broad decode InferencePool can continue selecting:

```yaml
selector:
  matchLabels:
    llm-d.ai/role: decode
```

Request handling can then filter endpoints using:

```yaml
matchLabels:
  disaggregatedset.x-k8s.io/subrole: short-context
```

Changing the sub-role label updates the endpoint metadata without removing the Pod from
the broad decode pool or restarting llm-d. Existing requests may finish while new
requests observe the new label; no protocol-specific drain is required because the
runtime configuration is identical.

During the deprecation period, the controller always exposes:

- the existing parent `-prv` Service for every live `(DisaggregatedSet, slice, revision,
  parent role)`, selecting all groups in the parent; and
- one `-prv` Service for every sub-role, adding the sub-role label to its selector.

The parent Service remains `<lws-name>-prv`; sub-role Services are named
`<lws-name>-<subrole>-prv`. The controller creates them as soon as the role revision
exists; no peer role needs to be present or ready. It deletes them when that revision is
drained. Admission rejects generated names that exceed the Kubernetes Service name limit
or collide with another generated Service name.

The `-prv` Services are deprecated and retained temporarily for compatibility with naive
Service-based load balancers. Native llm-d routing watches Ready Pods and performs
revision gating directly, as introduced by
[llm-d-router PR 2141](https://github.com/llm-d/llm-d-router/pull/2141), so it does not
depend on them. A `-prv` Service's existence must therefore not be interpreted as
meaning that the complete revision is ready. The controller will keep exposing these
Services unconditionally until their eventual removal completes the deprecation.

### Status, Slices, and Compatibility

`RoleStatus` remains the parent aggregate and gains `subRoleStatuses` with replicas,
ready replicas, and updated replicas per child. Parent values are their sums. A
`SubRolesAssigned` condition reports whether every extant group has a valid assignment.

Static sub-role replicas retain the existing per-slice meaning. Alpha rejects
`spec.slices > 1` when any sub-role is External, following KEP-849. Assignment identity
already includes the slice, allowing a later KEP to add aggregate or per-slice scaling.

Omitting `subRoles` preserves existing names, labels, scaling, and status. Existing
parent `-prv` Services keep their names and selectors. Adding sub-roles adds their
selector-specific Services without changing the broad parent Service. All are created
unconditionally for every live role revision during deprecation.
Enabling it labels existing groups in place and does not change the revision hash.
Disabling it removes the dynamic labels and returns replica ownership to the parent.
Changing the minimum parent-role count from two to one is a backward-compatible schema
relaxation.

### Test Plan

[X] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to implementation.

#### Unit tests

- API validation, replica resolution, and generated scaler names.
- Stable assignment, deficit filling, Pod recreation, and high-ordinal scale-down.
- Expanded planner state, parent budget aggregation, and initial snapshot parsing.
- Scaler creation, seeding, status, selector, and cleanup.
- Parent status aggregation and revision-hash stability.

#### Integration and e2e tests

- One parent with one or more sub-roles creates one physical LWS with summed replicas.
- Static and External target changes converge labels with minimal reassignment.
- Pod recreation restores assignment; scale-down preserves the requested distribution.
- Template rollout preserves sub-role availability and parent capacity limits.
- llm-d observes label changes and filters subsequent endpoint candidates.
- Parent and sub-role `-prv` Services have the expected selectors, are created before
  cross-role readiness, and are not used by native routing.
- Static multi-slice behavior works; External multi-slice objects are rejected in alpha.

### Graduation Criteria

**Alpha**:

- API, validation, parent aggregation, label assignment, rollout integration, scaler
  status/selectors, and unit/integration/e2e coverage.
- Single-slice restriction for External sub-roles.

**Beta**:

- Production feedback from a short/long-context deployment.
- Assignment and convergence metrics.
- Decision on aggregate or per-slice External scaler semantics.

**Stable**:

- Proven rollout and autoscaling stability with no known assignment-loss or routing
  correctness issues.

## Implementation History

- 2026-08-04: Initial provisional KEP draft.

## Drawbacks

1. The controller becomes a Pod-label writer in addition to managing LWS and Services.
2. Desired state spans the parent API, optional scalers, aggregate LWS replicas, and Pod
   labels.
3. Membership is eventually consistent across router caches.
4. A one-parent DisaggregatedSet broadens the API beyond its original topology scope.
5. Rollout execution must translate child plans into one physical scale field.

## Alternatives

### Model sub-roles as parent roles

Copying `decode` into `decode-short` and `decode-long` works today, but duplicates the
template and leaves aggregate capacity implicit.
