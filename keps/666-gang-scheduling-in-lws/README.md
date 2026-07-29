# KEP-666: Workload-Aware Gang Scheduling in LWS

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Kubernetes 1.37 Baseline](#kubernetes-137-baseline)
  - [User Stories](#user-stories)
  - [User-Facing API](#user-facing-api)
  - [Defaulting and Validation](#defaulting-and-validation)
  - [Scheduler Providers](#scheduler-providers)
  - [API Discovery and Cluster Prerequisites](#api-discovery-and-cluster-prerequisites)
- [Design Details](#design-details)
  - [Compiling an LWS into a Workload](#compiling-an-lws-into-a-workload)
  - [Workload and PodGroup Lifecycle](#workload-and-podgroup-lifecycle)
  - [Replica, Size, and Rollout Updates](#replica-size-and-rollout-updates)
  - [Parent Controller Integration](#parent-controller-integration)
  - [Unsupported Pod-Level Overrides](#unsupported-pod-level-overrides)
  - [Observability](#observability)
  - [Failure Handling](#failure-handling)
  - [Backwards Compatibility](#backwards-compatibility)
  - [Risks and Mitigations](#risks-and-mitigations)
  - [Examples](#examples)
  - [Test Plan](#test-plan)
    - [Unit Tests](#unit-tests)
    - [Integration Tests](#integration-tests)
    - [End-to-End Tests](#end-to-end-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

This KEP integrates LeaderWorkerSet (LWS) with the Kubernetes 1.37
Workload-Aware Scheduling (WAS) APIs. It adds a typed, alpha
`spec.scheduling` field to LWS and compiles an LWS into one
`scheduling.k8s.io/v1beta1` `Workload` plus one `PodGroup` for every active
LWS replica.

An LWS replica contains one leader and `size - 1` workers. In gang mode, all
pods in that replica reference the same PodGroup and the PodGroup's
`gang.minCount` equals the replica size. The scheduler therefore admits the
replica only when all of its members can be scheduled together. Different
replicas remain independent gangs and can make progress independently.

The LWS controller uses the standard
`k8s.io/component-helpers/scheduling/schedulingv1/workloadbuilder` library.
The user-facing field composes the reusable
`scheduling.k8s.io/v1alpha3` configuration blocks, while the objects consumed
by kube-scheduler are the `v1beta1` Workload and PodGroup APIs. This
distinction is important: Kubernetes 1.37 removed the old `v1alpha2` API.

The feature is guarded by the LWS `WorkloadAwareScheduling` feature gate and
uses the existing operator-level
`gangSchedulingManagement.schedulerProvider` setting. The upstream provider
is opt-in and requires Kubernetes' `GenericWorkload` feature gate on both
kube-apiserver and kube-scheduler.

## Motivation

Distributed inference replicas are useful only when their leader and workers
can run together. Scheduling their pods independently can lead to:

- partial scheduling, where scheduled members reserve resources but the
  replica cannot serve;
- deadlock, where several replicas each consume part of the cluster and none
  can obtain all required workers;
- inconsistent integration with topology, preemption, disruption, and shared
  device allocation features that operate on a workload rather than on one
  pod.

LWS already supports third-party gang schedulers through [KEP-407][kep407].
Kubernetes 1.37 provides an upstream Workload and PodGroup contract and
standard controller-integration guidance. LWS should use that contract rather
than maintain a parallel, LWS-specific representation.

### Goals

- Support upstream Kubernetes gang scheduling for an LWS replica.
- Represent one LWS as one Workload and each active replica as one PodGroup.
- Use the standard `v1alpha3` WAS building blocks and `workloadbuilder`
  translation library.
- Create scheduling objects in the strict order Workload, PodGroup, then Pod.
- Support replica scaling, rolling updates, `maxSurge`, leader recreation, and
  `LeaderWorkerTemplate.Size` updates.
- Allow LWS to operate as either a root WAS controller or a child of another
  registered workload controller.
- Preserve the existing Volcano integration while defining explicit provider
  capabilities for the new API.

### Non-Goals

- Implementing hierarchical or per-role scheduling for
  [DisaggregatedSet][kep766] in the first phase. Kubernetes 1.37 introduces
  `CompositePodGroup`, but it remains alpha and is handled as follow-up work.
- Expressing leader-first startup with a flat gang. Gang scheduling with
  `startupPolicy: LeaderReady` is rejected because the workers do not exist
  when the leader is expected to become ready.
- Supporting arbitrary user-managed Workload or PodGroup objects referenced
  directly from pod templates.
- Replacing provider-specific configuration such as Volcano queue annotations.
- Guaranteeing that optional alpha WAS capabilities are available merely
  because Workload and PodGroup discovery succeeds.

[kep407]: https://github.com/kubernetes-sigs/lws/tree/main/keps/407-gang-scheduling
[kep766]: https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet

## Proposal

### Kubernetes 1.37 Baseline

This KEP targets the Kubernetes 1.37 API and implementation, not the earlier
alpha design:

| Area | Kubernetes 1.37 state | LWS consequence |
| --- | --- | --- |
| Workload and PodGroup runtime APIs | `scheduling.k8s.io/v1beta1` | LWS creates and watches `v1beta1` objects. |
| Reusable controller API blocks | `scheduling.k8s.io/v1alpha3` | LWS embeds the standard policy, constraint, disruption, and resource-claim types in `spec.scheduling`. |
| `GenericWorkload` | Beta, default `false` | Operators must explicitly enable it in kube-apiserver and kube-scheduler. |
| `gang.minCount` | Mutable | LWS can support elastic size changes without rejecting all size updates. |
| Workload templates | Existing entries are updateable where their fields allow it; entries cannot be added or removed | LWS uses one stable leaf template and updates only mutable fields. |
| PodGroup protection | PodGroups have deletion protection | LWS owns PodGroups independently of leader Pods and follows ordered cleanup. |
| `TopologyAwareWorkloadScheduling` | Alpha, default `false` | Topology constraints require a separate cluster prerequisite. |
| `DRAWorkloadResourceClaims` | Alpha, default `false` | Shared claims require both DRA and WAS claim gates. |
| `CompositePodGroup` | Alpha, default `false` | Hierarchical LWS scheduling is deferred, but parent delegation is designed for it. |

The `scheduling.k8s.io/v1alpha2` API and its
`podGroupTemplateRef.workload` shape must not be used. In `v1beta1`, a
PodGroup links to its template through:

```yaml
workloadRef:
  workloadName: example
  templateName: replica
```

The upstream sources of truth are [KEP-4671][kep4671],
[KEP-6089][kep6089], the [`v1beta1` runtime types][runtime-types], the
[`v1alpha3` building blocks][building-blocks], and
[`workloadbuilder`][workloadbuilder].

[kep4671]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/4671-gang-scheduling
[kep6089]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/6089-was-controller-apis
[runtime-types]: https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/api/scheduling/v1beta1/types.go
[building-blocks]: https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/api/scheduling/v1alpha3/types.go
[workloadbuilder]: https://github.com/kubernetes/kubernetes/tree/master/staging/src/k8s.io/component-helpers/scheduling/schedulingv1/workloadbuilder

### User Stories

As an inference platform user, I want every leader-worker replica to be
admitted as one unit so that a partial replica does not waste scarce
accelerators.

As an autoscaling user, I want to add and remove independent replicas without
recreating the Workload definition or blocking replicas that already run.

As an operator, I want LWS to use the same Workload and PodGroup APIs as Job,
JobSet, Kueue, and kube-scheduler so that scheduling state, topology
constraints, and disruption behavior have one observable representation.

As a composite-controller author, I want a parent controller to compile the
root Workload and delegate the creation of per-replica PodGroups to LWS.

### User-Facing API

LWS adds an alpha `spec.scheduling` field. Its field names and types match the
standard controller-integration API:

```go
// api/leaderworkerset/v1/leaderworkerset_types.go
type LeaderWorkerSetSpec struct {
    // ... existing fields ...

    // Scheduling defines Workload-Aware Scheduling for this LWS.
    // Alpha; guarded by the WorkloadAwareScheduling feature gate.
    // +optional
    Scheduling *LeaderWorkerSetSchedulingConfiguration `json:"scheduling,omitempty"`
}

type LeaderWorkerSetSchedulingConfiguration struct {
    // SchedulingPolicy selects Basic or Gang scheduling.
    // The field and its variant are immutable after creation.
    // Only gang.minCount may change.
    // +optional
    SchedulingPolicy *schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy `json:"schedulingPolicy,omitempty"`

    // SchedulingConstraints defines group-level topology constraints.
    // Immutable after creation.
    // +optional
    SchedulingConstraints *schedulingv1alpha3.WorkloadPodGroupSchedulingConstraints `json:"schedulingConstraints,omitempty"`

    // DisruptionMode selects independent or all-at-once disruption.
    // Immutable after creation.
    // +optional
    DisruptionMode *schedulingv1alpha3.WorkloadPodGroupDisruptionMode `json:"disruptionMode,omitempty"`

    // ResourceClaims lists dynamic resource claims shared by replica members.
    // Immutable after creation.
    // +optional
    // +kubebuilder:validation:MaxItems=4
    // +listType=map
    // +listMapKey=name
    ResourceClaims []schedulingv1alpha3.WorkloadPodGroupResourceClaim `json:"resourceClaims,omitempty"`
}
```

Embedding the upstream building blocks keeps validation and future API
migration aligned with other workload controllers. LWS-specific structure
such as replicas, size, leader, workers, and subgroups remains in the LWS API;
users do not repeat that structure inside `spec.scheduling`.

### Defaulting and Validation

Defaulting is controller-side so the stored LWS preserves user intent:

- `spec.scheduling` absent means the new upstream integration is disabled for
  that LWS. No upstream Workload or PodGroup is created.
- `spec.scheduling: {}` defaults to Gang for LWS. This controller-specific
  default reflects that a leader-worker replica normally requires every
  member. `gang.minCount` defaults to
  `spec.leaderWorkerTemplate.size`.
- `schedulingPolicy.basic: {}` explicitly requests standard pod-by-pod
  scheduling while still allowing an upstream Workload representation for
  other requested WAS capabilities.
- `schedulingPolicy.gang: {}` defaults `minCount` to the replica size.

The validating webhook enforces:

1. The LWS `WorkloadAwareScheduling` gate is enabled.
2. Exactly one of `basic` and `gang` is selected after defaulting.
3. The scheduling field cannot be added or removed after LWS creation, and
   the Basic/Gang variant cannot change.
4. For the flat LWS gang implemented by this KEP,
   `gang.minCount == leaderWorkerTemplate.size`. A smaller value would admit a
   replica that cannot run, while a larger value can never be satisfied.
5. A size update and an explicitly set `gang.minCount` update must agree in
   the final object. If `minCount` is omitted, it continues to follow size.
6. Gang mode is incompatible with `startupPolicy: LeaderReady`.
7. Alpha rejects the combination of gang or WAS topology constraints with
   `leaderworkerset.sigs.k8s.io/exclusive-topology` until their combined
   placement and failure semantics are tested.
8. The selected scheduler provider supports every requested field.
9. Shared ResourceClaims have matching references in every member pod
   template that consumes them.

`SubGroupPolicy` does not create additional PodGroups in this phase. All
subgroups in one LWS replica remain members of the same PodGroup. Per-subgroup
policies require CompositePodGroup support.

### Scheduler Providers

Provider selection remains operator-level through the existing
`gangSchedulingManagement.schedulerProvider` configuration. This KEP adds an
upstream provider value, `kubernetes`, and extends the provider interface from
pod-only callbacks to workload compilation and lifecycle reconciliation.

The typed API is provider-neutral, but provider capabilities are not assumed
to be identical:

| Capability | `kubernetes` provider | Existing `volcano` provider |
| --- | --- | --- |
| Basic policy | Supported | Rejected for the typed API |
| Gang policy with replica-size minimum | Supported | Supported |
| Topology constraints | Requires `TopologyAwareWorkloadScheduling` | Rejected; existing provider annotations remain available |
| Disruption mode | Supported by upstream WAS | Rejected |
| Shared ResourceClaims | Requires `DRAWorkloadResourceClaims` and DRA | Rejected |
| Parent Workload delegation | Supported | Not part of this KEP |

Existing Volcano users who only configure the provider and do not set
`spec.scheduling` retain the behavior defined by KEP-407. This compatibility
mode is intentionally asymmetric. New integrations should use the typed
field; changing the legacy implicit behavior requires a separate deprecation
plan.

### API Discovery and Cluster Prerequisites

For the `kubernetes` provider, the LWS webhook and controller verify that
`scheduling.k8s.io/v1beta1` Workload and PodGroup resources are discoverable.
If either resource is missing, new opt-ins are rejected with the missing GVR
named in the error.

Discovery is necessary but not sufficient. It cannot prove that
kube-scheduler is running with the same feature gates as kube-apiserver.
Operators must enable `GenericWorkload` on both components. Optional fields
also require their corresponding gates:

- `TopologyAwareWorkloadScheduling` for `schedulingConstraints`;
- `DRAWorkloadResourceClaims` and `DynamicResourceAllocation` for shared
  claims;
- `CompositePodGroup` and `TopologyAwareWorkloadScheduling` when an LWS is
  attached below a CompositePodGroup.

`GenericWorkload` is Beta but remains disabled by default in Kubernetes 1.37.
LWS must not describe it as beta-on-by-default. An API rejection from the
server or an unsupported scheduler state is surfaced on the LWS through an
event and condition; pod creation remains blocked rather than silently
falling back to pod-by-pod scheduling.

## Design Details

### Compiling an LWS into a Workload

When LWS is the root workload controller, it builds one logical leaf item:

- item name: `replica`;
- default policy: Gang;
- default gang minimum: `leaderWorkerTemplate.size`;
- optional constraints, disruption mode, and claims copied from
  `spec.scheduling`.

The controller calls `workloadbuilder.NewBuilder(...).BuildWorkload()`.
`BuildWorkload` returns a `scheduling.k8s.io/v1beta1` Workload. LWS does not
hand-write conversions from `v1alpha3` user configuration to `v1beta1`
runtime objects.

The Workload:

- is named after the LWS;
- has a controller ownerReference to the LWS;
- sets `spec.controllerRef` to the LWS;
- contains one stable `podGroupTemplates[name=replica]` entry.

One template is sufficient because it is a blueprint. It can be instantiated
as any number of runtime PodGroups, including temporary surge replicas,
without adding template entries to the Workload.

### Workload and PodGroup Lifecycle

The LWS controller, rather than the leader-pod controller, manages the
upstream scheduling objects. The required order is:

1. Compile, create, or discover the Workload.
2. Instantiate every required PodGroup from the persisted Workload template
   with `NewBuilderFromExistingWorkload(...).NewPodGroup(...)`.
3. Only after its PodGroup exists, allow the leader StatefulSet and worker
   resources for that replica to create pods.
4. Stamp every member pod with
   `spec.schedulingGroup.podGroupName = <pod-group-name>`.

PodGroups use the existing revision-aware provider naming convention:

```text
<lws-name>-<group-index>-<template-revision-hash>
```

Revision-aware names avoid collisions while old and new replicas coexist
during a rolling update. A leader restart within the same revision reuses the
same PodGroup.

Every PodGroup has:

- a controller ownerReference to the LWS, never to the leader Pod;
- a non-controller ownerReference to the Workload;
- labels for LWS name, group index, and template revision;
- `spec.workloadRef.workloadName` and `templateName: replica`;
- an inline copy of the resolved template fields.

This ownership model is required by strict ordering and PodGroup deletion
protection. A leader Pod cannot own an object that must exist before the
leader Pod. It also removes the same-name delete/recreate race during leader
restarts.

For scale-down or rollout cleanup, LWS first removes the member pods and their
workload resources. It then deletes the PodGroup and waits for the upstream
protection finalizer to complete. Deleting the entire LWS uses owner-based
garbage collection, with controller reconciliation providing ordered
best-effort cleanup.

### Replica, Size, and Rollout Updates

Replica count and rollout updates are reconciled as follows:

- **Scale up:** create the new revision-specific PodGroup before increasing
  the leader StatefulSet to expose the new group index.
- **Scale down:** stop and delete the replica's pods, then delete its
  PodGroup.
- **Rolling update:** pre-create the new revision's PodGroup before creating
  its leader. The old and new revision-specific PodGroups may coexist while
  `maxSurge` is active.
- **Leader recreation:** reuse the existing PodGroup because group index and
  revision are unchanged.

Kubernetes 1.37 makes `gang.minCount` mutable, so this KEP does not prohibit
all size changes. A `ResizePolicy: Recreate` size update is handled as a
revision transition:

1. Recompile and patch the Workload template with the new desired minimum.
2. Keep old revision PodGroups at their old minimum while their old-size pods
   still exist.
3. Create new revision PodGroups with the new minimum before creating the new
   replica pods.
4. Delete old PodGroups after their pods are gone.

This avoids raising an old PodGroup's minimum beyond its actual member count.
For any future in-place size policy, LWS must explicitly coordinate PodGroup
and pod membership updates; it cannot merely patch `minCount`.

### Parent Controller Integration

LWS follows [KEP-6089][kep6089]'s root-controller rule. The root-most
registered workload controller owns and compiles the Workload. A child LWS
must not create a second Workload.

When an LWS has a registered controller owner and the root delegates runtime
group management:

1. LWS follows the controller-owner chain and discovers the root Workload.
2. The parent supplies
   `scheduling.k8s.io/group-template-name` on the child LWS to select the
   Workload template.
3. If the LWS leaf belongs below a runtime CompositePodGroup, the parent also
   supplies `scheduling.k8s.io/parent-composite-podgroup`.
4. LWS uses `NewBuilderFromExistingWorkload` and creates its per-replica
   PodGroups from the selected persisted template.
5. The resulting PodGroups set `parentCompositePodGroupName` when the parent
   annotation is present.

The annotations are controller-to-controller linkage, not user scheduling
preferences. Validation rejects missing templates, an invalid owner chain, or
a parent CompositePodGroup that is not present. LWS blocks pod creation until
the delegated Workload and any required parent instance exist.

Initial alpha supports LWS as a delegated leaf. Compiling LWS itself into a
multi-level CompositePodGroup tree, including leader-first and per-role
policies, is future work under [KEP-6012][kep6012].

[kep6012]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/6012-composite-podgroup-api

### Unsupported Pod-Level Overrides

LWS-managed templates must not pre-set `pod.spec.schedulingGroup`. The
previous alpha escape hatch allowed an arbitrary external object name and
disabled LWS lifecycle management, but it could not establish:

- which Workload template defines the policy;
- which controller owns the Workload;
- whether the PodGroup exists before pods;
- how scaling, rollout, and finalizer cleanup are coordinated.

The standardized parent annotations are the supported delegation mechanism.
Admission rejects a pre-set `schedulingGroup` when
`spec.scheduling` is managed by LWS. A future bring-your-own-object mode, if
needed, must define ownership and reconciliation explicitly in a separate
KEP.

### Observability

Users can inspect:

- the Workload and its `controllerRef`;
- one PodGroup per active replica and its `workloadRef`;
- `PodGroup.status.conditions[type=PodGroupInitiallyScheduled]`;
- pod events and `spec.schedulingGroup`;
- LWS events and a new `WorkloadSchedulingReady` condition.

`WorkloadSchedulingReady=False` includes stable reasons for:

- `APINotAvailable`;
- `UnsupportedProviderCapability`;
- `InvalidSchedulingConfiguration`;
- `WorkloadCreateFailed`;
- `PodGroupCreateFailed`;
- `ParentWorkloadNotReady`;
- `PodGroupCleanupBlocked`.

PodGroup status remains the detailed scheduler-facing source of truth. LWS
status summarizes readiness and does not duplicate all PodGroup conditions.

### Failure Handling

Compilation and object creation are idempotent:

- existing objects are discovered through owner/controller references and
  deterministic names;
- a crash after Workload creation resumes at PodGroup creation;
- a crash after PodGroup creation resumes at pod creation;
- immutable-field drift produces an event and condition instead of deleting
  and recreating live objects automatically;
- API errors requeue with backoff and block only replicas whose scheduling
  prerequisites are incomplete.

LWS never silently removes `spec.schedulingGroup` or falls back to Basic when
Gang was requested. Such fallback could create a partially running replica
and violate the user's declared policy.

### Backwards Compatibility

- LWS objects with `spec.scheduling` absent and no legacy provider behavior
  are unchanged.
- Existing Volcano installations retain KEP-407 behavior when the new field
  is absent.
- The new field is alpha and guarded by `WorkloadAwareScheduling`, default
  `false`.
- Enabling the LWS gate alone does not change existing objects.
- Disabling the LWS gate after objects have opted in stops new compilation but
  does not mutate or orphan live objects; operators must drain or remove
  opted-in LWS objects before disabling the upstream Kubernetes gates.
- The `v1alpha2` draft has no compatibility promise. It was never a stable LWS
  API and is replaced by the Kubernetes 1.37 design before implementation.

### Risks and Mitigations

**Upstream APIs are still evolving.** Workload and PodGroup are Beta in 1.37,
but controller building blocks, Job integration, and CompositePodGroup remain
Alpha.

*Mitigation:* isolate translation in `workloadbuilder`, vendor a tested
Kubernetes dependency, and keep `spec.scheduling` behind an alpha LWS gate.

**Feature-gate skew can cause unsafe behavior.** kube-apiserver and
kube-scheduler may not have identical WAS gates.

*Mitigation:* document both components as prerequisites, verify API discovery,
block pods until runtime objects are accepted, and test skew. Never silently
fall back.

**Scheduling object cardinality grows with replicas and surge.** An LWS has
one Workload and approximately `replicas` PodGroups, temporarily more during
rollout.

*Mitigation:* use one shared template, deterministic names, owner indexes, and
filtered watches. Add scale tests before Beta.

**Deletion protection can delay rollout or scale-down.** A PodGroup cannot
disappear while member pods still reference it.

*Mitigation:* delete pods first, surface cleanup state, and treat a finalizer
wait as reconciliation progress rather than creating a conflicting object.

**Provider capabilities differ.** A policy accepted for Kubernetes may have
no faithful Volcano representation.

*Mitigation:* validate against an explicit capability set. Do not implement
lossy translation.

**Size changes can mix old and new replica shapes during rollout.**

*Mitigation:* use revision-specific PodGroups and retain the old inline policy
until the old replica is removed.

### Examples

An LWS opts into upstream gang scheduling:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: inference
spec:
  replicas: 4
  scheduling:
    schedulingPolicy:
      gang: {} # minCount defaults to size (2)
    disruptionMode:
      all: {}
  leaderWorkerTemplate:
    size: 2
    leaderTemplate:
      spec:
        containers:
        - name: leader
          image: example/leader:latest
    workerTemplate:
      spec:
        containers:
        - name: worker
          image: example/worker:latest
```

LWS compiles one Workload:

```yaml
apiVersion: scheduling.k8s.io/v1beta1
kind: Workload
metadata:
  name: inference
  ownerReferences:
  - apiVersion: leaderworkerset.x-k8s.io/v1
    kind: LeaderWorkerSet
    name: inference
    uid: 5c66068f-90af-46ad-9208-b447df8e1843
    controller: true
spec:
  controllerRef:
    apiGroup: leaderworkerset.x-k8s.io
    kind: LeaderWorkerSet
    name: inference
  podGroupTemplates:
  - name: replica
    schedulingPolicy:
      gang:
        minCount: 2
    disruptionMode:
      all: {}
```

For group index `0` and revision `dd6699c7c`, LWS instantiates:

```yaml
apiVersion: scheduling.k8s.io/v1beta1
kind: PodGroup
metadata:
  name: inference-0-dd6699c7c
  labels:
    leaderworkerset.sigs.k8s.io/name: inference
    leaderworkerset.sigs.k8s.io/group-index: "0"
    leaderworkerset.sigs.k8s.io/template-revision-hash: dd6699c7c
  ownerReferences:
  - apiVersion: leaderworkerset.x-k8s.io/v1
    kind: LeaderWorkerSet
    name: inference
    uid: 5c66068f-90af-46ad-9208-b447df8e1843
    controller: true
  - apiVersion: scheduling.k8s.io/v1beta1
    kind: Workload
    name: inference
    uid: e5399d0d-bd5e-4468-a02b-b4ea068856be
spec:
  workloadRef:
    workloadName: inference
    templateName: replica
  schedulingPolicy:
    gang:
      minCount: 2
  disruptionMode:
    all: {}
```

Both the leader and worker pods contain:

```yaml
spec:
  schedulingGroup:
    podGroupName: inference-0-dd6699c7c
```

### Test Plan

[x] I/we understand the owners of the involved components may require updates
to existing tests to make this code solid before implementation.

#### Unit Tests

- API defaulting: absent scheduling, empty scheduling, Basic, Gang, and omitted
  `minCount`.
- Validation: policy union, policy immutability, `minCount == size`, atomic
  size/minimum updates, `LeaderReady`, exclusive topology, resource-claim
  matching, and provider capabilities.
- `workloadbuilder` input generation and error-path mapping.
- Correct `v1beta1` Workload, PodGroup, `controllerRef`, `workloadRef`,
  ownerReferences, labels, and revision-aware names.
- Parent owner-chain and well-known annotation validation.
- Feature-gate-disabled and missing-API behavior.

#### Integration Tests

- Strict Workload -> PodGroup -> Pod creation order, including injected
  failures and controller restarts between steps.
- Scale up, scale down, rolling update, `maxSurge`, leader recreation, and
  whole-LWS deletion.
- `ResizePolicy: Recreate` with old and new revision PodGroups carrying their
  respective minimums.
- PodGroup deletion-protection finalizer behavior.
- Delegated leaf mode with template and parent CompositePodGroup annotations.
- Status conditions and events for invalid configuration, missing parent, and
  API errors.
- Legacy Volcano behavior when `spec.scheduling` is absent.

#### End-to-End Tests

- Kubernetes 1.37 with `GenericWorkload=true`: a complete replica schedules
  together and an incomplete replica remains pending.
- Two competing replicas do not enter the partial-scheduling deadlock.
- Autoscaling creates and removes only the corresponding PodGroups.
- A rolling update with surge never creates a pod before its revision-specific
  PodGroup.
- Optional topology and shared-claim suites run only with their required gates
  enabled.
- Gate-skew and rollback tests demonstrate that LWS blocks unsafe pod creation
  and reports an actionable condition.

### Graduation Criteria

**Alpha**

- Introduce `spec.scheduling` and `WorkloadAwareScheduling=false`.
- Add the `kubernetes` provider using `v1beta1` Workload and PodGroup.
- Use `v1alpha3` building blocks and `workloadbuilder`.
- Implement strict object ordering, LWS ownership, scaling, rollout, size
  updates, cleanup, and delegated-leaf integration.
- Add unit, integration, and opt-in e2e coverage.

**Beta**

- Gather at least two release cycles of user and operator feedback.
- Demonstrate scale and rollout behavior at supported LWS replica counts.
- Define compatibility for the Kubernetes dependency and building-block API
  graduation.
- Provide stable metrics, events, conditions, and a troubleshooting guide.
- Resolve or formally defer universal Basic Workload representation for LWS.
- Decide the LWS gate default independently of upstream maturity;
  `GenericWorkload` being Beta does not by itself justify default-on.
- Document a migration plan for the legacy implicit provider mode.

**GA**

- Depend only on stable upstream runtime and controller-integration contracts.
- Have no known data-loss, orphaning, or scheduling-safety issues across
  upgrade, downgrade, rollout, resizing, and deletion.
- Provide a supported hierarchical design for LWS integrations that require
  CompositePodGroup, or document it as a separate graduated feature.
- Remove the LWS feature gate only after the provider and API compatibility
  contracts are stable.

## Implementation History

- 2025-10-13: Initial KEP drafted against the early upstream alpha API in
  [lws#844][lws-pr-844].
- 2026-07-27: Rewritten for Kubernetes 1.37: `v1beta1` runtime APIs,
  `v1alpha3` building blocks, `workloadbuilder`, mutable `minCount`, strict
  lifecycle ordering, and KEP-6089 parent integration.

[lws-pr-844]: https://github.com/kubernetes-sigs/lws/pull/844

## Drawbacks

- The upstream path requires a non-default Kubernetes feature gate in 1.37.
- LWS creates an additional API object per active replica.
- Supporting both upstream and third-party providers increases validation and
  test complexity.
- Revision-specific PodGroups make rollouts safe but temporarily increase
  object count.
- Embedding alpha building-block types means the LWS alpha API may need to
  track upstream package changes before graduation.

## Alternatives

**Continue using only third-party schedulers.** This avoids an upstream
dependency but leaves LWS outside the standard Workload contract and requires
users to install another scheduler.

**Keep `spec.gangScheduling: {}` as an LWS-specific empty marker.** This is
smaller initially but cannot express standard policy, topology, disruption,
or shared-claim intent and diverges from KEP-6089 and Job integration.

**Create PodGroups from leader-pod reconciliation and make the leader their
owner.** This matches the existing Volcano shortcut but violates the required
Workload -> PodGroup -> Pod order and conflicts with PodGroup deletion
protection. It is rejected for the upstream provider.

**Use one PodGroup for the entire LWS.** This would require all replicas to fit
simultaneously, couples independent serving replicas, and makes autoscaling
unnecessarily disruptive.

**Create one Workload per replica.** This duplicates identical policy, creates
more objects, and loses the Workload-level representation of the LWS.

**Implement hierarchical LWS scheduling immediately.** Kubernetes 1.37 has
the necessary CompositePodGroup primitives, but they remain alpha and would
substantially expand the first implementation. The flat replica design is a
useful, independently testable foundation.
