---
title: DisaggregatedSet API
linkTitle: DisaggregatedSet API
weight: 20
content_type: tool-reference
package: disaggregatedset.x-k8s.io/v1
auto_generated: true
description: Generated API reference documentation for disaggregatedset.x-k8s.io/v1.
---


## Resource Types 


- [DisaggregatedSet](#disaggregatedset-x-k8s-io-v1-DisaggregatedSet)
  

## `DisaggregatedSet`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedSet}
    

**Appears in:**



<p>DisaggregatedSet is the Schema for the disaggregatedsets API</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
<tr><td><code>apiVersion</code><br/>string</td><td><code>disaggregatedset.x-k8s.io/v1</code></td></tr>
<tr><td><code>kind</code><br/>string</td><td><code>DisaggregatedSet</code></td></tr>
    
  
<tr><td><code>spec</code> <B>[Required]</B><br/>
<a href="#disaggregatedset-x-k8s-io-v1-DisaggregatedSetSpec"><code>DisaggregatedSetSpec</code></a>
</td>
<td>
   <p>spec defines the desired state of DisaggregatedSet</p>
</td>
</tr>
<tr><td><code>status</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-DisaggregatedSetStatus"><code>DisaggregatedSetStatus</code></a>
</td>
<td>
   <p>status defines the observed state of DisaggregatedSet</p>
</td>
</tr>
</tbody>
</table>

## `DisaggregatedRoleSpec`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedRoleSpec}
    

**Appears in:**

- [DisaggregatedSetSpec](#disaggregatedset-x-k8s-io-v1-DisaggregatedSetSpec)


<p>DisaggregatedRoleSpec defines the configuration for a disaggregated role.
This structure embeds LeaderWorkerSetTemplateSpec from sigs.k8s.io/lws, with validation
to reject unsupported fields (RolloutStrategy.Type must be RollingUpdate,
RolloutStrategy.RollingUpdateConfiguration.Partition must not be set).</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>name</code> <B>[Required]</B><br/>
<code>string</code>
</td>
<td>
   <p>Name is the unique identifier for this role.</p>
</td>
</tr>
<tr><td><code>subRoles</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-DisaggregatedSubRoleSpec"><code>[]DisaggregatedSubRoleSpec</code></a>
</td>
<td>
   <p>SubRoles partitions one LWS into pools sharing its templates.
Child targets sum to parent replicas; omit parent scaling. Parent spec.replicas is ignored.</p>
</td>
</tr>
<tr><td><code>scaling</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-RoleScaling"><code>RoleScaling</code></a>
</td>
<td>
   <p>Scaling configures how replicas are determined. Omit for inline Static
scaling (default). When set to External, the DisaggregatedSet controller
auto-creates a DisaggregatedSetRoleScaler and reads its spec.replicas.</p>
</td>
</tr>
<tr><td><code>LeaderWorkerSetTemplateSpec</code> <B>[Required]</B><br/>
<a href="#leaderworkerset-x-k8s-io-v1-LeaderWorkerSetTemplateSpec"><code>LeaderWorkerSetTemplateSpec</code></a>
</td>
<td>(Members of <code>LeaderWorkerSetTemplateSpec</code> are embedded into this type.)
   <p>LeaderWorkerSetTemplateSpec defines the LWS template for this role.
Note: Spec.RolloutStrategy.Type must be RollingUpdate (or empty) and
Spec.RolloutStrategy.RollingUpdateConfiguration.Partition must not be set.
DisaggregatedSet handles rollouts across roles.</p>
</td>
</tr>
</tbody>
</table>

## `DisaggregatedSetRoleScaler`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedSetRoleScaler}
    

**Appears in:**



<p>DisaggregatedSetRoleScaler exposes the /scale subresource for a single role
of a DisaggregatedSet. Instances are auto-created by the DisaggregatedSet
controller for every role with scaling.mode: External and are named
&quot;<!-- raw HTML omitted -->-<!-- raw HTML omitted -->&quot;. External autoscalers (HPA, KEDA, or any
/scale-aware controller) write spec.replicas; the DisaggregatedSet controller
reads it and drives the role's LeaderWorkerSet.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>spec</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-DisaggregatedSetRoleScalerSpec"><code>DisaggregatedSetRoleScalerSpec</code></a>
</td>
<td>
   <p>spec defines the desired state of DisaggregatedSetRoleScaler</p>
</td>
</tr>
<tr><td><code>status</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-DisaggregatedSetRoleScalerStatus"><code>DisaggregatedSetRoleScalerStatus</code></a>
</td>
<td>
   <p>status defines the observed state of DisaggregatedSetRoleScaler</p>
</td>
</tr>
</tbody>
</table>

## `DisaggregatedSetRoleScalerSpec`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedSetRoleScalerSpec}
    

**Appears in:**

- [DisaggregatedSetRoleScaler](#disaggregatedset-x-k8s-io-v1-DisaggregatedSetRoleScaler)


<p>DisaggregatedSetRoleScalerSpec is the desired state written by an external
autoscaler via the /scale subresource. The (DS, role) association is derived
from the scaler's controller ownerReference and its role label.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>replicas</code> <B>[Required]</B><br/>
<code>int32</code>
</td>
<td>
   <p>Replicas is the desired replica count for the role. The controller seeds
this at scaler creation — 1 for a fresh role (0 would deadlock vanilla
HPA in ScalingDisabled), or the LWS's current replica count for a
Static→External flip so the role does not silently drain to zero.
External autoscalers overwrite it on their first tick.</p>
<p>Non-pointer with a default because kube-apiserver's CRD /scale handler
extracts .spec.replicas at read time and errors (&quot;the spec replicas
field does not exist&quot;) when the JSONPath resolves to nothing. HPA reads
/scale before its first write; a missing field would deadlock the loop.</p>
</td>
</tr>
</tbody>
</table>

## `DisaggregatedSetRoleScalerStatus`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedSetRoleScalerStatus}
    

**Appears in:**

- [DisaggregatedSetRoleScaler](#disaggregatedset-x-k8s-io-v1-DisaggregatedSetRoleScaler)


<p>DisaggregatedSetRoleScalerStatus is the observed state written back by the
DisaggregatedSet controller.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>replicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>Replicas is the observed replica count for this role — LWS groups
(== leader pods), aggregated across all revisions currently present.
Read by HPA as the &quot;current&quot; count for its ratio math.</p>
</td>
</tr>
<tr><td><code>selector</code><br/>
<code>string</code>
</td>
<td>
   <p>Selector is a label selector (in string form) matching one pod per LWS
group (the leader), across all revisions:
disaggregatedset.x-k8s.io/name=<!-- raw HTML omitted -->,disaggregatedset.x-k8s.io/role=<!-- raw HTML omitted -->,leaderworkerset.sigs.k8s.io/worker-index=0
Leader-only so HPA's per-pod-metric averaging divides by group count
(matching spec.replicas that HPA writes), keeping the ratio math
consistent for leaderWorkerTemplate.size &gt; 1. Aggregate across
revisions so HPA observes the serving fleet during a rolling update.</p>
</td>
</tr>
<tr><td><code>observedGeneration</code><br/>
<code>int64</code>
</td>
<td>
   <p>ObservedGeneration is the .metadata.generation the status reflects.</p>
</td>
</tr>
<tr><td><code>conditions</code><br/>
<a href="https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#condition-v1-meta"><code>[]k8s.io/apimachinery/pkg/apis/meta/v1.Condition</code></a>
</td>
<td>
   <p>Conditions expose scaler-level state (Ready).</p>
</td>
</tr>
</tbody>
</table>

## `DisaggregatedSetScalingPolicy`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedSetScalingPolicy}
    

**Appears in:**

- [DisaggregatedSetSpec](#disaggregatedset-x-k8s-io-v1-DisaggregatedSetSpec)


<p>DisaggregatedSetScalingPolicy configures how replica target changes interact
with DisaggregatedSet reconciliation.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>scaleDownDuringRollout</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-RolloutScaleDownPolicy"><code>RolloutScaleDownPolicy</code></a>
</td>
<td>
   <p>ScaleDownDuringRollout controls demand-driven target replica reductions.
Deferred waits until old revisions finish draining. Immediate considers
reductions during the rollout, preferring safe old-revision drains.
Both policies allow scale-up and old-revision drains within rollout bounds.</p>
</td>
</tr>
<tr><td><code>subRoleScaleUpDelaySeconds</code><br/>
<code>int32</code>
</td>
<td>
   <p>SubRoleScaleUpDelaySeconds delays growth of an existing sub-role LWS so
nearby target updates can be applied together. Valid values are 0 to 60;
omitted or zero disables the delay. Only steady-state net growth waits:
ordinary roles, zero-to-positive scaling, and template rollouts do not.
The first growth request starts a fixed window; later updates do not extend
it. At expiry, the latest targets apply. The wait is cancelled if their sum
no longer exceeds current replicas. This adds scale-up latency and does not
combine updates arriving after the window.</p>
</td>
</tr>
</tbody>
</table>

## `DisaggregatedSetSpec`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedSetSpec}
    

**Appears in:**

- [DisaggregatedSet](#disaggregatedset-x-k8s-io-v1-DisaggregatedSet)


<p>DisaggregatedSetSpec defines the desired state of DisaggregatedSet.</p>
<p>The all-or-nothing replicas rule (either every role has replicas &gt; 0, or
every role has replicas == 0) applies only to non-External, unpartitioned
roles. External targets come from scalers; sub-roles may independently scale
to zero without pausing sibling routing pools.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>roles</code> <B>[Required]</B><br/>
<a href="#disaggregatedset-x-k8s-io-v1-DisaggregatedRoleSpec"><code>[]DisaggregatedRoleSpec</code></a>
</td>
<td>
   <p>Roles defines the list of roles (at least 1 required).
Each role has a unique name and its own configuration.</p>
</td>
</tr>
<tr><td><code>slices</code><br/>
<code>int32</code>
</td>
<td>
   <p>Slices is the number of independent copies of the whole role topology.
Each slice is a complete set of all roles that rolls out independently.
Changing Slices scales copies up or down and does not trigger a rollout.</p>
</td>
</tr>
<tr><td><code>scalingPolicy</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-DisaggregatedSetScalingPolicy"><code>DisaggregatedSetScalingPolicy</code></a>
</td>
<td>
   <p>ScalingPolicy controls replica target changes. Omit it for Deferred
scale-down during rollouts and no delay for steady-state sub-role growth.</p>
</td>
</tr>
<tr><td><code>placementPolicy</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-PlacementPolicy"><code>PlacementPolicy</code></a>
</td>
<td>
   <p>PlacementPolicy controls how a slice's roles are co-located and how the
DisaggregatedSet's slices are spread across topology domains. When set, the
controller injects pod affinity and anti-affinity into the managed
LeaderWorkerSet pod templates. Placement is applied when a LeaderWorkerSet is
created, so changing it takes effect on the next rollout.</p>
</td>
</tr>
</tbody>
</table>

## `DisaggregatedSetStatus`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedSetStatus}
    

**Appears in:**

- [DisaggregatedSet](#disaggregatedset-x-k8s-io-v1-DisaggregatedSet)


<p>DisaggregatedSetStatus defines the observed state of DisaggregatedSet.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>observedGeneration</code><br/>
<code>int64</code>
</td>
<td>
   <p>observedGeneration is the most recent generation observed for this DisaggregatedSet.</p>
</td>
</tr>
<tr><td><code>roleStatuses</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-RoleStatus"><code>[]RoleStatus</code></a>
</td>
<td>
   <p>RoleStatuses lists current parent roles in spec order, with nested sub-role statuses.
Parent counts include historical layouts and may exceed the visible child sum.
Removed roles are omitted even while their groups drain.</p>
</td>
</tr>
<tr><td><code>conditions</code><br/>
<a href="https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#condition-v1-meta"><code>[]k8s.io/apimachinery/pkg/apis/meta/v1.Condition</code></a>
</td>
<td>
   <p>conditions represent the current state of the DisaggregatedSet resource.
Each condition has a unique type and reflects the status of a specific aspect of the resource.</p>
<p>Standard condition types include:</p>
<ul>
<li>&quot;Available&quot;: the resource is fully functional</li>
<li>&quot;Progressing&quot;: the resource is being created or updated</li>
</ul>
<p>The status of each condition is one of True, False, or Unknown.</p>
</td>
</tr>
</tbody>
</table>

## `DisaggregatedSubRoleSpec`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedSubRoleSpec}
    

**Appears in:**

- [DisaggregatedRoleSpec](#disaggregatedset-x-k8s-io-v1-DisaggregatedRoleSpec)


<p>DisaggregatedSubRoleSpec defines a scaling and routing pool sharing its parent's templates.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>name</code> <B>[Required]</B><br/>
<code>string</code>
</td>
<td>
   <p>Name is unique within the parent role.</p>
</td>
</tr>
<tr><td><code>replicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>Replicas counts groups (Static default: 1); omit it for External scaling.</p>
</td>
</tr>
<tr><td><code>scaling</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-RoleScaling"><code>RoleScaling</code></a>
</td>
<td>
   <p>Scaling selects this pool's replica source.</p>
</td>
</tr>
<tr><td><code>maxSurge</code><br/>
<a href="https://pkg.go.dev/k8s.io/apimachinery/pkg/util/intstr#IntOrString"><code>k8s.io/apimachinery/pkg/util/intstr.IntOrString</code></a>
</td>
<td>
   <p>MaxSurge overrides the parent's rollout surge budget for this sub-role.
Accepts a non-negative integer or a percentage up to 100%, rounded up
against this sub-role's replica target. Omitted values inherit the parent;
with no parent rollout configuration, the default is 0.</p>
</td>
</tr>
<tr><td><code>maxUnavailable</code><br/>
<a href="https://pkg.go.dev/k8s.io/apimachinery/pkg/util/intstr#IntOrString"><code>k8s.io/apimachinery/pkg/util/intstr.IntOrString</code></a>
</td>
<td>
   <p>MaxUnavailable overrides the parent's rollout unavailability budget for
this sub-role. Accepts a non-negative integer or a percentage up to 100%,
rounded down against this sub-role's replica target. Omitted values inherit
the parent; with no parent rollout configuration, the default is 1. Zero is explicit.</p>
</td>
</tr>
</tbody>
</table>

## `PlacementPolicy`     {#disaggregatedset-x-k8s-io-v1-PlacementPolicy}
    

**Appears in:**

- [DisaggregatedSetSpec](#disaggregatedset-x-k8s-io-v1-DisaggregatedSetSpec)


<p>PlacementPolicy controls topology placement of a DisaggregatedSet's slices.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>type</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-PlacementType"><code>PlacementType</code></a>
</td>
<td>
   <p>Type selects the placement guarantee. Defaults to None.</p>
</td>
</tr>
<tr><td><code>topology</code><br/>
<code>string</code>
</td>
<td>
   <p>Topology is the node-label key that defines a domain, used as the affinity
topologyKey. Required when Type is not None.</p>
</td>
</tr>
</tbody>
</table>

## `PlacementType`     {#disaggregatedset-x-k8s-io-v1-PlacementType}
    
(Alias of `string`)

**Appears in:**

- [PlacementPolicy](#disaggregatedset-x-k8s-io-v1-PlacementPolicy)


<p>PlacementType selects the DisaggregatedSet placement guarantee.</p>




## `RoleScaling`     {#disaggregatedset-x-k8s-io-v1-RoleScaling}
    

**Appears in:**

- [DisaggregatedRoleSpec](#disaggregatedset-x-k8s-io-v1-DisaggregatedRoleSpec)

- [DisaggregatedSubRoleSpec](#disaggregatedset-x-k8s-io-v1-DisaggregatedSubRoleSpec)


<p>RoleScaling configures how replicas are determined for a role. Sub-struct
(not a bare enum) so future per-role scaling policies can be added without
a v2 API bump.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>mode</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-RoleScalingMode"><code>RoleScalingMode</code></a>
</td>
<td>
   <p>Mode controls the source of the replica count. Static (default) uses
inline spec.replicas; External uses the auto-created scaler CR.</p>
</td>
</tr>
</tbody>
</table>

## `RoleScalingMode`     {#disaggregatedset-x-k8s-io-v1-RoleScalingMode}
    
(Alias of `string`)

**Appears in:**

- [RoleScaling](#disaggregatedset-x-k8s-io-v1-RoleScaling)


<p>RoleScalingMode controls the source of the replica count for a role.</p>




## `RoleStatus`     {#disaggregatedset-x-k8s-io-v1-RoleStatus}
    

**Appears in:**

- [DisaggregatedSetStatus](#disaggregatedset-x-k8s-io-v1-DisaggregatedSetStatus)


<p>RoleStatus defines the observed state of a single parent role.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>name</code> <B>[Required]</B><br/>
<code>string</code>
</td>
<td>
   <p>Name is the parent role name.</p>
</td>
</tr>
<tr><td><code>desiredReplicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>DesiredReplicas is the number of replicas the controller is driving this role
toward, totalled across slices: its replicas, or the target of its
DisaggregatedSetRoleScaler when its scaling mode is External. A partitioned
role reports the sum of its sub-role targets. It is unset while any of those
targets is unknown, for example before the scaler exists, so it is never
mistaken for a requested zero. The Available condition is judged against it.</p>
</td>
</tr>
<tr><td><code>replicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>Replicas is the total number of replicas for this role.</p>
</td>
</tr>
<tr><td><code>readyReplicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>ReadyReplicas is the number of ready replicas for this role.
Sub-role readiness requires a coherent assignment across the whole group.</p>
</td>
</tr>
<tr><td><code>updatedReplicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>UpdatedReplicas is the number of replicas updated to the latest revision.</p>
</td>
</tr>
<tr><td><code>subRoleStatuses</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-SubRoleStatus"><code>[]SubRoleStatus</code></a>
</td>
<td>
   <p>SubRoleStatuses lists the current sub-roles in spec order.
Removed sub-roles are omitted, but their groups remain in the parent counts
while draining. Do not add child counts to their parent aggregate.</p>
</td>
</tr>
</tbody>
</table>

## `RolloutScaleDownPolicy`     {#disaggregatedset-x-k8s-io-v1-RolloutScaleDownPolicy}
    
(Alias of `string`)

**Appears in:**

- [DisaggregatedSetScalingPolicy](#disaggregatedset-x-k8s-io-v1-DisaggregatedSetScalingPolicy)


<p>RolloutScaleDownPolicy controls when demand-driven target replica reductions are
considered during an active rolling update. Both policies allow scale-up and
old-revision retirement within the rollout's availability and surge bounds.
For roles still present, reductions preserve effective desired - maxUnavailable
Ready replicas, or avoid worsening a shortfall if fewer are currently Ready.
Deferred External roles use at least the target's current Spec as effective
desired while old replicas of that role remain.</p>




## `SubRoleStatus`     {#disaggregatedset-x-k8s-io-v1-SubRoleStatus}
    

**Appears in:**

- [RoleStatus](#disaggregatedset-x-k8s-io-v1-RoleStatus)


<p>SubRoleStatus defines the observed state of a sub-role within its parent.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>name</code> <B>[Required]</B><br/>
<code>string</code>
</td>
<td>
   <p>Name is the sub-role name, without the parent prefix.</p>
</td>
</tr>
<tr><td><code>desiredReplicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>DesiredReplicas is the number of replica groups the controller is driving
this sub-role toward, totalled across slices: its replicas, or the target of
its DisaggregatedSetRoleScaler when its scaling mode is External. It is unset
while that target is unknown.</p>
</td>
</tr>
<tr><td><code>replicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>Replicas is the number of replica groups assigned to this sub-role.</p>
</td>
</tr>
<tr><td><code>readyReplicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>ReadyReplicas counts Ready groups with a coherent whole-group assignment.</p>
</td>
</tr>
<tr><td><code>updatedReplicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>UpdatedReplicas counts groups belonging to the latest revision.</p>
</td>
</tr>
</tbody>
</table>
  