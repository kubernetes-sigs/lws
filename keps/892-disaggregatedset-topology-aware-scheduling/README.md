# KEP-892: [DisaggregatedSet] Topology-Aware Scheduling

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Architecture and Responsibility Split](#architecture-and-responsibility-split)
  - [User Stories](#user-stories)
    - [Story 1: Rack-Local Prefill](#story-1-rack-local-prefill)
    - [Story 2: Block-Constrained Serving Unit](#story-2-block-constrained-serving-unit)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API Changes](#api-changes)
    - [SchedulingConstraints and TopologyConstraint](#schedulingconstraints-and-topologyconstraint)
    - [LeaderWorkerSet API Updates](#leaderworkerset-api-updates)
    - [DisaggregatedSet API Updates](#disaggregatedset-api-updates)
  - [Controller Behaviour](#controller-behaviour)
    - [Compiling Intent into WAS Objects](#compiling-intent-into-was-objects)
    - [Fallback to TopologySpreadConstraints](#fallback-to-topologyspreadconstraints)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: LWS-specific custom TopologyConstraint API (with Key/Strength)](#alternative-1-lws-specific-custom-topologyconstraint-api-with-keystrength)
  - [Alternative 2: Reuse Pod Affinity / nodeAffinity only](#alternative-2-reuse-pod-affinity--nodeaffinity-only)
<!-- /toc -->

## Summary

This KEP proposes adding topology-aware scheduling (TAS) support to `DisaggregatedSet` and `LeaderWorkerSet` by aligning with the upstream [KEP-6089: Workload Aware Scheduling (WAS) Controller Integration APIs](https://github.com/kubernetes/enhancements/issues/6089) and [KEP-5732: Topology-Aware Workload Scheduling](https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5732-topology-aware-workload-scheduling).

Rather than introducing custom LWS-specific placement APIs, this proposal reuses upstream WAS building blocks (`schedulingConstraints.topology`) and leverages `DisaggregatedSet` as the outer/root controller that compiles topology and scheduling intent into a hierarchical tree consisting of `Workload` / `CompositePodGroup` (representing the entire serving unit) and leaf `PodGroup`s (representing LWS replicas).

## Motivation

Disaggregated serving performance (e.g. vLLM or similar multi-host LLM serving frameworks) is highly sensitive to network topology. Prefill and decode workers perform KV-cache transfers and collective communication that suffer significant latency penalties when spanning slow inter-rack, inter-block, or inter-zone boundaries.

Currently, LWS users can only define static `nodeAffinity` or `podAffinity` on pod templates. These do not communicate group-level topology requirements to the scheduler, cannot express co-location constraints for group scheduling units, and do not give the scheduler visibility into the resource requirements of the entire group.

To resolve this, we need a native way to express group-level topology constraints. Since the upstream Kubernetes community is actively standardizing Workload Aware Scheduling (WAS) APIs (KEP-6089 / KEP-5732), LWS and `DisaggregatedSet` must align with these upstream APIs to benefit from native scheduler plugins (e.g., `TopologyPlacement` plugin) while providing a robust fallback for clusters without the new APIs installed.

### Goals

1. Add `schedulingConstraints` to the `LeaderWorkerSet` API to allow expressing group topology constraints (e.g. `topology.kubernetes.io/rack`).
2. Propagate topology constraints from LWS templates in `DisaggregatedSetSpec` to the managed child `LeaderWorkerSet` controllers.
3. Support compiling the disaggregated serving unit scheduling intent into a hierarchical `CompositePodGroup` or `Workload` tree as proposed in KEP-6089.
4. Provide a fallback mechanism that translates `schedulingConstraints` to pod-level `topologySpreadConstraints` when upstream WAS APIs are not available in the cluster.

### Non-Goals

1. Implementing a custom scheduler plugin or modifying the default kube-scheduler.
2. Supporting multi-cluster or multi-zone federation.
3. Automatically rescheduling already running pods.

## Proposal

### Architecture and Responsibility Split

We define a clean split of responsibility between `DisaggregatedSet` and `LeaderWorkerSet`:

1. **LeaderWorkerSet** is the leaf controller responsible for a single role's replicas. It supports a first-class `schedulingConstraints` field in its spec, making it scheduling-aware.
2. **DisaggregatedSet** is the root/orchestration controller. It manages multiple roles, each represented by a child LWS. It exposes `schedulingConstraints` at the serving-unit level (global) and allows overriding them per-role inside the `roles` (LWS templates) list.
3. **Compilation**: The `DisaggregatedSet` controller acts as the root compiler. It translates the multi-role topology constraints into a `CompositePodGroup` (or root `Workload`) and children `PodGroup`s (one per LWS replica).

```
          [ DisaggregatedSet ] (Root Controller)
                   │
         ┌─────────┴─────────┐
         ▼                   ▼
    [ LWS: prefill ]    [ LWS: decode ] (Leaf Controllers)
         │                   │
         ▼                   ▼
    [ PodGroup ]        [ PodGroup ] (Leaf PodGroups)
         └─────────┬─────────┘
                   ▼
         [ CompositePodGroup ] (Compiled Scheduling Tree)
```

By aligning with KEP-6089, we define `SchedulingConstraints` exactly matching the upstream WAS structure, using a `topology` list containing keys.

### User Stories

#### Story 1: Rack-Local Prefill

As a platform engineer deploying a disaggregated serving cluster, I want all prefill pods in a single replica to land on nodes within the same rack, so that collective-communication overhead stays within low-latency rack fabric.

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: vllm-serving
spec:
  roles:
    - name: prefill
      spec:
        replicas: 4
        schedulingConstraints:
          topology:
            - key: topology.kubernetes.io/rack
        leaderWorkerTemplate: { ... }
    - name: decode
      spec:
        replicas: 8
        leaderWorkerTemplate: { ... }
```

The prefill LWS controller will compile this intent into `PodGroup`s with rack-level topology constraints.

#### Story 2: Block-Constrained Serving Unit

As a platform engineer, I want the entire serving unit (both prefill and decode roles) to be placed within the same network block to ensure high-bandwidth KV-cache transfers between roles.

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: vllm-serving
spec:
  schedulingConstraints:
    topology:
      - key: topology.example.com/block
  roles:
    - name: prefill
      spec:
        replicas: 4
        leaderWorkerTemplate: { ... }
    - name: decode
      spec:
        replicas: 8
        leaderWorkerTemplate: { ... }
```

The `DisaggregatedSet` controller compiles this global constraint into the root `CompositePodGroup` representing the entire serving unit.

### Risks and Mitigations

**Risk 1**: Upstream WAS APIs (KEP-6089 / KEP-6012) are still evolving and might change.
*Mitigation*: We hide the native WAS integration behind the `DisaggregatedSetTAS` feature gate (disabled by default). The fallback path (using `topologySpreadConstraints`) is always available and stable.

**Risk 2**: Conflict between global (unit-level) and role-level topology constraints.
*Mitigation*: The validation webhook will enforce that role-level topology domains must be sub-domains of (or equal to) the unit-level topology domain. For example, if the unit-level is zone-constrained, a role can be rack-constrained, but not vice-versa.

## Design Details

### API Changes

#### SchedulingConstraints and TopologyConstraint

We align with `scheduling.k8s.io` to define the following structures:

```go
// SchedulingConstraints defines scheduling constraints for a PodGroup or LeaderWorkerSet.
type SchedulingConstraints struct {
    // Topology defines placement constraints across topology domains.
    // +optional
    Topology []TopologyConstraint `json:"topology,omitempty"`
}

// TopologyConstraint describes a topology co-location constraint.
type TopologyConstraint struct {
    // Key is the node label key that identifies the topology domain.
    // +required
    Key string `json:"key"`
}
```

#### LeaderWorkerSet API Updates

We add `SchedulingConstraints` to `LeaderWorkerSetSpec`:

```go
type LeaderWorkerSetSpec struct {
    // SchedulingConstraints defines group-level scheduling constraints.
    // +optional
    SchedulingConstraints *SchedulingConstraints `json:"schedulingConstraints,omitempty"`
    
    // Remaining fields unchanged ...
}
```

#### DisaggregatedSet API Updates

We add global and role-level `SchedulingConstraints`:

```go
type DisaggregatedSetSpec struct {
    // SchedulingConstraints defines unit-level scheduling constraints applied globally.
    // +optional
    SchedulingConstraints *SchedulingConstraints `json:"schedulingConstraints,omitempty"`

    // Roles defines the list of roles.
    // +required
    Roles []DisaggregatedRoleSpec `json:"roles"`
}

type DisaggregatedRoleSpec struct {
    // Name is the unique identifier for this role.
    // +required
    Name string `json:"name"`

    // Spec defines the child LeaderWorkerSet spec, which now includes SchedulingConstraints.
    // +required
    Spec LeaderWorkerSetSpec `json:"spec"`
}
```

### Controller Behaviour

#### Compiling Intent into WAS Objects

When the feature gate `DisaggregatedSetTAS=true` is enabled:

1. For each `DisaggregatedSet`, the controller creates a `CompositePodGroup` (representing the entire serving unit).
2. If global `schedulingConstraints` are specified, they are populated into the `CompositePodGroup.spec.schedulingConstraints`.
3. For each role and LWS replica, a leaf `PodGroup` is created and associated with the parent `CompositePodGroup`.
4. Role-specific `schedulingConstraints` are populated into the respective leaf `PodGroup.spec.schedulingConstraints`.

The scheduler's `TopologyPlacement` plugin will evaluate this hierarchy holistically.

#### Fallback to TopologySpreadConstraints

When `DisaggregatedSetTAS=false` or the WAS APIs are not installed:

The controller translates group-level `schedulingConstraints` into pod-level `topologySpreadConstraints` injected into the `leaderWorkerTemplate`:

- For each constraint key (e.g. `topology.kubernetes.io/rack`):
  - Add a `TopologySpreadConstraint` with:
    - `maxSkew: 1`
    - `topologyKey: <key>`
    - `whenUnsatisfiable: DoNotSchedule` (ensuring strict co-location matching the required semantics of group topology constraints).
    - `labelSelector` matching the specific LWS replica.

### Test Plan

#### Unit tests

- Webhook validation for constraint hierarchies.
- Fallback generation: verifying that `schedulingConstraints` map correctly to pod-level `topologySpreadConstraints`.
- Hierarchical merging of global and role-specific constraints.

#### Integration tests

- Verifying child `LeaderWorkerSet` specs contain the expected `SchedulingConstraints`.
- Verifying the creation of `CompositePodGroup` and leaf `PodGroup` resources with proper hierarchy when the feature gate is enabled.
- Verifying pod creation webhook correctly admits and validates the mutated templates.

#### e2e tests

- Verify successful scheduling of a `DisaggregatedSet` workload with topology constraints on a multi-rack Kind cluster.

### Graduation Criteria

**Alpha (v0.9)**:
- `SchedulingConstraints` API fields added to LWS and DisaggregatedSet.
- Fallback path to `TopologySpreadConstraints` fully implemented.
- Webhook validation for constraints.

**Beta (v1.0)**:
- Out-of-the-box `CompositePodGroup` compilation enabled behind `DisaggregatedSetTAS` feature gate.
- Integration tests with simulated WAS controller.

**Stable (v1.1)**:
- Remove feature gate, enabling native WAS compilation by default.

## Implementation History

- 2026-07-08: Initial KEP draft revised to align with KEP-6089 WAS APIs.

## Drawbacks

- **Complexity in Fallback**: Translating group-level scheduling constraints to pod-level topology spread constraints has limitations (e.g., scheduler can't evaluate the total group size upfront), but it provides a reasonable approximation until WAS is widely adopted.

## Alternatives

### Alternative 1: LWS-specific custom TopologyConstraint API (with Key/Strength)

We previously considered introducing a custom `TopologyConstraint` struct containing `key` and `strength` (Required/Preferred) fields.

**Rejected because**:
- It introduces LWS-specific scheduling primitives that duplicate or conflict with the upstream WAS/TAS specifications under KEP-6089/KEP-5732.
- The default Kubernetes scheduler cannot natively parse custom LWS API fields, forcing LWS to either manage complex fallback logic or run a custom scheduler plugin, whereas upstream WAS allows using the native `TopologyPlacement` plugin.

### Alternative 2: Reuse Pod Affinity / nodeAffinity only

**Rejected because**:
- Pod affinity is evaluated on a per-pod basis during scheduling. It does not allow the scheduler to lock a topology domain (e.g., rack) based on the combined resource needs of the entire group. This frequently leads to partial scheduling and resource deadlocks in busy clusters.
