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
<tr><td><code>LeaderWorkerSetTemplateSpec</code> <B>[Required]</B><br/>
<code>sigs.k8s.io/lws/api/leaderworkerset/v1.LeaderWorkerSetTemplateSpec</code>
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

## `DisaggregatedSetSpec`     {#disaggregatedset-x-k8s-io-v1-DisaggregatedSetSpec}
    

**Appears in:**

- [DisaggregatedSet](#disaggregatedset-x-k8s-io-v1-DisaggregatedSet)


<p>DisaggregatedSetSpec defines the desired state of DisaggregatedSet</p>


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
  