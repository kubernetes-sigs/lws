# KEP-766: DisaggDeployment

<!--
This KEP proposes adding DisaggDeployment as a higher-level API for managing
disaggregated inference workloads using LeaderWorkerSet as the underlying
workload primitive.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Deploy Disaggregated LLM Inference](#story-1-deploy-disaggregated-llm-inference)
    - [Story 2: Rolling Update with Zero Downtime](#story-2-rolling-update-with-zero-downtime)
    - [Story 3: Coordinated Service Discovery](#story-3-coordinated-service-discovery)
  - [Notes/Constraints](#notesconstraints)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [DisaggDeployment API](#disaggdeployment-api)
  - [Two-Dimensional Rolling Update Algorithm](#two-dimensional-rolling-update-algorithm)
  - [Service Orchestration](#service-orchestration)
  - [Controller Architecture](#controller-architecture)
  - [Test Plan](#test-plan)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Extend LeaderWorkerSet with Multi-Template Support](#alternative-1-extend-leaderworkerset-with-multi-template-support)
  - [Alternative 2: Helm Chart or Kustomize Overlay](#alternative-2-helm-chart-or-kustomize-overlay)
  - [Alternative 3: External Controller Without CRD](#alternative-3-external-controller-without-crd)
<!-- /toc -->

## Summary

This KEP proposes adding `DisaggDeployment` as a new Custom Resource Definition (CRD) to the LeaderWorkerSet (LWS) project. DisaggDeployment is a higher-level abstraction that orchestrates multiple LeaderWorkerSets with coordinated lifecycle management, specifically designed for disaggregated inference architectures where "prefill" and "decode" workloads run on separate infrastructure.

Disaggregated serving is an optimization for LLM inference workloads that takes advantage of the fact that the two phases of inference (prefill and decode) have different computational characteristics. State-of-the-art LLM serving frameworks such as [vLLM](https://github.com/vllm-project/vllm) and [SGLang](https://github.com/sgl-project/sglang) support this optimization.

DisaggDeployment simplifies the deployment of disaggregated LLM inference by:
- Managing two LeaderWorkerSets (prefill and decode) as a single logical unit
- Providing coordinated two-dimensional rolling updates across both sides
- Automatically creating Services when both sides are ready

## Motivation

Currently, deploying disaggregated inference workloads requires users to manually create and coordinate two separate LeaderWorkerSets. This leads to several challenges:

1. **Operational complexity**: Users must manually ensure both sides are updated together and handle failure scenarios across two resources.

2. **Rolling update coordination**: There is no built-in mechanism to coordinate rolling updates across prefill and decode sides, risking service disruption if one side is updated without the other.

3. **Service lifecycle**: Users must manually manage Services and ensure they only route traffic when both sides are ready.

4. **Configuration drift**: Without a unified resource, prefill and decode configurations can drift apart, leading to subtle incompatibilities.

### Goals

1. **Unified Management**: Provide a single CRD that manages both prefill and decode LeaderWorkerSets as a cohesive unit.

2. **Coordinated Rolling Updates**: Implement a two-dimensional rolling update algorithm that updates both sides in lockstep, respecting per-side surge constraints.

3. **Service Orchestration**: Automatically manage Services based on workload readiness, ensuring traffic only flows when both sides are operational.

4. **Stateless Controller**: Design the controller to derive all state from observed resources, enabling safe restarts at any point.

### Non-Goals

1. **Auto-scaling support**: HPA/VPA integration or automatic scaling based on inference load metrics is out of scope.

2. **Multi-cluster federation**: Managing DisaggDeployments across multiple Kubernetes clusters is not addressed.

3. **Custom workload backends**: Supporting backends other than LeaderWorkerSet (e.g., StatefulSet, Deployment) is not planned.

4. **Traffic management / routing**: Integration with service meshes or ingress controllers for traffic splitting during rollouts is out of scope.

5. **Generic group support**: Supporting arbitrary groups beyond prefill/decode (e.g., `decode-long-context`, `encode`) is deferred to a future enhancement.

## Proposal

We propose adding a new CRD called `DisaggDeployment` that acts as a higher-level controller over LeaderWorkerSet resources. The DisaggDeployment controller will:

1. Create and manage two LeaderWorkerSets (one for prefill, one for decode)
2. Coordinate rolling updates using a two-dimensional algorithm
3. Manage Service lifecycle based on workload readiness

### User Stories

#### Story 1: Deploy Disaggregated LLM Inference

As a platform engineer, I want to deploy a disaggregated LLM inference workload by creating a single DisaggDeployment resource that specifies both prefill and decode configurations, so that I don't have to manage two separate LeaderWorkerSets manually.

#### Story 2: Rolling Update with Zero Downtime

As a DevOps engineer, I want to update my model version across both prefill and decode sides with zero downtime, with the system automatically coordinating the rollout to ensure capacity is maintained throughout the update.

#### Story 3: Coordinated Service Discovery

As an application developer, I want Services to be automatically created for my prefill and decode endpoints, but only when both sides are ready to serve traffic, so that my application doesn't route requests to partially-ready infrastructure.

### Notes/Constraints

**Hardcoded Prefill/Decode Groups**: The initial implementation uses hardcoded `prefill` and `decode` groups rather than a generic group abstraction. This design choice is intentional:

- **Simplicity**: Prefill/decode is the most common disaggregated inference pattern, covering the majority of use cases.
- **Clear semantics**: Explicit fields (`spec.prefill`, `spec.decode`) provide better discoverability and validation than a generic list.
- **Future extensibility**: The architecture can be extended in a follow-up enhancement to support generic groups (e.g., `decode-long-context`, `encode`) by treating groups in a more flexible way, while maintaining backwards compatibility with the prefill/decode API.

### Risks and Mitigations

**Risk**: The two-dimensional rolling update algorithm adds complexity that could lead to stuck rollouts.

**Mitigation**: The algorithm is designed with safety invariants (scale up before scale down, coordinated drain) and the controller is stateless, allowing manual intervention by scaling workloads directly if needed.

**Risk**: Adding a new CRD increases the API surface and maintenance burden.

**Mitigation**: DisaggDeployment is additive and does not modify existing LeaderWorkerSet behavior. Users who don't need disaggregated inference can continue using LeaderWorkerSet directly.

## Design Details

### DisaggDeployment API

```go
// DisaggDeployment is the Schema for the disaggdeployments API
type DisaggDeployment struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   DisaggDeploymentSpec   `json:"spec"`
    Status DisaggDeploymentStatus `json:"status,omitempty"`
}

// DisaggDeploymentSpec defines the desired state of DisaggDeployment
type DisaggDeploymentSpec struct {
    // Prefill configuration for the disaggregated deployment
    // +required
    Prefill *DisaggSideConfig `json:"prefill"`

    // Decode configuration for the disaggregated deployment
    // +required
    Decode *DisaggSideConfig `json:"decode"`
}

// DisaggSideConfig defines the configuration for the prefill/decode side.
// This structure embeds LeaderWorkerSetSpec fields and adds disagg-specific
// fields like ServiceTemplate.
type DisaggSideConfig struct {
    // Replicas is the number of leader-worker groups.
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:default=1
    Replicas *int32 `json:"replicas,omitempty"`

    // LeaderWorkerTemplate defines the template for leader/worker pods.
    // +required
    LeaderWorkerTemplate LeaderWorkerTemplate `json:"leaderWorkerTemplate"`

    // RolloutStrategy defines the strategy for rolling out updates.
    RolloutStrategy RolloutStrategy `json:"rolloutStrategy,omitempty"`

    // StartupPolicy determines when workers should start relative to the leader.
    // +kubebuilder:default=LeaderCreated
    StartupPolicy StartupPolicyType `json:"startupPolicy,omitempty"`

    // NetworkConfig defines the network configuration of the group.
    NetworkConfig *NetworkConfig `json:"networkConfig,omitempty"`

    // ServiceTemplate defines an optional Service to create alongside the LWS.
    // Services are only created when both prefill and decode sides are ready.
    ServiceTemplate *ServiceTemplate `json:"serviceTemplate,omitempty"`
}

// ServiceTemplate defines the template for creating a Service alongside the LWS.
type ServiceTemplate struct {
    // Spec is the Service specification.
    // The selector will be auto-populated unless AutoPopulateSelector is false.
    Spec corev1.ServiceSpec `json:"spec"`

    // Labels to add to the Service.
    Labels map[string]string `json:"labels,omitempty"`

    // AutoPopulateSelector controls whether the selector is auto-populated.
    // +kubebuilder:default=true
    AutoPopulateSelector *bool `json:"autoPopulateSelector,omitempty"`
}
```

**Naming Convention**: LeaderWorkerSets are named `{disaggdeployment-name}-{revision}-{side}` where:
- `revision` is a truncated hash of both pod templates (ensures coordinated updates)
- `side` is either `prefill` or `decode`

**Labels**: The following labels are applied to managed LeaderWorkerSets:
- `disaggdeployment.x-k8s.io/name`: DisaggDeployment name
- `disaggdeployment.x-k8s.io/side`: `prefill` or `decode`
- `disaggdeployment.x-k8s.io/revision`: Template hash

### Two-Dimensional Rolling Update Algorithm

The rolling update algorithm coordinates both prefill and decode sides using a linear interpolation approach:

```
newAtStep(i) = ceil(i * target / totalSteps)    // scale up: 0 → target
oldAtStep(i) = source - floor(i * source / totalSteps)  // scale down: source → 0
```

**Key Properties**:

1. **Decoupled Steps**: Each step changes EITHER old OR new replicas, not both. This simplifies reasoning about state transitions.

2. **Two-Dimensional Coordination**:
   - Scale-up uses `min(prefillStep, decodeStep)` to keep sides in sync
   - Scale-down uses `max(prefillStep, decodeStep)` to ensure both drain together

3. **Coordinated Drain**: If either side reaches 0 replicas, both sides are forced to 0. This prevents orphaned single-side workloads from interrupted rollouts.

4. **Surge Constraints**: Per-side `maxSurge` and `maxUnavailable` are respected:
   ```
   old + new <= target + maxSurge
   ```

5. **Scale-Up Priority**: New replicas are scaled up before old replicas are scaled down, ensuring capacity is never below the minimum.

6. **Stability Check**: The controller waits for `replicas == readyReplicas` before computing the next step.

**Example 1: Symmetric Rollout** (3 prefill, 3 decode → 3 prefill, 3 decode, maxSurge=1):

| Step | Old Prefill | Old Decode | New Prefill | New Decode |
|------|-------------|------------|-------------|------------|
| 0    | 3           | 3          | 0           | 0          |
| 1    | 3           | 3          | 1           | 1          |
| 2    | 2           | 2          | 1           | 1          |
| 3    | 2           | 2          | 2           | 2          |
| 4    | 1           | 1          | 2           | 2          |
| 5    | 1           | 1          | 3           | 3          |
| 6    | 0           | 0          | 3           | 3          |

**Example 2: Asymmetric Replicas** (9 prefill, 3 decode → 9 prefill, 3 decode, maxSurge=1):

When prefill and decode have different replica counts, the algorithm computes steps based on the larger dimension (9 in this case). The smaller side (decode) progresses proportionally slower:

| Step | Old Prefill | Old Decode | New Prefill | New Decode | Notes |
|------|-------------|------------|-------------|------------|-------|
| 0    | 9           | 3          | 0           | 0          | Start |
| 1    | 9           | 3          | 1           | 1          | Scale up both |
| 2    | 8           | 3          | 1           | 1          | Scale down prefill (surge limit) |
| 3    | 8           | 3          | 2           | 1          | Scale up prefill |
| 4    | 7           | 3          | 2           | 1          | Scale down prefill |
| 5    | 7           | 3          | 3           | 1          | Scale up prefill |
| 6    | 6           | 2          | 3           | 1          | Scale down both |
| 7    | 6           | 2          | 4           | 2          | Scale up both |
| ...  | ...         | ...        | ...         | ...        | ... |
| n    | 0           | 0          | 9           | 3          | Complete |

Key observations:
- Prefill progresses through more steps due to higher replica count
- Decode only changes when proportionally appropriate (roughly every 3 prefill steps)
- Total capacity is maintained throughout: `old + new` stays within surge limits per side

**Example 3: Replica Count Change** (9 prefill, 3 decode → 3 prefill, 9 decode, maxSurge=1):

The algorithm also handles simultaneous replica count changes across sides. This scenario might occur when rebalancing infrastructure based on workload characteristics:

| Step | Old Prefill | Old Decode | New Prefill | New Decode | Notes |
|------|-------------|------------|-------------|------------|-------|
| 0    | 9           | 3          | 0           | 0          | Start |
| 1    | 9           | 3          | 1           | 1          | Scale up both |
| 2    | 8           | 3          | 1           | 1          | Scale down prefill |
| 3    | 8           | 3          | 1           | 2          | Scale up decode |
| 4    | 7           | 2          | 1           | 2          | Scale down both |
| 5    | 7           | 2          | 2           | 3          | Scale up both |
| ...  | ...         | ...        | ...         | ...        | ... |
| n    | 0           | 0          | 3           | 9          | Complete |

Key observations:
- Prefill scales down from 9 to 3 (net decrease of 6)
- Decode scales up from 3 to 9 (net increase of 6)
- The algorithm handles both directions simultaneously
- At no point does either side drop below minimum required capacity

### Service Orchestration

Services are managed based on workload readiness:

1. **Creation Condition**: Services are only created when the target revision's LeaderWorkerSets are ready on BOTH sides.

2. **Selector Auto-Population**: By default, selectors are populated with labels matching the current revision.

3. **Cleanup**: Services for old revisions are deleted when those revisions are fully drained.

### Controller Architecture

The controller follows a stateless design:

1. **Reconciliation Loop**:
   - Fetch DisaggDeployment
   - Compute revision hash from both templates
   - Cleanup drained workloads (both sides at 0)
   - Route to rolling update or simple reconcile based on old workload existence
   - Reconcile Services

2. **State Tracking**:
   - `initial-replicas` annotation records starting replica count for rolling updates
   - No in-memory state; safe to restart at any point

3. **Owner References**: Set on managed LeaderWorkerSets and Services for garbage collection.

### Test Plan

[X] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

#### Unit tests

- Rolling update planner: step computation, edge cases, constraint violations
- Executor: scale-up/scale-down coordination, stability checks
- Service manager: creation conditions, cleanup logic
- API validation: both sides required, replica constraints

#### Integration tests

- DisaggDeployment creation creates two LeaderWorkerSets
- Template update triggers coordinated rolling update
- Service created only when both sides ready
- Interrupted rollout resumes correctly after controller restart
- Deletion cascades to owned resources

### Graduation Criteria

**Alpha (v0.1)**:
- DisaggDeployment CRD with validation
- LeaderWorkerSet creation and ownership
- Two-dimensional rolling update algorithm
- Service orchestration
- Comprehensive test coverage (>80%)
- Documentation and examples

**Beta (v0.2)**:
- Production usage feedback incorporated
- Metrics for observability
- (Optional) Generic group support beyond prefill/decode

**Stable (v1.0)**:
- Performance optimization for large deployments
- Proven stability in production environments

## Implementation History

- 2026-03-05: Initial KEP draft

## Drawbacks

1. **Increased Complexity**: Adding another CRD increases the API surface and learning curve for new users.

2. **Opinionated Design**: DisaggDeployment assumes a prefill/decode architecture which may not fit all disaggregated workloads.

3. **Controller Overhead**: Managing an additional controller layer adds some overhead, though minimal for typical deployment sizes.

## Alternatives

### Alternative 1: Extend LeaderWorkerSet with Multi-Template Support

Instead of a new CRD, extend LeaderWorkerSet to support multiple pod templates with labels distinguishing "roles" (prefill vs decode).

**Rejected because**:
- Significantly changes LeaderWorkerSet semantics
- Makes the core LWS controller more complex
- Rolling update coordination would be harder to implement within a single resource

### Alternative 2: Helm Chart or Kustomize Overlay

Provide a Helm chart that creates two coordinated LeaderWorkerSets.

**Rejected because**:
- No runtime coordination for rolling updates
- Users would need to implement their own update strategy
- Services cannot be conditionally created based on workload readiness

### Alternative 3: External Controller Without CRD

Build an external controller that watches LeaderWorkerSets with specific labels and coordinates them.

**Rejected because**:
- Poor user experience (no single resource to manage)
- Harder to discover and use
- State management would be complex without a CRD
