---
title: DisaggregatedSet API
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
this at scaler creation — 0 for a fresh role, or the LWS's current
replica count for a Static→External flip so the role does not silently
drain to zero. External autoscalers overwrite it on their first tick.</p>
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
   <p>Replicas is the observed pod count for this role, aggregated across all
revisions currently present. Read by HPA as the &quot;current&quot; replica count.</p>
</td>
</tr>
<tr><td><code>selector</code><br/>
<code>string</code>
</td>
<td>
   <p>Selector is a label selector (in string form) matching all pods for this
role across all revisions:
disaggregatedset.x-k8s.io/name=<!-- raw HTML omitted -->,disaggregatedset.x-k8s.io/role=<!-- raw HTML omitted -->
Aggregate (revision-agnostic) so HPA sees the actual serving fleet during
a rolling update.</p>
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
<a href="https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta"><code>[]k8s.io/apimachinery/pkg/apis/meta/v1.Condition</code></a>
</td>
<td>
   <p>Conditions expose scaler-level state (Ready).</p>
</td>
</tr>
</tbody>
</table>

## `DisaggregatedSetSpec`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedSetSpec}
    

**Appears in:**

- [DisaggregatedSet](#disaggregatedset-x-k8s-io-v1-DisaggregatedSet)


<p>DisaggregatedSetSpec defines the desired state of DisaggregatedSet.</p>
<p>The all-or-nothing replicas rule (either every role has replicas &gt; 0, or
every role has replicas == 0) applies only to non-External roles. External
roles are exempt because their effective replicas live outside the DS spec —
they are driven via DisaggregatedSetRoleScaler.spec.replicas.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>roles</code> <B>[Required]</B><br/>
<a href="#disaggregatedset-x-k8s-io-v1-DisaggregatedRoleSpec"><code>[]DisaggregatedRoleSpec</code></a>
</td>
<td>
   <p>Roles defines the list of roles (at least 2 required).
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
    
  
<tr><td><code>roleStatuses</code><br/>
<a href="#disaggregatedset-x-k8s-io-v1-RoleStatus"><code>[]RoleStatus</code></a>
</td>
<td>
   <p>RoleStatuses contains the status for each role.
The order matches spec.roles.</p>
</td>
</tr>
<tr><td><code>conditions</code><br/>
<a href="https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta"><code>[]k8s.io/apimachinery/pkg/apis/meta/v1.Condition</code></a>
</td>
<td>
   <p>conditions represent the current state of the DisaggregatedSet resource.
Each condition has a unique type and reflects the status of a specific aspect of the resource.</p>
<p>Standard condition types include:</p>
<ul>
<li>&quot;Available&quot;: the resource is fully functional</li>
<li>&quot;Progressing&quot;: the resource is being created or updated</li>
<li>&quot;Degraded&quot;: the resource failed to reach or maintain its desired state</li>
</ul>
<p>The status of each condition is one of True, False, or Unknown.</p>
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


<p>RoleStatus defines the observed state of a single role.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>name</code> <B>[Required]</B><br/>
<code>string</code>
</td>
<td>
   <p>Name is the name of the role (matches spec.roles[].name).</p>
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
   <p>ReadyReplicas is the number of ready replicas for this role.</p>
</td>
</tr>
<tr><td><code>updatedReplicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>UpdatedReplicas is the number of replicas updated to the latest revision.</p>
</td>
</tr>
</tbody>
</table>
  