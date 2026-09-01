# KEP-666: Workload-Aware Gang Scheduling in LWS

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Future Goals](#future-goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Kubernetes 1.37 Baseline](#kubernetes-137-baseline)
  - [User Stories](#user-stories)
  - [Scheduling Hierarchy and Phased Delivery](#scheduling-hierarchy-and-phased-delivery)
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

This KEP integrates LeaderWorkerSet (LWS) with Kubernetes 1.37
Workload-Aware Scheduling (WAS). It adds an alpha `spec.scheduling` field that
mirrors LWS structure: the whole LWS, each replica, and the leader/worker
leaves. Phase 1 materializes one `scheduling.k8s.io/v1beta1` Workload and flat
PodGroups from exactly one active level. The default is one PodGroup per
replica with `gang.minCount` equal to replica size. The same API can later
compile `CompositePodGroup` trees without changing or deprecating the LWS
field.

LWS uses `k8s.io/component-helpers/scheduling/schedulingv1/workloadbuilder`.
User-facing fields are LWS-owned API types whose initial JSON semantics align
with the Kubernetes 1.37 `scheduling.k8s.io/v1alpha3` controller building
blocks. The controller converts them at an explicit internal boundary;
kube-scheduler consumes `v1beta1` Workload and PodGroup objects. The feature
is guarded by the LWS `WorkloadAwareScheduling` gate, delivered through the
LWS Configuration API, and selected via the existing
`gangSchedulingManagement.schedulerProvider` setting. The upstream
`kubernetes` provider also requires Kubernetes `GenericWorkload` on both
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
than maintain a parallel runtime representation. The LWS-owned user API is a
versioned adapter into that contract, not a second scheduling protocol.

### Goals

- Add optional, centralized `spec.scheduling` to `LeaderWorkerSetSpec`.
- Support Basic and Gang policies, topology constraints, disruption modes,
  and shared resource claims at any Phase-1 flat level or at leader/worker
  leaves.
- Represent the LWS, replica, and leader/worker levels in that field from
  the first release, so later CompositePodGroup support does not change or
  deprecate the API.
- Make the scheduling configuration available to integrations such as Kueue,
  so they can select the appropriate workload representation and admission
  behavior.
- Adopt `workloadbuilder` for Workload/PodGroup creation and validation.
- Allow LWS to operate as a root WAS controller or as a child of another
  registered workload controller.
- Preserve the existing Volcano integration when `spec.scheduling` is
  absent, with explicit provider capabilities for the typed API.

### Future Goals

- Materialize a CompositePodGroup hierarchy (`LWS root CPG -> per-replica
  CPGs -> leader/worker PodGroups`) in the controller.

This is a future goal because CompositePodGroup is alpha in Kubernetes 1.37.
Alpha APIs are difficult to support for many users, so nested CPG creation
stays behind a separate LWS gate. The user-facing field already reserves the
hierarchy; Phase 1 only creates Workload and flat PodGroups.

### Non-Goals

- Extending this KEP's three-level LWS hierarchy with the additional role and
  slice levels needed by [DisaggregatedSet][kep766].
- Combining `startupPolicy: LeaderReady` with a gang that contains both the
  leader and workers.
- Supporting arbitrary user-managed Workload or PodGroup objects referenced
  directly from pod templates.
- Replacing provider-specific configuration such as Volcano queue annotations.
- Implementing Kueue admission or queue management. Kueue integration is
  limited to exposing scheduling configuration; a follow-up Kueue design must
  define queueing behavior.
- Guaranteeing that optional WAS capabilities are available merely because
  Workload and PodGroup discovery succeeds. Their feature gates and maturity
  are independent.

[kep407]: https://github.com/kubernetes-sigs/lws/tree/main/keps/407-gang-scheduling
[kep766]: https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet
[kep5547]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-apps/5547-integrate-workload-with-job
[kep5710]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5710-workload-aware-preemption
[kep5729]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5729-resourceclaim-support-for-workloads
[kep5732]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5732-topology-aware-workload-scheduling

## Proposal

### Kubernetes 1.37 Baseline

This KEP targets the Kubernetes 1.37 APIs, not the earlier `v1alpha2` design:

| Area | Kubernetes 1.37 state | LWS consequence |
| --- | --- | --- |
| Workload and PodGroup runtime APIs | `scheduling.k8s.io/v1beta1` | LWS creates and watches `v1beta1` objects. |
| Reusable controller API blocks | `scheduling.k8s.io/v1alpha3` | LWS-owned `spec.scheduling` structs mirror the supported semantics and are converted internally; the stable LWS `v1` API does not import, embed, or alias the alpha Go types. |
| `workloadbuilder` | Shipped in `k8s.io/component-helpers/scheduling/schedulingv1/workloadbuilder` | LWS uses the release implementation for validation, Workload compilation, and PodGroup materialization. |
| `GenericWorkload` | Beta, default `false` | Operators must explicitly enable it in kube-apiserver and kube-scheduler. |
| Gang minima (`minCount`, `minGroupCount`) | Mutable, but both must remain positive | LWS can support elastic size and replica changes; zero replicas use an unused template placeholder of `1`. |
| Workload templates | Existing entries are updateable where their fields allow it; entries cannot be added or removed | A single-level shape always uses stable leaf templates for the selected level. A multi-level shape, admitted only in Phase 2, compiles into nested composite and leaf templates. |
| PodGroup protection | PodGroups have deletion protection | LWS owns PodGroups independently of leader Pods and follows ordered cleanup. |
| Workload-aware preemption ([KEP-5710][kep5710]) | Beta behavior under `GenericWorkload`; no separate feature gate | The PodGroup priority is authoritative and every member Pod must have the same effective priority. |
| `TopologyAwareWorkloadScheduling` ([KEP-5732][kep5732]) | Alpha, default `false` in `release-1.37` | Topology constraints require a separate cluster prerequisite. The KEP targets Beta, but the 1.37 release-branch gate did not graduate. |
| `DRAWorkloadResourceClaims` ([KEP-5729][kep5729]) | Beta, default `false` | Shared claims require both DRA and WAS claim gates. |
| `PodGroupPreemptionPolicy` | Alpha, default `false` | Propagating a PriorityClass preemption policy to a PodGroup requires a separate cluster prerequisite. |
| `CompositePodGroup` runtime API | `scheduling.k8s.io/v1alpha3`; Alpha, default `false` | Runtime CPG creation is deferred, but LWS-owned fields reserve the full hierarchy and can be converted to the level-appropriate composite building blocks later. |
| Job integration ([KEP-5547][kep5547]) | `WorkloadWithJob` Alpha, default `false` | Reference semantics for controller conversion; Job's gate and direct type composition do not define the LWS `v1` compatibility contract. |

Where enhancement metadata and release code disagree, this KEP uses the
`release-1.37` types and feature-gate registry. Topology-aware workload
scheduling is not treated as Beta for LWS 1.37 compatibility.

Do not use `scheduling.k8s.io/v1alpha2` or `podGroupTemplateRef.workload`. In
`v1beta1`, a PodGroup links to its template through:

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
[runtime-types]: https://github.com/kubernetes/kubernetes/blob/release-1.37/staging/src/k8s.io/api/scheduling/v1beta1/types.go
[building-blocks]: https://github.com/kubernetes/kubernetes/blob/release-1.37/staging/src/k8s.io/api/scheduling/v1alpha3/types.go
[workloadbuilder]: https://github.com/kubernetes/kubernetes/tree/release-1.37/staging/src/k8s.io/component-helpers/scheduling/schedulingv1/workloadbuilder

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

As a platform user, I want to place every replica of an LWS in the same zone
while placing each replica within a rack, without migrating to a different LWS
API when CompositePodGroup support is enabled.

As an accelerator user, I want workers to use a topology-constrained gang and
a shared DRA claim while the lightweight leader remains a separate leaf group.

### Scheduling Hierarchy and Phased Delivery

LWS exposes scheduling intent at three structural levels:

1. **LWS (level 1):** all replicas. Examples include a whole-LWS gang,
   zone-level placement, or disruption of every replica together.
2. **Replica (level 2):** one leader and its workers. This is the default MVP
   level and maps naturally to one gang per replica with `minCount == size`.
3. **Leader/worker (level 3):** the two pod-bearing leaves within a replica.
   This allows workers to request an NVLink domain or shared DRA claim without
   forcing the leader to use the same resources. Kubernetes 1.37 still
   requires one effective priority across the entire Workload.

Delivery is split into two phases:

- **Phase 1:** LWS creates only Workload and PodGroup objects. Admission
  permits exactly one active level: whole LWS, replica, or leader/worker
  leaves. This single-level shape is always lowered to flat PodGroup
  templates. An empty `spec.scheduling` selects replica mode and Gang
  scheduling. Leader and worker leaves are admitted independently in this
  phase; coordinating them as a gang of groups requires Phase 2.
- **Phase 2:** behind a separate LWS gate and Kubernetes'
  `CompositePodGroup` gate, admission additionally permits multiple active
  levels. This multi-level shape is compiled as `LWS root CPG -> per-replica
  CPGs -> leader/worker PodGroups`. A single-level object remains flat even
  after Phase 2 is enabled; only a newly created object may opt into the
  multi-level shape because the scheduling hierarchy is immutable.

The object shape, not the controller version, creation time, enabled phase, or
an annotation/status side channel, is the persisted representation
discriminator. LWS computes it from normalized user intent before synthesizing
structural Basic ancestors or missing leaves:

1. A level is active when the user configures policy, constraints, disruption,
   or claims at that level. An explicit `leader` or `worker` block activates
   the shared role level; the enclosing `replica` pointer is only a path and
   does not by itself activate the replica level.
2. `spec.scheduling: {}` and `spec.scheduling.replica: {}` are the two special
   empty forms. Both normalize to one active replica level with replica Gang.
3. Exactly one active level means `Flat`; more than one means `Composite`.
   Representation-specific Basic nodes and leaves are synthesized only after
   this decision and therefore cannot change it.

`Composite` is admitted only when both Phase-2 gates are enabled. Updates
cannot change an object from `Flat` to `Composite` or back. Consequently a
restart, upgrade, or later Phase-2 enablement recompiles every existing
Phase-1 object to the same flat Workload templates without storing an
additional mode field.

For a Phase-2 multi-level configuration, LWS fills omitted levels with Basic
nodes or leaves. LWS and replica blocks always compile to composite nodes;
leader and worker blocks always compile to leaf PodGroups.

This follows the phased API approach in [JobSet KEP-969][jobset-kep969]: ship
flat PodGroups first while reserving the natural hierarchy for CPG
materialization. Users set policy on existing LWS structure; the controller
derives template names, instances, membership, and parent links.

[jobset-kep969]: https://github.com/kubernetes-sigs/jobset/pull/1253

### User-Facing API

LWS adds an alpha `spec.scheduling` field. Its schema consists only of types
owned by the stable `leaderworkerset.x-k8s.io/v1` API package. LWS and replica
levels use composite policy variants; leader and worker leaves use pod policy
variants, so a gang minimum counts child groups or pods according to the
level. These types intentionally use the Kubernetes 1.37 building blocks'
initial JSON spellings, but they are neither Go aliases nor embedded upstream
types:

```go
// api/leaderworkerset/v1/leaderworkerset_types.go
type LeaderWorkerSetSpec struct {
    // ... existing fields ...

    // Scheduling defines Workload-Aware Scheduling for this LWS.
    // Alpha; guarded by the WorkloadAwareScheduling feature gate.
    // +optional
    Scheduling *LeaderWorkerSetScheduling `json:"scheduling,omitempty"`
}

type LeaderWorkerSetScheduling struct {
    // SchedulingPolicy defines level-1 scheduling for all replicas in the LWS.
    // When this is the only active level, it is lowered to one flat PodGroup.
    // In a multi-level shape it configures the root CPG.
    // Immutable after creation.
    // +optional
    SchedulingPolicy *LeaderWorkerSetCompositeSchedulingPolicy `json:"schedulingPolicy,omitempty"`

    // SchedulingConstraints defines level-1 placement for all replicas.
    // Immutable after creation.
    // +optional
    SchedulingConstraints *LeaderWorkerSetSchedulingConstraints `json:"schedulingConstraints,omitempty"`

    // DisruptionMode defines how replica groups may be disrupted.
    // Immutable after creation.
    // +optional
    DisruptionMode *LeaderWorkerSetDisruptionMode `json:"disruptionMode,omitempty"`

    // ResourceClaims lists dynamic resource claims that pods across the LWS
    // may share. This is supported only while level 1 is selected as a flat
    // PodGroup. It is mutually exclusive with Replica because a root CPG
    // cannot own a pod-level resource claim; nested configurations put claims
    // on replica or role leaves.
    // Immutable after creation.
    // +optional
    // +kubebuilder:validation:MaxItems=4
    // +listType=map
    // +listMapKey=name
    ResourceClaims []LeaderWorkerSetResourceClaim `json:"resourceClaims,omitempty"`

    // Replica defines level-2 scheduling for each LWS replica.
    // +optional
    Replica *LeaderWorkerSetReplicaScheduling `json:"replica,omitempty"`
}

type LeaderWorkerSetReplicaScheduling struct {
    // SchedulingPolicy defines level-2 scheduling for a leader and its workers.
    // When this is the only active level, it is lowered to one PodGroup per
    // replica. In a multi-level shape it configures each replica CPG.
    // Immutable after creation.
    // +optional
    SchedulingPolicy *LeaderWorkerSetCompositeSchedulingPolicy `json:"schedulingPolicy,omitempty"`

    // SchedulingConstraints defines level-2 placement for each replica.
    // Immutable after creation.
    // +optional
    SchedulingConstraints *LeaderWorkerSetSchedulingConstraints `json:"schedulingConstraints,omitempty"`

    // DisruptionMode defines how the leader and worker groups may be disrupted.
    // Immutable after creation.
    // +optional
    DisruptionMode *LeaderWorkerSetDisruptionMode `json:"disruptionMode,omitempty"`

    // ResourceClaims lists dynamic resource claims shared by replica members.
    // A flat representation supports this while replica is the selected leaf. It is
    // mutually exclusive with leader and worker because a CPG cannot own a
    // pod-level resource claim; nested configurations put claims on the leaves.
    // Immutable after creation.
    // +optional
    // +kubebuilder:validation:MaxItems=4
    // +listType=map
    // +listMapKey=name
    ResourceClaims []LeaderWorkerSetResourceClaim `json:"resourceClaims,omitempty"`

    // Leader defines the level-3 leader PodGroup.
    // +optional
    Leader *LeaderWorkerSetLeaderScheduling `json:"leader,omitempty"`

    // Worker defines the level-3 worker PodGroup.
    // +optional
    Worker *LeaderWorkerSetWorkerScheduling `json:"worker,omitempty"`
}

type LeaderWorkerSetLeaderScheduling struct {
    // SchedulingPolicy defines scheduling for the leader PodGroup.
    // Immutable after creation.
    // +optional
    SchedulingPolicy *LeaderWorkerSetPodSchedulingPolicy `json:"schedulingPolicy,omitempty"`

    // SchedulingConstraints defines placement for the leader PodGroup.
    // Immutable after creation.
    // +optional
    SchedulingConstraints *LeaderWorkerSetSchedulingConstraints `json:"schedulingConstraints,omitempty"`

    // DisruptionMode defines how leader pods may be disrupted.
    // Immutable after creation.
    // +optional
    DisruptionMode *LeaderWorkerSetDisruptionMode `json:"disruptionMode,omitempty"`

    // ResourceClaims lists dynamic resource claims shared by leader pods.
    // Immutable after creation.
    // +optional
    // +kubebuilder:validation:MaxItems=4
    // +listType=map
    // +listMapKey=name
    ResourceClaims []LeaderWorkerSetResourceClaim `json:"resourceClaims,omitempty"`
}

type LeaderWorkerSetWorkerScheduling struct {
    // SchedulingPolicy defines scheduling for the worker PodGroup.
    // Immutable after creation.
    // +optional
    SchedulingPolicy *LeaderWorkerSetPodSchedulingPolicy `json:"schedulingPolicy,omitempty"`

    // SchedulingConstraints defines placement for the worker PodGroup.
    // Immutable after creation.
    // +optional
    SchedulingConstraints *LeaderWorkerSetSchedulingConstraints `json:"schedulingConstraints,omitempty"`

    // DisruptionMode defines how worker pods may be disrupted.
    // Immutable after creation.
    // +optional
    DisruptionMode *LeaderWorkerSetDisruptionMode `json:"disruptionMode,omitempty"`

    // ResourceClaims lists dynamic resource claims shared by worker pods.
    // Immutable after creation.
    // +optional
    // +kubebuilder:validation:MaxItems=4
    // +listType=map
    // +listMapKey=name
    ResourceClaims []LeaderWorkerSetResourceClaim `json:"resourceClaims,omitempty"`
}

// LeaderWorkerSetCompositeSchedulingPolicy selects the policy for a group of
// child groups. Exactly one member must be set.
type LeaderWorkerSetCompositeSchedulingPolicy struct {
    // +optional
    Basic *LeaderWorkerSetBasicSchedulingPolicy `json:"basic,omitempty"`
    // +optional
    Gang *LeaderWorkerSetCompositeGangSchedulingPolicy `json:"gang,omitempty"`
}

// LeaderWorkerSetPodSchedulingPolicy selects the policy for a group of pods.
// Exactly one member must be set.
type LeaderWorkerSetPodSchedulingPolicy struct {
    // +optional
    Basic *LeaderWorkerSetBasicSchedulingPolicy `json:"basic,omitempty"`
    // +optional
    Gang *LeaderWorkerSetPodGangSchedulingPolicy `json:"gang,omitempty"`
}

type LeaderWorkerSetBasicSchedulingPolicy struct{}

type LeaderWorkerSetCompositeGangSchedulingPolicy struct {
    // MinGroupCount is a positive minimum number of child groups.
    // +optional
    // +kubebuilder:validation:Minimum=1
    MinGroupCount *int32 `json:"minGroupCount,omitempty"`
}

type LeaderWorkerSetPodGangSchedulingPolicy struct {
    // MinCount is a positive minimum number of pods.
    // +optional
    // +kubebuilder:validation:Minimum=1
    MinCount *int32 `json:"minCount,omitempty"`
}

type LeaderWorkerSetSchedulingConstraints struct {
    // +optional
    // +kubebuilder:validation:MaxItems=1
    // +listType=atomic
    Topology []LeaderWorkerSetTopologyConstraint `json:"topology,omitempty"`
}

type LeaderWorkerSetTopologyConstraint struct {
    // Key is a qualified Kubernetes label key naming a topology domain.
    // +required
    Key string `json:"key"`
}

// LeaderWorkerSetDisruptionMode selects how members at the configured level
// may be disrupted. Exactly one member must be set.
type LeaderWorkerSetDisruptionMode struct {
    // +optional
    Single *LeaderWorkerSetSingleDisruptionMode `json:"single,omitempty"`
    // +optional
    All *LeaderWorkerSetAllDisruptionMode `json:"all,omitempty"`
}

type LeaderWorkerSetSingleDisruptionMode struct{}
type LeaderWorkerSetAllDisruptionMode struct{}

type LeaderWorkerSetResourceClaim struct {
    // Name is a DNS label unique within the group.
    // +required
    Name string `json:"name"`
    // Exactly one claim source must be set.
    // +optional
    ResourceClaimName *string `json:"resourceClaimName,omitempty"`
    // +optional
    ResourceClaimTemplateName *string `json:"resourceClaimTemplateName,omitempty"`
}
```

The nested fields select existing LWS structure. Users do not repeat replica
counts, pod templates, or runtime group instances inside `spec.scheduling`.
Leader and worker use separate Go structs even though their initial fields are
identical, so a future role-specific field can be added without changing the
other role's API.

The shared LWS-owned constraint, disruption, and claim structs deliberately
freeze only the subset used by this KEP. The active level determines whether
the controller converts them to upstream pod-group or composite-pod-group
building blocks. Adding or changing an upstream alpha field does not
automatically change the LWS CRD; support for it requires an explicit,
backward-compatible LWS API change and conversion update.

The conversion contract is:

| LWS-owned input | Flat target | Composite target |
| --- | --- | --- |
| Composite policy on LWS or replica | Convert Basic directly; lower an empty Gang to a pod Gang with LWS-derived `minCount`; reject explicit `minGroupCount` | Convert field-by-field to `WorkloadCompositePodGroupSchedulingPolicy` |
| Pod policy on leader or worker | Convert field-by-field to `WorkloadPodGroupSchedulingPolicy` | Same leaf conversion |
| Constraints and disruption | Convert to the upstream pod-group variant | Convert to the upstream composite or pod-group variant according to the materialized node |
| Resource claim | Convert to `WorkloadPodGroupResourceClaim` on the selected flat or leaf group | Convert only on a leaf; composite placement is rejected |

The alpha feature gate does not waive compatibility for JSON already stored in
the LWS `v1` CRD. LWS will not remove or incompatibly redefine these owned
fields merely because the corresponding upstream alpha type changes. A newer
controller must either keep converting the old form or introduce an additive
LWS API evolution with an explicit migration.

### Defaulting and Validation

Defaulting is controller-side so the stored LWS preserves user intent:

- `spec.scheduling` absent means the new upstream integration is disabled for
  that LWS. No upstream Workload or PodGroup is created.
- `spec.scheduling: {}` selects replica mode and defaults to
  `replica.schedulingPolicy.gang: {}`. As a single-level shape, LWS lowers
  that composite intent to one PodGroup per replica with `gang.minCount` equal
  to `spec.leaderWorkerTemplate.size`.
- The controller-specific Gang default applies only when replica is the sole
  selected level, including `spec.scheduling.replica: {}`. At selected LWS and
  role levels, an omitted policy defaults to Basic. In a composite
  representation, any structural LWS or replica ancestor with no policy also
  defaults to Basic. A flat role representation does not synthesize its
  unmaterialized replica parent. This avoids making every ancestor an implicit
  gang when the user requested only topology or disruption behavior.
- In leader/worker mode, an explicit leaf Gang defaults `minCount` to `1` for
  the leader and `size - 1` for workers. An omitted sibling leaf is synthesized
  as Basic so every pod is represented by exactly one leaf group.
- Composite `gang.minGroupCount` is meaningful only in a multi-level shape
  that creates CPGs. A flat shape accepts `gang: {}` for lowering but rejects
  an explicit `minGroupCount` rather than interpreting a group count as a pod
  count.
- In a multi-level shape, an omitted composite `minGroupCount` defaults to
  `replicas` at the LWS root and to `2` (leader and worker leaf groups) at each
  replica CPG.
  When `replicas == 0`, an omitted, controller-computed root minimum stores
  `minGroupCount: 1` instead because the API does not allow zero; an explicit
  positive value remains unchanged, and no root CPG is instantiated.
- In flat whole-LWS mode, the Gang `minCount` is `replicas * size` while
  replicas are present. When `replicas == 0`, the unused template similarly
  stores `minCount: 1`, and no PodGroup is instantiated.

The recommended `startupPolicy` combinations are:

| `startupPolicy` | Scheduling shape | Result |
| --- | --- | --- |
| `LeaderCreated` (default) | Whole-LWS or replica Gang containing leader and workers | Supported and recommended. Worker resources are created after the leader Pod is created, without waiting for it to become Ready, so the scheduler can observe the complete gang. |
| `LeaderReady` | Whole-LWS or replica Gang containing leader and workers | Rejected. The leader cannot become Ready until the gang is complete, while workers are not created until the leader is Ready. |
| `LeaderReady` | Independent role leaves: leader Basic and worker Gang | Supported. The leader can become Ready first; LWS then creates the workers, which are admitted as their own gang. |

The validating webhook enforces:

1. The LWS `WorkloadAwareScheduling` gate is enabled.
2. Each policy or disruption union at an active level selects exactly one
   variant after defaulting.
3. Normalize the two empty replica forms, then count user-active scheduling
   levels before synthesizing structural Basic nodes or leaves. Exactly one
   active level deterministically selects the flat representation; more than
   one selects the composite representation and is admitted only when both the
   LWS and Kubernetes CPG gates are enabled. Phase 1 admits only the flat
   shape.
4. The scheduling hierarchy cannot be added, removed, or switched between
   levels after LWS creation. In particular, an update cannot change the
   representation from flat to composite or back. Policy variants and
   immutable constraints cannot change. Mutable generated leaf
   `gang.minCount` values continue to follow LWS cardinality.
5. A `resourceClaims` list may apply only to a flat or leaf PodGroup. All
   shapes reject top-level `resourceClaims` combined with `replica` and replica
   `resourceClaims` combined with leader/worker leaves. A flat shape also
   rejects explicit composite `minGroupCount` and all other fields that require
   a runtime CPG.
6. In delegated flat mode, one `group-template-name` can select only one
   flat template. Whole-LWS and replica modes are supported, but
   leader/worker mode is rejected until a composite shape can select a root
   CPG and materialize both role descendants.
7. When `replicas > 0`, a flat whole-LWS gang contains `replicas * size` pods,
   a replica gang contains `size` pods, a leader gang contains one pod, and a
   worker gang contains `size - 1` pods. An explicitly configured leaf
   `minCount` must equal its complete LWS-derived membership. With
   `replicas == 0`, LWS creates no runtime group and uses the valid template
   placeholders described above rather than compiling a zero minimum.
8. Flat leader/worker mode and every multi-level configuration
   require `size >= 2`, keeping both stable leaf templates valid for the
   lifetime of the Workload.
9. A gang containing both the leader and workers is incompatible with
   `startupPolicy: LeaderReady`, because the workers do not exist when the
   leader is expected to become ready. A worker-only leaf gang remains valid.
10. Alpha rejects the combination of gang or WAS topology constraints with
   `leaderworkerset.sigs.k8s.io/exclusive-topology` until their combined
   placement and failure semantics are tested.
11. The selected scheduler provider supports every requested field and active
   level.
12. Every ResourceClaim entry has a valid name and selects exactly one of
    `resourceClaimName` or `resourceClaimTemplateName`. Shared claims have
    matching references in every member pod template that consumes them.
13. All pod templates represented by an LWS-managed Workload must use one
    Workload-wide effective `priorityClassName`, irrespective of the selected
    scheduling level. LWS copies this common value into every PodGroupTemplate
    and CompositePodGroupTemplate. Different class names are rejected even if
    their PriorityClass objects have the same numeric priority. Once
    `spec.scheduling` is set, an update that changes the effective class of any
    represented pod template is rejected because the compiled Workload
    priority fields are immutable. Mixed-role priorities are deferred until
    the upstream Workload API supports them.
14. When `spec.scheduling` is set, every pod template
    (`leaderTemplate`, `workerTemplate`, and the implicit shared template)
    must leave `spec.schedulingGroup` unset. LWS stamps
    `schedulingGroup.podGroupName` on created pods after the matching
    PodGroup exists. A user-supplied value is rejected rather than
    overwritten, because it cannot express Workload ownership, creation
    order, or revision-specific group names.

The LWS CRD schema enforces structural constraints owned by LWS, including
positive minima, topology and claim list limits, and required names. The LWS
validating webhook enforces unions, immutability, active-level shape, and the
cross-field rules above. It does not depend on declarative validation markers
from an imported alpha package.

After LWS validation, explicit conversion functions map the owned structs to
Kubernetes 1.37 `WorkloadItem`, `PodGroupData`, or `CompositePodGroupData`.
The conversion is field-by-field, not an unsafe cast or JSON round trip. LWS
then leaves `workloadbuilder.BuildOptions.DisableDeclarativeValidation` set to
`false` and calls `Builder.Validate(ctx, ValidationInput)` on create and
update. For a composite-level field lowered in a flat shape, LWS first converts
and validates a temporary `WorkloadItem` with `CompositePodGroupData`, then
converts, validates, and builds the lowered leaf item. Builder validation and
the policy/disruption allow-lists are a second deny-by-default compatibility
boundary; they do not replace LWS API validation.

`SubGroupPolicy` does not add a fourth scheduling level in this KEP. Worker
subgroups remain members of the replica PodGroup in replica mode or of the
worker PodGroup in role mode. Per-subgroup WAS policies require a separate
hierarchy extension.

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
| Gang policy at one Phase-1 level | Supported | Replica mode only |
| Workload-aware preemption | Beta through `GenericWorkload`; requires one common priority across the Workload | No typed mapping in this KEP |
| Topology constraints | Requires `TopologyAwareWorkloadScheduling` | Rejected; existing provider annotations remain available |
| Disruption mode | Supported by upstream WAS | Rejected |
| Shared ResourceClaims | Requires `DRAWorkloadResourceClaims` and DRA | Rejected |
| Nested LWS / replica / role hierarchy | Phase 2; requires `CompositePodGroup` | Rejected |
| Parent Workload delegation | Supported | Not part of this KEP |

Existing Volcano users who only configure the provider and do not set
`spec.scheduling` retain the behavior defined by KEP-407. This compatibility
mode is intentionally asymmetric. New integrations should use the typed
field; changing the legacy implicit behavior requires a separate deprecation
plan.

### API Discovery and Cluster Prerequisites

For the `kubernetes` provider, the webhook and controller reject new opt-ins
unless `scheduling.k8s.io/v1beta1` Workload and PodGroup are discoverable,
naming the missing GVR in the error.

Discovery is necessary but not sufficient: it does not prove kube-scheduler
has the same gates as kube-apiserver. Operators must enable `GenericWorkload`
on both, plus the optional gates in the 1.37 baseline table for topology,
shared claims, PodGroup preemption policy, and CompositePodGroup.

`GenericWorkload` is Beta but disabled by default in 1.37. An API rejection
or unsupported scheduler state is surfaced on the LWS; pod creation stays
blocked rather than falling back to pod-by-pod scheduling.

## Design Details

### Compiling an LWS into a Workload

When LWS is the root workload controller, a flat-shaped object selects exactly
one level and builds the following leaf templates. Phase 1 admits only this
shape, and Phase 2 continues to use it for every single-level object:

| Active level | Stable template(s) | Runtime instances | Gang minimum |
| --- | --- | --- | --- |
| LWS | `lws` | one when `replicas > 0` | `replicas * size`; template placeholder `1` at zero |
| Replica (default) | `replica` | one per active replica | `size` |
| Leader/worker | `leader`, `worker` | up to two per active replica | `1`, `size - 1` |

Role mode requires `size >= 2` and always reserves both stable templates, so a
later size update does not add a Workload template.

For the default replica mode, the integration has the following shape (error
handling omitted):

```go
items, oldItems, allErrs := flatLeafItems(lws, oldLWS)

opts := workloadbuilder.BuildOptions{
    Name:      workloadName(lws), // <name-prefix>-<hash(lws.UID)>
    Namespace: lws.Namespace,
    Owner:     metav1.NewControllerRef(lws, leaderWorkerSetGVK),
    AllowedPolicies: []workloadbuilder.SchedulingPolicyOption{
        workloadbuilder.BasicPolicy,
        workloadbuilder.GangPolicy,
    },
    AllowedDisruptionModes: []workloadbuilder.DisruptionModeOption{
        workloadbuilder.SingleMode,
        workloadbuilder.AllMode,
    },
}

for i := range items {
    builder := workloadbuilder.NewBuilder(items[i], opts)
    allErrs = append(allErrs, builder.Validate(ctx,
        workloadbuilder.ValidationInput{OldRoot: oldItems[i]})...)
}
workload, err := buildFlatWorkload(items, opts)
```

`flatLeafItems` first converts the selected LWS-owned fields, then lowers any
composite-level block to
`workloadbuilder.SchedulingConfig`: policy, topology, and disruption keep
their meaning; an LWS-specific `resourceClaims` list is attached to the
lowered leaf; explicit `minGroupCount` has already been rejected; and the
LWS-derived pod count becomes the leaf Gang minimum. Leader/worker inputs use
standard `PodGroupData` with no lowering. `buildFlatWorkload` builds each leaf
and merges templates when role mode produces two.

On create, each `OldRoot` is nil. On update it is the previous input for the
same level so the builder can check immutability. LWS-specific checks
(active-level immutability, membership, `LeaderReady`, provider capabilities,
and Workload-wide priority consistency and immutability) stay outside the
shared builder.

A multi-level shape, admitted only with Phase 2 enabled, skips the lowering and
becomes a `WorkloadItem` tree: composite nodes use
`CompositePodGroupData`, leaves use `PodGroupData`, and `Children` decides
which nodes compile to CompositePodGroup templates. Enabling Phase 2 never
changes the flat compilation path for a single-level object.

The Workload is owned by the LWS and sets `spec.controllerRef` to the LWS.
Its name is `<name-prefix>-<uid-hash>`, where the hash is derived from the LWS
UID, matching the Job pattern in [KEP-5547][kep5547]. Discovery does not use
this name: LWS finds the object through the controller ownerReference and
`spec.controllerRef`. That avoids colliding with another namespaced controller
that also creates a Workload named after its CR (for example a Job also named
`inference`), and LWS must not adopt a pre-existing object at the computed
name whose owner or `controllerRef` points elsewhere.

A flat Workload includes only the stable template set for its selected level;
a composite Workload includes the root composite template and nested
replica/role templates. One template is instantiated many times, so scaling
does not add Workload entries.

### Workload and PodGroup Lifecycle

The LWS controller, not the leader-pod controller, manages scheduling objects
in this order:

1. Compile, create, or discover the Workload.
2. In Phase 2, instantiate parent CPGs from root to leaf parent. In Phase 1
   this step is empty.
3. Instantiate every required leaf PodGroup from the persisted Workload
   template with `NewBuilderFromExistingWorkload(...).NewPodGroup(...)`.
4. Only after a pod's complete parent chain and leaf PodGroup exist, allow the
   leader StatefulSet and worker resources to create it.
5. Stamp every member pod with
   `spec.schedulingGroup.podGroupName = <pod-group-name>`.

Runtime names identify the selected level:

| Object | Name |
| --- | --- |
| Workload | `<name-prefix>-<uid-hash>` |
| Whole-LWS PodGroup or root CPG | `<name-prefix>-<uid-hash>-lws` |
| Replica PodGroup or CPG | `<name-prefix>-<uid-hash>-<group-index>-<template-revision-hash>` |
| Leader/worker PodGroup | `<name-prefix>-<uid-hash>-<group-index>-<role>-<template-revision-hash>` |

`uid-hash` is the same deterministic DNS-safe short hash of `lws.UID` used in
the Workload name. `name-prefix` truncates `lws.Name` as needed so the complete
name satisfies the target API's length limit. Reconciliation always verifies
the controller ownerReference and expected labels before reusing a computed
name; a hash collision is surfaced rather than adopted.

The UID component prevents a newly created, same-name LWS from colliding with
deletion-protected groups that belong to the previous UID. Revision-aware
suffixes avoid collisions while old and new replicas of one LWS coexist during
a rolling update. A leader restart within the same UID and revision reuses the
same replica and role groups. The whole-LWS group is stable for the lifetime of
the LWS; its gang guarantee covers initial admission, while later rolling
replacement follows normal LWS availability semantics.

Every PodGroup has:

- a controller ownerReference to the LWS, never to the leader Pod;
- labels for LWS name, active level, optional group index, role, and template
  revision;
- `spec.workloadRef.workloadName` and the selected `templateName` (`lws`,
  `replica`, `leader`, or `worker`);
- an inline copy of the resolved template fields.

The Workload is referenced through `spec.workloadRef`, not a second
ownerReference, matching `workloadbuilder.NewPodGroup`. A leader Pod cannot
own an object that must exist before the leader, and must not be the owner
across a same-name restart.

On scale-down or rollout cleanup, LWS deletes member pods first, then the
PodGroup, and waits for the protection finalizer. Deleting the LWS relies on
owner-based garbage collection, with reconciliation providing ordered
best-effort cleanup.

### Replica, Size, and Rollout Updates

Replica count and rollout updates in the default replica mode are reconciled
as follows:

- **Scale up:** create the new revision-specific PodGroup before increasing
  the leader StatefulSet to expose the new group index.
- **Scale down:** stop and delete the replica's pods, then delete its
  PodGroup.
- **Rolling update:** pre-create the new revision's PodGroup before creating
  its leader. The old and new revision-specific PodGroups may coexist while
  `maxSurge` is active.
- **Leader recreation:** reuse the existing PodGroup because group index and
  revision are unchanged.

Scaling to `replicas: 0` removes scheduling instances without deleting the
Workload:

1. Stop pod creation and delete all member pods.
2. Delete leaf PodGroups and, in Phase 2, LWS-owned parent CPGs from leaves to
   the LWS root, waiting for deletion protection at each level. A
   parent-controller-owned ancestor is not deleted.
3. Keep the Workload because templates alone do not reserve resources. Store
   `1` in any otherwise-zero controller-computed whole-LWS `minCount` or root
   `minGroupCount`; preserve an explicit positive root minimum.
4. On scale-up from zero, patch the Workload to the positive computed minimum
   before recreating the required group chain and allowing pods.

This differs from JobSet suspension: LWS has no `spec.suspend`, and scaling to
zero does not open an immutability window or require rebuilding its scheduling
definition.

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

Leader/worker mode applies the same revision transition independently to the
two leaf groups. Whole-LWS mode patches its computed minimum when replica or
size cardinality changes; because its PodGroup is already admitted, that
patch does not promise a second all-at-once admission during rolling updates.
Phase 2 uses revision-specific replica CPGs and role PodGroups, preserving the
same per-replica rollout boundary as the default mode.

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
   supplies `scheduling.k8s.io/parent-compositepodgroup`.
4. In Phase 1, LWS uses `NewBuilderFromExistingWorkload` and creates either the
   selected whole-LWS group or per-replica groups from the one persisted flat
   template. Delegated leader/worker mode is rejected because the single
   annotation cannot identify both role templates.
5. The resulting root LWS group sets `parentCompositePodGroupName` when the
   parent annotation is present; LWS then owns all internal descendant links.

The annotations are controller-to-controller linkage, not user scheduling
preferences. Validation rejects missing templates, an invalid owner chain, or
a parent CompositePodGroup that is not present. LWS blocks pod creation until
the delegated Workload and any required parent instance exist.

In Phase 2, `group-template-name` selects the LWS root composite template. LWS
then materializes that root CPG and its replica and role descendants, so one
annotation is sufficient for delegated leader/worker mode.

The two annotation spellings above follow the Kubernetes 1.37 KEP-6089
controller-linkage proposal and its implementation-sync update. The latter is
still under review in [kubernetes/enhancements#6244][kep6089-sync] as of
2026-08-12, and neither key is exported by Kubernetes code yet. LWS must
consume upstream exported constants if they are added and revalidate both
literals before implementation rather than maintaining private spellings.

[kep6089-sync]: https://github.com/kubernetes/enhancements/pull/6244

Materializing LWS's internal hierarchy as a nested CompositePodGroup tree is
Phase 2 work under [KEP-6012][kep6012].

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
- the PodGroups selected by the active Phase-1 level and their `workloadRef`;
- in Phase 2, each CPG's parent link and the complete root-to-leaf chain;
- `PodGroup.status.conditions[type=PodGroupInitiallyScheduled]`;
- pod events and `spec.schedulingGroup`;
- LWS events and a new `WorkloadSchedulingReady` condition.

`WorkloadSchedulingReady=False` includes stable reasons for:

- `APINotAvailable`;
- `UnsupportedProviderCapability`;
- `InvalidSchedulingConfiguration`;
- `WorkloadCreateFailed`;
- `CompositePodGroupCreateFailed`;
- `PodGroupCreateFailed`;
- `ParentWorkloadNotReady`;
- `PodGroupCleanupBlocked`.

PodGroup status remains the detailed scheduler-facing source of truth. LWS
status summarizes readiness and does not duplicate all PodGroup conditions.

### Failure Handling

Compilation and object creation are idempotent:

- existing objects are discovered through owner/controller references and
  deterministic names, never by assuming the Workload is named after the LWS;
- a Workload already present at the computed name with a different owner or
  `controllerRef` is not adopted; LWS sets `WorkloadCreateFailed` and blocks
  pods;
- a crash after Workload creation resumes at root CPG or PodGroup creation;
- a crash after parent CPG creation resumes at its next descendant;
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
- Enabling Phase 2 does not rewrite a Phase-1 object's flat layout: its one
  active level continues to select `Flat` on every reconciliation.
- Disabling the LWS gate after objects have opted in stops new compilation but
  does not mutate or orphan live objects; operators must drain opted-in LWS
  objects before disabling the upstream Kubernetes gates.
- The unpublished `v1alpha2` LWS draft has no compatibility promise.

### Risks and Mitigations

**Upstream APIs are still evolving.** Workload and PodGroup are Beta in 1.37,
but controller building blocks, Job integration, and CompositePodGroup remain
Alpha. The implementation-sync update for KEP-6089 is still open even though
the corresponding library is already present in `release-1.37`.

*Mitigation:* keep the persisted LWS schema owned by LWS and isolate upstream
types in field-by-field conversion functions. Vendor a tested Kubernetes 1.37
dependency, use release-branch source as the conversion target, and require an
explicit backward-compatible LWS API decision before exposing a newly added or
changed upstream field. The topology-aware workload KEP targets Beta for 1.37,
but the `release-1.37` registry still marks
`TopologyAwareWorkloadScheduling` Alpha; compatibility claims follow the
release branch.

**Feature-gate skew can cause unsafe behavior.** kube-apiserver and
kube-scheduler may not have identical WAS gates.

*Mitigation:* document both components as prerequisites, verify API discovery,
block pods until runtime objects are accepted, and test skew. Never silently
fall back.

**Mixed member priorities are rejected by the 1.37 Workload API.** Kubernetes
requires all PodGroup and CompositePodGroup templates in one Workload tree to
have identical effective `priorityClassName` and priority values, even when
leader and worker use separate leaves.

*Mitigation:* derive one effective priority from all represented LWS pod
templates, reject any mismatch at admission, and reject later pod-template
updates that would change it. Copy the common class into every generated
template and cover both create and update validation in tests.

**Scheduling object cardinality grows with replicas, role leaves, and surge.**
The default mode has approximately `replicas` PodGroups; role mode has up to
twice that number, and Phase 2 also adds CPGs.

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

**Flat lowering must not redefine composite counts.** A CPG Gang counts
child groups, while a PodGroup Gang counts pods.

*Mitigation:* a flat shape accepts an empty composite Gang as intent but
rejects an explicit `minGroupCount`; LWS computes the flat leaf `minCount` from
its own structure. A multi-level shape converts the LWS-owned composite policy
to the upstream composite type without lowering it.

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
    replica:
      schedulingPolicy:
        gang: {} # Phase 1 PodGroup minCount defaults to size (2)
      disruptionMode:
        all: {}
  leaderWorkerTemplate:
    size: 2
    leaderTemplate:
      spec:
        priorityClassName: inference-high
        # ...
    workerTemplate:
      spec:
        priorityClassName: inference-high
        # ...
```

LWS compiles one Workload:

```yaml
apiVersion: scheduling.k8s.io/v1beta1
kind: Workload
metadata:
  name: inference-b7c8d2f1 # <name-prefix>-<uid-hash>
  labels:
    leaderworkerset.sigs.k8s.io/name: inference
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
    priorityClassName: inference-high
```

For LWS UID hash `b7c8d2f1`, group index `0`, and revision `dd6699c7c`, LWS
instantiates:

```yaml
apiVersion: scheduling.k8s.io/v1beta1
kind: PodGroup
metadata:
  name: inference-b7c8d2f1-0-dd6699c7c
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
spec:
  workloadRef:
    workloadName: inference-b7c8d2f1
    templateName: replica
  schedulingPolicy:
    gang:
      minCount: 2
  disruptionMode:
    all: {}
  priorityClassName: inference-high
```

Both the leader and worker pods contain:

```yaml
spec:
  schedulingGroup:
    podGroupName: inference-b7c8d2f1-0-dd6699c7c
```

With Phase 2 enabled, the same API can express multiple levels without a new
field or migration:

```yaml
spec:
  scheduling:
    # Level 1: keep all replica groups in one zone.
    schedulingConstraints:
      topology:
      - key: topology.kubernetes.io/zone
    replica:
      # Level 2: coordinate the leader and worker leaf groups in each replica.
      schedulingPolicy:
        gang: {}
      schedulingConstraints:
        topology:
        - key: topology.kubernetes.io/rack
      disruptionMode:
        all: {}
      worker:
        # Level 3: workers need a high-bandwidth domain and shared claim.
        schedulingPolicy:
          gang: {}
        schedulingConstraints:
          topology:
          - key: nvidia.com/nvlink-domain
        resourceClaims:
        - name: imex-channel
          resourceClaimTemplateName: imex-template
```

LWS compiles this as one root CPG, one child CPG per replica, and leader and
worker PodGroups below each replica CPG. The omitted leader leaf is synthesized
with Basic policy. Phase-1 admission rejects this manifest because it activates
more than one scheduling level.

### Test Plan

[x] I/we understand the owners of the involved components may require updates
to existing tests to make this code solid before implementation.

#### Unit Tests

- API defaulting: absent scheduling, empty scheduling to replica Gang, and
  defaults for whole-LWS, replica, and the distinct leader and worker leaf
  structs, including valid whole-LWS/root template placeholders when
  `replicas == 0`.
- LWS-owned API schema and field-by-field conversion tests for every supported
  policy, constraint, disruption mode, and resource-claim source. Golden tests
  pin the generated CRD schema independently from the Kubernetes dependency.
- Validation: policy unions, active-level and policy immutability, Phase-1
  level mutual exclusion, explicit composite `minGroupCount` rejection,
  computed leaf membership, all `startupPolicy` combinations above, exclusive
  topology, whole-LWS/replica/role resource-claim placement and matching,
  Workload-wide priority equality and update immutability, pre-set
  `pod.spec.schedulingGroup`, delegated Phase-1 role-mode rejection, and
  provider capabilities.
- Phase-1 lowering and `workloadbuilder` input generation for LWS, replica,
  and leader/worker modes, including precise error-path mapping.
- `workloadbuilder.Validate` with create/update `ValidationInput`, declarative
  validation enabled, and explicit policy/disruption allow-lists.
- Correct `v1beta1` Workload, selected leaf templates, PodGroups,
  `controllerRef`, `workloadRef`, controller ownerReferences, labels,
  common Workload-wide priority class, and bounded UID-hashed Workload,
  PodGroup, and CPG names with level-aware revision suffixes. Rejection of a
  computed name owned by another controller.
- Phase-2 `WorkloadItem` tree generation maps LWS and replica fields through
  `CompositePodGroupData` and role leaves through `PodGroupData`.
- Representation selection is a pure function of normalized user intent: one
  active level remains flat and multiple active levels remain composite across
  controller restart, dependency update, and Phase-2 gate enablement.
- Parent owner-chain and well-known annotation validation.
- Feature-gate-disabled and missing-API behavior.

#### Integration Tests

- Strict Workload -> PodGroup -> Pod creation order in all three Phase-1
  modes, including injected failures and controller restarts between steps.
- Scale up, scale down, rolling update, `maxSurge`, leader recreation, and
  whole-LWS deletion in default replica mode; role mode covers both leaves.
- Scale to and from `replicas: 0` in every flat mode and Phase 2: no runtime
  group remains at zero, no template contains a zero minimum, the Workload is
  retained, and its positive minimum is restored before any group or pod.
- `ResizePolicy: Recreate` with old and new revision PodGroups carrying their
  respective minimums.
- PodGroup deletion-protection finalizer behavior.
- Delete and recreate an LWS with the same namespace/name while an old
  deletion-protected group remains, verifying UID-qualified runtime names and
  refusal to adopt an object owned by the previous UID.
- Delegated whole-LWS and replica modes with the template annotation in Phase
  1, explicit rejection of delegated role mode in Phase 1, and delegated root
  CPG plus role descendants in Phase 2.
- Status conditions and events for invalid configuration, missing parent, and
  API errors.
- Legacy Volcano behavior when `spec.scheduling` is absent.
- With Phase 2 enabled, strict Workload -> root CPG -> replica CPG -> role
  PodGroup -> Pod ordering and restart recovery at each boundary.

#### End-to-End Tests

- Kubernetes 1.37 with `GenericWorkload=true`: a complete replica schedules
  together and an incomplete replica remains pending.
- `LeaderCreated` allows a complete replica Gang to form, while `LeaderReady`
  with a Basic leader and worker Gang starts the leader before admitting the
  workers.
- Common-priority members participate in workload-aware preemption. A mixed
  priority LWS is rejected in whole-LWS, replica, and separate role-leaf modes,
  and updates cannot change the effective priority while scheduling is set.
- Two competing replicas do not enter the partial-scheduling deadlock.
- Autoscaling creates and removes only the corresponding PodGroups.
- A rolling update with surge never creates a pod before its revision-specific
  PodGroup.
- Optional topology and whole-LWS/replica/role shared-claim suites run only
  with their required gates enabled.
- Gate-skew and rollback tests demonstrate that LWS blocks unsafe pod creation
  and reports an actionable condition.
- Upgrade an existing single-level object to a Phase-2-capable controller and
  verify that its Workload templates and runtime object kinds remain flat.

### Graduation Criteria

**Alpha**

- Introduce `spec.scheduling` and the versioned
  `WorkloadAwareScheduling=false` gate through the LWS Configuration API.
- Add the `kubernetes` provider using `v1beta1` Workload and PodGroup.
- Introduce the three-level LWS-owned scheduling API, explicit conversion to
  the Kubernetes 1.37 building blocks, and flat-shape mutual-exclusion
  validation.
- Use `workloadbuilder` to lower one active level to flat PodGroups.
- Implement strict object ordering, LWS ownership, scaling, rollout, size
  updates, cleanup, and delegated flat-template integration.
- Add unit, integration, and opt-in e2e coverage.

**Alpha 2 (CompositePodGroup)**

- Add a separate, default-off LWS gate for nested scheduling.
- Compile multi-level configurations into root and replica CPGs with
  leader/worker PodGroup leaves, without changing `spec.scheduling`.
- Implement hierarchical lifecycle, status, scale, rollout, and e2e coverage.

**Beta**

- Gather at least two release cycles of user and operator feedback.
- Demonstrate scale and rollout behavior at supported LWS replica counts.
- Validate the LWS-owned conversion contract against the supported Kubernetes
  dependencies and building-block API graduation.
- Provide stable metrics, events, conditions, and a troubleshooting guide.
- Resolve or formally defer universal Basic Workload representation for LWS.
- Decide the LWS gate default independently of upstream maturity;
  `GenericWorkload` being Beta does not by itself justify default-on.
- Document a migration plan for the legacy implicit provider mode.
- Demonstrate that single-level objects remain flat and valid after Alpha-2 or
  Beta upgrades, independent of when they were created.

**GA**

- Depend only on stable upstream runtime and controller-integration contracts.
- Have no known data-loss, orphaning, or scheduling-safety issues across
  upgrade, downgrade, rollout, resizing, and deletion.
- Provide supported CompositePodGroup materialization for the hierarchy
  already represented by the LWS API, or graduate it as a separately gated
  feature.
- Remove the LWS feature gate only after the provider and API compatibility
  contracts are stable.

## Implementation History

- 2025-10-13: Initial KEP drafted against the early upstream alpha API in
  [lws#844][lws-pr-844].
- 2026-07-27: Rewritten for Kubernetes 1.37: `v1beta1` runtime APIs,
  `v1alpha3` building blocks, `workloadbuilder`, mutable `minCount`, strict
  lifecycle ordering, and KEP-6089 parent integration.
- 2026-08-12: Revalidated against `release-1.37` and `v1.37.0-rc.0`; aligned
  with the final builder validation shape, corrected
  `DRAWorkloadResourceClaims` to Beta, clarified PodGroup ownership and common
  priority, retained topology-aware scheduling as Alpha based on the release
  feature registry, and updated the provisional parent linkage annotation.
- 2026-08-26: Reworked `spec.scheduling` to represent the LWS, replica, and
  leader/worker levels from the first release; defined flat Phase-1 lowering
  and forward-compatible Phase-2 CompositePodGroup compilation.
- 2026-08-31: Added flat whole-LWS ResourceClaims, made priority consistency
  and immutability Workload-wide, documented startup and delegated-role
  compatibility, split leader/worker API structs, and defined zero-replica and
  UID-qualified runtime-object lifecycle behavior.
- 2026-09-01: Made object shape the durable flat/composite discriminator and
  replaced upstream alpha types in the stable LWS API with LWS-owned structs
  plus an explicit conversion boundary.

[lws-pr-844]: https://github.com/kubernetes-sigs/lws/pull/844

## Drawbacks

- The upstream path requires a non-default Kubernetes feature gate in 1.37.
- Each replica (and each role leaf or CPG in later modes) adds scheduling
  objects, and revision-qualified objects increase that count during surge.
- Supporting both upstream and third-party providers increases validation and
  test complexity.
- LWS must maintain and test explicit conversion code while upstream alpha
  building blocks evolve; new upstream fields are not exposed automatically.
- The flat path must maintain a lowering adapter because LWS and replica
  policies express group-of-groups intent while the controller materializes
  leaf PodGroups.

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

**Make one PodGroup for the entire LWS the only representation.** Whole-LWS
mode is useful when explicitly requested, but making it the default would
require all replicas to fit simultaneously, couple independent serving
replicas, and hide replica and role boundaries from future CPG and topology
scheduling.

**Create one Workload per replica.** This duplicates identical policy, creates
more objects, and loses the Workload-level representation of the LWS.

**Expose only a flat replica scheduling block and add hierarchy later.** Adding
LWS and leader/worker levels later would change the field's meaning or require
a second API.

**Materialize hierarchical LWS scheduling immediately.** CompositePodGroup is
still alpha in 1.37 and would expand the first implementation. The hierarchical
API with Phase-1 lowering is a smaller, testable first step.

**Embed the upstream `v1alpha3` controller building blocks directly in the LWS
`v1` Go types.** This removes explicit conversion code, but it also couples the
persisted stable CRD schema to an alpha dependency that may remove or redefine
fields. The LWS feature gate cannot waive compatibility for existing stored
objects. LWS-owned structs with field-by-field conversion are selected instead.

**Drive WAS from LWS labels instead of a typed `spec.scheduling` field.** 
Well-known labels on the LWS (and optionally on
leader/worker templates) could opt into replica gang, topology, or disruption
behavior so the controller still compiles Workload and PodGroup objects
with an even smaller typed API surface. The selected design already avoids
embedding pre-GA `scheduling.k8s.io` types by owning the LWS schema, so labels
do not provide an additional compatibility boundary. They are not the primary
API because labels cannot represent the
LWS/replica/leader-worker hierarchy, union-typed policy, topology lists,
disruption modes, or shared claims without an unbounded stringly-typed
vocabulary, and validation and immutability become ad hoc. A later switch from
labels to a typed field would still be a user-facing migration.
