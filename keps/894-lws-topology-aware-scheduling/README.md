# KEP-894: [LeaderWorkerSet] Topology-Aware Scheduling with Workload API

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Rack-Local Multi-Node Inference](#story-1-rack-local-multi-node-inference)
    - [Story 2: Subgroup Topology Isolation](#story-2-subgroup-topology-isolation)
  - [Notes and Constraints](#notes-and-constraints)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API Changes](#api-changes)
    - [SchedulingConstraints](#schedulingconstraints)
    - [LeaderWorkerSetSpec Extension](#leaderworkersetspec-extension)
  - [PodGroup Propagation](#podgroup-propagation)
  - [Fallback: TopologySpreadConstraints](#fallback-topologyspreadconstraints)
  - [Backward Compatibility: ExclusiveTopology Annotation](#backward-compatibility-exclusivetopology-annotation)
  - [Interaction with StartupPolicy](#interaction-with-startuppolicy)
  - [Feature Gate](#feature-gate)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Custom TopologyConstraint with Strength field](#alternative-1-custom-topologyconstraint-with-strength-field)
  - [Alternative 2: nodeAffinity / podAffinity only](#alternative-2-nodeaffinity--podaffinity-only)
  - [Alternative 3: ExclusiveTopology annotation as sole mechanism](#alternative-3-exclusivetopology-annotation-as-sole-mechanism)
<!-- /toc -->

## Summary

This KEP designs how `LeaderWorkerSet` expresses topology-aware scheduling (TAS) intent
by aligning with the upstream Workload Aware Scheduling (WAS) API from
[KEP-6089](https://github.com/kubernetes/enhancements/issues/6089) and
[KEP-5732](https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5732-topology-aware-workload-scheduling).

Rather than introducing a custom LWS-specific topology API, this KEP reuses the upstream
`schedulingConstraints.topology` building blocks from `scheduling.k8s.io`. Each LWS
replica's pods are expressed as a `PodGroup` with `schedulingConstraints` that the
scheduler's `TopologyPlacement` plugin can evaluate.

Until the upstream API stabilises, the controller provides a fallback that injects
`topologySpreadConstraints` directly on the pod template.

This is the LWS-level counterpart of KEP-892, which addresses topology awareness for
`DisaggregatedSet`.

## Motivation

Multi-node inference services running on LWS are sensitive to topology. Leader/worker
communication and collective operations (AllReduce, AllGather) suffer when pod pairs span
slow inter-rack or inter-block boundaries.

Existing mechanisms and their limitations:

| Mechanism | Limitation |
|-----------|-----------|
| `nodeAffinity` | Per-pod; does not express group-level placement |
| `podAntiAffinity` | Spreads pods; cannot force colocation within a domain |
| `leaderworkerset.sigs.k8s.io/exclusive-topology` label | Annotation-based; scheduler-specific; LWS-proprietary |
| `topologySpreadConstraints` | Static per-pod; does not communicate group capacity requirements |

The upstream Workload/PodGroup `schedulingConstraints` API solves these limitations by
giving the scheduler visibility into the collective resource picture of the entire group
before any pod is placed. LWS should track and integrate with this API so users get
first-class, standards-aligned topology placement.

### Goals

1. Add a `schedulingConstraints` field to `LeaderWorkerSetSpec` that aligns with the
   upstream `scheduling.k8s.io` API shape.
2. When the upstream WAS PodGroup API is available (feature gate `LeaderWorkerSetTAS`
   enabled), the pod-controller creates one `PodGroup` per replica carrying the
   `schedulingConstraints` from the LWS spec.
3. Fall back to `topologySpreadConstraints` on the pod template when the upstream API is
   not installed.
4. Retain backward compatibility with the existing
   `leaderworkerset.sigs.k8s.io/exclusive-topology` annotation by deriving it
   automatically from the first entry in `schedulingConstraints.topology`.
5. Subgroup-level topology constraints are deferred to beta (tracked in issue #859).

### Non-Goals

1. Implementing or modifying the upstream Workload / PodGroup scheduler API.
2. Cross-cluster placement.
3. Dynamic rescheduling of live pods.
4. DisaggregatedSet topology — covered by KEP-892.
5. Subgroup topology at alpha (deferred, tracked separately in issue #859).

## Proposal

Add a `schedulingConstraints` field to `LeaderWorkerSetSpec` using the same structure as
the upstream `scheduling.k8s.io` API. The controller uses this field to:

- **At alpha (fallback path)**: inject `topologySpreadConstraints` on the leader and
  worker pod templates so the default scheduler attempts to place each replica within a
  single topology domain.
- **At beta (native path)**: create `PodGroup` objects per replica with
  `schedulingConstraints.topology` populated from the LWS spec, enabling the
  `TopologyPlacement` scheduler plugin to enforce group-level placement.

### User Stories

#### Story 1: Rack-Local Multi-Node Inference

As a platform engineer, I run LWS with `replicas: 4` and `size: 8` (one leader + 7
workers per replica). I want each replica's 8 pods to land on nodes within the same rack
to avoid slow inter-rack AllReduce.

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: llama-inference
spec:
  replicas: 4
  leaderWorkerTemplate:
    size: 8
    leaderTemplate: { ... }
    workerTemplate: { ... }
  schedulingConstraints:
    topology:
      - key: topology.kubernetes.io/rack
```

Each replica's 8 pods will be placed in the same rack domain. In the fallback path
(alpha), this becomes a `DoNotSchedule` `TopologySpreadConstraint`; in the native path
(beta), a `PodGroup` with `schedulingConstraints.topology` carries this intent.

#### Story 2: Subgroup Topology Isolation

*(Deferred to beta, tracked in issue #859.)*

As a platform engineer, I run an LWS with subgroups where each subgroup of 4 workers
acts as an NVLink island. I want each subgroup to stay within the same node while
inter-subgroup traffic is expected to cross nodes.

### Notes and Constraints

- At alpha the fallback path provides a best-effort placement hint via
  `topologySpreadConstraints`. It does not guarantee group-level scheduling atomicity
  because the default scheduler evaluates pods individually.
- True group-level TAS atomicity requires the upstream `PodGroup` TAS API (KEP-5732 /
  KEP-6089). Until that API reaches beta, the native integration path is unavailable.
- The `schedulingConstraints.topology` API (as of `scheduling.k8s.io/v1alpha2`) accepts
  one topology key per `PodGroup`. Multiple keys in the list may be supported in later
  alpha revisions; LWS will track the upstream API.

### Risks and Mitigations

**Risk 1**: Upstream WAS API changes shape before beta.

*Mitigation*: The native path is gated behind `LeaderWorkerSetTAS` (disabled by default
at alpha). The fallback is always available and produces correct behaviour for most cases.

**Risk 2**: Generated `topologySpreadConstraints` conflict with user-defined ones.

*Mitigation*: The controller **appends** generated constraints after user-defined ones.
Duplicate keys are detected and rejected by the webhook.

**Risk 3**: Strict topology colocation makes replicas permanently unschedulable on dense
clusters.

*Mitigation*: Documentation clarifies that `topologySpreadConstraints` with
`whenUnsatisfiable: DoNotSchedule` in the fallback path may cause replicas to remain
pending on undersized clusters. The feature gate default is `false` at alpha.

## Design Details

### API Changes

#### SchedulingConstraints

LWS adopts the upstream `scheduling.k8s.io` struct shapes:

```go
// SchedulingConstraints defines group-level scheduling requirements for each LWS replica.
// It aligns with the upstream scheduling.k8s.io API from KEP-6089 / KEP-5732.
type SchedulingConstraints struct {
    // Topology defines the topology domains within which the replica's pods must be placed.
    // +optional
    Topology []TopologyConstraint `json:"topology,omitempty"`
}

// TopologyConstraint specifies a single topology placement requirement.
type TopologyConstraint struct {
    // Key is the node label key that identifies the topology domain.
    // Examples: "topology.kubernetes.io/rack", "topology.kubernetes.io/zone".
    // +kubebuilder:validation:MinLength=1
    // +required
    Key string `json:"key"`
}
```

Note: unlike the earlier provisional design, there is **no `strength` field**. The
upstream `scheduling.k8s.io` `schedulingConstraints.topology` does not carry a
Required/Preferred knob — placement is always attempted within the specified domain.
In the fallback path, `topologySpreadConstraints` always uses
`whenUnsatisfiable: DoNotSchedule` (equivalent to "Required") because the semantic
intent of listing a topology key is to colocate pods.

#### LeaderWorkerSetSpec Extension

```go
type LeaderWorkerSetSpec struct {
    // ... existing fields unchanged ...

    // SchedulingConstraints defines group-level scheduling requirements for each replica.
    // When set, pods in the same replica are scheduled within the same topology domain.
    // Aligns with the upstream scheduling.k8s.io WAS API (KEP-6089).
    // +optional
    SchedulingConstraints *SchedulingConstraints `json:"schedulingConstraints,omitempty"`
}
```

### PodGroup Propagation

When `LeaderWorkerSetTAS=true` and the upstream `PodGroup` CRD is installed, the pod-
controller's `CreatePodGroupIfNotExists` method (KEP-407 path) creates one `PodGroup` per
leader pod, carrying the topology constraints from the LWS spec:

```go
func (p *WorkloadProvider) CreatePodGroupIfNotExists(
    ctx context.Context,
    lws *leaderworkerset.LeaderWorkerSet,
    leaderPod *corev1.Pod,
) error {
    // ... existing group name / owner-ref / minCount logic from KEP-407 ...

    // Propagate topology constraints if specified
    if sc := lws.Spec.SchedulingConstraints; sc != nil {
        pg.Spec.SchedulingConstraints = buildPodGroupSchedulingConstraints(sc)
    }

    return p.client.Create(ctx, &pg)
}
```

The resulting `PodGroup` looks like:

```yaml
apiVersion: scheduling.k8s.io/v1alpha2
kind: PodGroup
metadata:
  name: llama-inference-0        # lws.Name + groupIndex
  ownerReferences:
    - kind: Pod                   # leader pod (lifecycle from KEP-407)
spec:
  schedulingPolicy:
    gang:
      minCount: 8                 # lws.Spec.LeaderWorkerTemplate.Size
  schedulingConstraints:
    topology:
      - key: topology.kubernetes.io/rack
```

### Fallback: TopologySpreadConstraints

When `LeaderWorkerSetTAS=false` (default at alpha) or the PodGroup CRD is absent, the
mutating pod webhook injects `topologySpreadConstraints` for each entry in
`schedulingConstraints.topology` into both leader and worker pod templates:

```go
func buildTopologySpreadConstraint(
    tc leaderworkerset.TopologyConstraint,
    lws *leaderworkerset.LeaderWorkerSet,
) corev1.TopologySpreadConstraint {
    return corev1.TopologySpreadConstraint{
        MaxSkew:           1,
        TopologyKey:       tc.Key,
        WhenUnsatisfiable: corev1.DoNotSchedule,
        LabelSelector: &metav1.LabelSelector{
            MatchLabels: map[string]string{
                leaderworkerset.SetNameLabelKey:    lws.Name,
                leaderworkerset.GroupIndexLabelKey: pod.Labels[leaderworkerset.GroupIndexLabelKey],
            },
        },
    }
}
```

The `GroupIndexLabelKey` label selector ensures the constraint applies only to pods in
the same replica group.

### Backward Compatibility: ExclusiveTopology Annotation

LWS already supports the proprietary
`leaderworkerset.sigs.k8s.io/exclusive-topology: <key>` annotation on the LWS object.
This annotation is retained for backward compatibility.

When `schedulingConstraints.topology` is set, the controller automatically derives the
`exclusive-topology` annotation from `topology[0].key` if the annotation is not already
present. This ensures schedulers that understand the annotation continue to work without
user changes.

If the annotation is set explicitly on the LWS object alongside `schedulingConstraints`,
the explicit annotation value takes precedence.

### Interaction with StartupPolicy

| StartupPolicy | Effect on TAS |
|---------------|---------------|
| `LeaderCreated` (default) | All pods in a replica must be placed together (`minCount = size`). The topology constraint applies to the whole replica group. |
| `LeaderReady` | Leader pod is admitted first with `minCount = 1`. The topology constraint is still set on the `PodGroup` so the scheduler reserves capacity for worker pods before scheduling the leader. |

### Feature Gate

```go
// LeaderWorkerSetTAS enables PodGroup topology-aware scheduling integration
// for LeaderWorkerSet. Requires the upstream PodGroup CRD (KEP-5732 / KEP-6089)
// to be installed in the cluster.
LeaderWorkerSetTAS featuregate.Feature = "LeaderWorkerSetTAS"
```

Defaults to `false` at alpha.

### Test Plan

#### Unit tests

- Webhook: `schedulingConstraints.topology` with empty key rejected.
- Webhook: duplicate topology keys in `topology` list rejected.
- Fallback injection: leader and worker pod templates both receive the generated
  `topologySpreadConstraint` for each topology key.
- Backward compat: `exclusive-topology` annotation auto-derived from first topology key
  when not set explicitly.
- PodGroup generation: correct `minCount` and `schedulingConstraints.topology` fields.

#### Integration tests

- LWS with rack topology constraint + feature gate disabled: pods carry
  `DoNotSchedule` `TopologySpreadConstraint` on the correct topology key.
- Rolling update: updated revision pods carry updated constraints; old pods retain old
  constraints until replaced.
- Scale-up: new replica groups receive the same constraint as existing ones.
- Feature gate enabled: `PodGroup` created with `schedulingConstraints.topology`.

#### e2e tests

- (Deferred until upstream KEP-5732 / KEP-6089 APIs reach beta.)
- Smoke test on a topology-labelled kind cluster: replicas land within the same topology
  domain when capacity allows.

### Graduation Criteria

**Alpha (v0.9)**:
- `schedulingConstraints` field added to `LeaderWorkerSetSpec`.
- Fallback path (TopologySpreadConstraints on pod template) implemented and tested.
- `LeaderWorkerSetTAS` feature gate added (disabled by default).
- Backward compat: auto-derivation of `exclusive-topology` annotation.
- Webhook validation for empty/duplicate topology keys.
- Unit and integration coverage > 80%.
- Documentation and example manifests.

**Beta (v1.0)**:
- Upstream PodGroup TAS API (KEP-5732 / KEP-6089) reaches beta.
- Native integration path implemented via `WorkloadProvider` in the pod-controller.
- Feature gate enabled by default.
- e2e coverage with a reference topology-aware scheduler.

**Stable (v1.1)**:
- Feature gate removed.
- Subgroup-level constraints promoted from beta (issue #859).
- Full e2e coverage.

## Implementation History

- 2026-07-08: Initial KEP draft revised to align with upstream WAS/KEP-6089 `schedulingConstraints.topology` API.

## Drawbacks

1. **Two code paths**: Maintaining both the fallback and native paths until KEP-5732 /
   KEP-6089 stabilises doubles the test surface and increases the risk of subtle
   divergence.

2. **Constraint injection in webhook**: Injecting `topologySpreadConstraints` in the
   mutating webhook means pod templates silently gain extra constraints not visible in the
   LWS spec. This can surprise operators who inspect pod specs directly.

## Alternatives

### Alternative 1: Custom TopologyConstraint with Strength field

We initially considered a custom `TopologyConstraint{Key, Strength}` struct with
`Strength: Required|Preferred` to express strictness semantics.

**Rejected because**:
- Introduces LWS-specific scheduling primitives that duplicate or conflict with upstream
  WAS/TAS specifications (KEP-6089/KEP-5732).
- The upstream `scheduling.k8s.io` `schedulingConstraints.topology` does not have a
  strength field; adding one creates an incompatible surface that cannot be mapped
  one-to-one to the upstream PodGroup API.
- In the fallback path, the distinction between Required/Preferred maps to
  `DoNotSchedule`/`ScheduleAnyway` in `topologySpreadConstraints`. However, this
  difference becomes irrelevant once the native PodGroup path is available because
  `schedulingConstraints.topology` enforces strict colocation by design.

### Alternative 2: nodeAffinity / podAffinity only

Users can author `nodeAffinity` or `podAffinity` rules in `leaderWorkerTemplate` today.

**Rejected because**: These are per-pod hints evaluated individually by the scheduler.
The scheduler cannot guarantee that all pods in a replica will fit within the same domain
before starting placement. This leads to partial scheduling and resource deadlocks.

### Alternative 3: ExclusiveTopology annotation as sole mechanism

**Rejected because**: The annotation is proprietary, annotation-based, and lacks a
well-defined API contract. It works only with schedulers that specifically support the
LWS-specific annotation. The structured `schedulingConstraints` field aligns with the
upstream community direction and is portable across WAS-compatible schedulers.
