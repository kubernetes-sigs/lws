---
title: LeaderWorkerSet API
content_type: tool-reference
package: leaderworkerset.x-k8s.io/v1
auto_generated: true
description: Generated API reference documentation for leaderworkerset.x-k8s.io/v1.
---


## Resource Types 


- [LeaderWorkerSet](#leaderworkerset-x-k8s-io-v1-LeaderWorkerSet)
  

## `LeaderWorkerSet`     {#leaderworkerset-x-k8s-io-v1-LeaderWorkerSet}
    

**Appears in:**



<p>LeaderWorkerSet is the Schema for the leaderworkersets API</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
<tr><td><code>apiVersion</code><br/>string</td><td><code>leaderworkerset.x-k8s.io/v1</code></td></tr>
<tr><td><code>kind</code><br/>string</td><td><code>LeaderWorkerSet</code></td></tr>
    
  
<tr><td><code>spec</code> <B>[Required]</B><br/>
<a href="#leaderworkerset-x-k8s-io-v1-LeaderWorkerSetSpec"><code>LeaderWorkerSetSpec</code></a>
</td>
<td>
   <span class="text-muted">No description provided.</span></td>
</tr>
<tr><td><code>status</code> <B>[Required]</B><br/>
<a href="#leaderworkerset-x-k8s-io-v1-LeaderWorkerSetStatus"><code>LeaderWorkerSetStatus</code></a>
</td>
<td>
   <span class="text-muted">No description provided.</span></td>
</tr>
</tbody>
</table>

## `LeaderWorkerSetSpec`     {#leaderworkerset-x-k8s-io-v1-LeaderWorkerSetSpec}
    

**Appears in:**

- [LeaderWorkerSet](#leaderworkerset-x-k8s-io-v1-LeaderWorkerSet)


<p>One group consists of a single leader and M workers, and the total number of pods in a group is M+1.
LeaderWorkerSet will create N replicas of leader-worker pod groups (hereinafter referred to as group).</p>
<p>Each group has a unique index between 0 and N-1. We call this the leaderIndex.
The leaderIndex is used to uniquely name the leader pod of each group in the following format:
leaderWorkerSetName-leaderIndex. This is considered as the name of the group too.</p>
<p>Each worker pod in the group has a unique workerIndex between 1 and M. The leader also
gets a workerIndex, and it is always set to 0.
Worker pods are named using the format: leaderWorkerSetName-leaderIndex-workerIndex.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>replicas</code><br/>
<code>int32</code>
</td>
<td>
   <p>Number of leader-workers groups. A scale subresource is available to enable HPA. The
selector for HPA will be that of the leader pod, and so practically HPA will be looking up the
leader pod metrics. Note that the leader pod could aggregate metrics from
the rest of the group and expose them as a summary custom metric representing the whole
group.
On scale down, the leader pod as well as the workers statefulset will be deleted.
Default to 1.</p>
</td>
</tr>
<tr><td><code>leaderWorkerTemplate</code> <B>[Required]</B><br/>
<a href="#leaderworkerset-x-k8s-io-v1-LeaderWorkerTemplate"><code>LeaderWorkerTemplate</code></a>
</td>
<td>
   <p>LeaderWorkerTemplate defines the template for leader/worker pods</p>
</td>
</tr>
<tr><td><code>rolloutStrategy</code><br/>
<a href="#leaderworkerset-x-k8s-io-v1-RolloutStrategy"><code>RolloutStrategy</code></a>
</td>
<td>
   <p>RolloutStrategy defines the strategy that will be applied to update replicas
when a revision is made to the leaderWorkerTemplate.</p>
</td>
</tr>
<tr><td><code>startupPolicy</code><br/>
<a href="#leaderworkerset-x-k8s-io-v1-StartupPolicyType"><code>StartupPolicyType</code></a>
</td>
<td>
   <p>StartupPolicy determines the startup policy for the worker statefulset.</p>
</td>
</tr>
<tr><td><code>networkConfig</code><br/>
<a href="#leaderworkerset-x-k8s-io-v1-NetworkConfig"><code>NetworkConfig</code></a>
</td>
<td>
   <p>NetworkConfig defines the network configuration of the group</p>
</td>
</tr>
</tbody>
</table>

## `LeaderWorkerSetStatus`     {#leaderworkerset-x-k8s-io-v1-LeaderWorkerSetStatus}
    

**Appears in:**

- [LeaderWorkerSet](#leaderworkerset-x-k8s-io-v1-LeaderWorkerSet)


<p>LeaderWorkerSetStatus defines the observed state of LeaderWorkerSet</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>conditions</code> <B>[Required]</B><br/>
<a href="https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta"><code>[]k8s.io/apimachinery/pkg/apis/meta/v1.Condition</code></a>
</td>
<td>
   <p>Conditions track the condition of the leaderworkerset.</p>
</td>
</tr>
<tr><td><code>readyReplicas</code> <B>[Required]</B><br/>
<code>int32</code>
</td>
<td>
   <p>ReadyReplicas track the number of groups that are in ready state (updated or not).</p>
</td>
</tr>
<tr><td><code>updatedReplicas</code> <B>[Required]</B><br/>
<code>int32</code>
</td>
<td>
   <p>UpdatedReplicas track the number of groups that have been updated (ready or not).</p>
</td>
</tr>
<tr><td><code>replicas</code> <B>[Required]</B><br/>
<code>int32</code>
</td>
<td>
   <p>Replicas track the total number of groups that have been created (updated or not, ready or not)</p>
</td>
</tr>
<tr><td><code>hpaPodSelector</code> <B>[Required]</B><br/>
<code>string</code>
</td>
<td>
   <p>HPAPodSelector for pods that belong to the LeaderWorkerSet object, this is
needed for HPA to know what pods belong to the LeaderWorkerSet object. Here
we only select the leader pods.</p>
</td>
</tr>
</tbody>
</table>

## `LeaderWorkerTemplate`     {#leaderworkerset-x-k8s-io-v1-LeaderWorkerTemplate}
    

**Appears in:**

- [LeaderWorkerSetSpec](#leaderworkerset-x-k8s-io-v1-LeaderWorkerSetSpec)


<p>Template of the leader/worker pods, the group will include at least one leader pod.
Defaults to the worker template if not specified. The idea is to allow users to create a
group with identical templates without needing to specify the template in both places.
For the leader it represents the id of the group, while for the workers it represents the
index within the group. For this reason, users should depend on the labels injected by this
API whenever possible.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>leaderTemplate</code> <B>[Required]</B><br/>
<a href="https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#podtemplatespec-v1-core"><code>k8s.io/api/core/v1.PodTemplateSpec</code></a>
</td>
<td>
   <p>LeaderTemplate defines the pod template for leader pods.</p>
</td>
</tr>
<tr><td><code>workerTemplate</code> <B>[Required]</B><br/>
<a href="https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#podtemplatespec-v1-core"><code>k8s.io/api/core/v1.PodTemplateSpec</code></a>
</td>
<td>
   <p>WorkerTemplate defines the pod template for worker pods.</p>
</td>
</tr>
<tr><td><code>size</code><br/>
<code>int32</code>
</td>
<td>
   <p>Number of pods to create. It is the total number of pods in each group.
The minimum is 1 which represent the leader. When set to 1, the leader
pod is created for each group as well as a 0-replica StatefulSet for the workers.
Default to 1.</p>
</td>
</tr>
<tr><td><code>restartPolicy</code><br/>
<a href="#leaderworkerset-x-k8s-io-v1-RestartPolicyType"><code>RestartPolicyType</code></a>
</td>
<td>
   <p>RestartPolicy defines the restart policy when pod failures happen.
The former named Default policy is deprecated, will be removed in the future,
replace with None policy for the same behavior.</p>
</td>
</tr>
<tr><td><code>subGroupPolicy</code><br/>
<a href="#leaderworkerset-x-k8s-io-v1-SubGroupPolicy"><code>SubGroupPolicy</code></a>
</td>
<td>
   <p>SubGroupPolicy describes the policy that will be applied when creating subgroups
in each replica.</p>
</td>
</tr>
<tr><td><code>volumeClaimTemplates</code><br/>
<a href="https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#persistentvolumeclaim-v1-core"><code>[]k8s.io/api/core/v1.PersistentVolumeClaim</code></a>
</td>
<td>
   <p>VolumeClaimTemplates is a list of claims that pods are allowed to reference.
Every claim in this list must have at least one matching (by name) volumeMount
in one container in the template. A claim in this list takes precedence over
any volumes in the template, with the same name.</p>
</td>
</tr>
<tr><td><code>persistentVolumeClaimRetentionPolicy</code><br/>
<a href="https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#statefulsetpersistentvolumeclaimretentionpolicy-v1-apps"><code>k8s.io/api/apps/v1.StatefulSetPersistentVolumeClaimRetentionPolicy</code></a>
</td>
<td>
   <p>PersistentVolumeClaimRetentionPolicy describes the policy used for PVCs created from
the VolumeClaimTemplates.</p>
</td>
</tr>
</tbody>
</table>

## `NetworkConfig`     {#leaderworkerset-x-k8s-io-v1-NetworkConfig}
    

**Appears in:**

- [LeaderWorkerSetSpec](#leaderworkerset-x-k8s-io-v1-LeaderWorkerSetSpec)



<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>subdomainPolicy</code> <B>[Required]</B><br/>
<a href="#leaderworkerset-x-k8s-io-v1-SubdomainPolicy"><code>SubdomainPolicy</code></a>
</td>
<td>
   <p>SubdomainPolicy determines the policy that will be used when creating
the headless service, defaults to shared</p>
</td>
</tr>
</tbody>
</table>

## `RestartPolicyType`     {#leaderworkerset-x-k8s-io-v1-RestartPolicyType}
    
(Alias of `string`)

**Appears in:**

- [LeaderWorkerTemplate](#leaderworkerset-x-k8s-io-v1-LeaderWorkerTemplate)





## `RollingUpdateConfiguration`     {#leaderworkerset-x-k8s-io-v1-RollingUpdateConfiguration}
    

**Appears in:**

- [RolloutStrategy](#leaderworkerset-x-k8s-io-v1-RolloutStrategy)


<p>RollingUpdateConfiguration defines the parameters to be used for RollingUpdateStrategyType.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>partition</code><br/>
<code>int32</code>
</td>
<td>
   <p>Partition indicates the ordinal at which the lws should be partitioned for updates.
During a rolling update, all the groups from ordinal Partition to Replicas-1 will be updated.
The groups from 0 to Partition-1 will not be updated.
This is helpful in incremental rollout strategies like canary deployments
or interactive rollout strategies for multiple replicas like xPyD deployments.
Once partition field and maxSurge field both set, the bursted replicas will keep remaining
until the rolling update is completely done and the partition field is reset to 0.
This is as expected to reduce the reconciling complexity.
The default value is 0.</p>
</td>
</tr>
<tr><td><code>maxUnavailable</code> <B>[Required]</B><br/>
<code>k8s.io/apimachinery/pkg/util/intstr.IntOrString</code>
</td>
<td>
   <p>The maximum number of replicas that can be unavailable during the update.
Value can be an absolute number (ex: 5) or a percentage of total replicas at the start of update (ex: 10%).
Absolute number is calculated from percentage by rounding down.
This can not be 0 if MaxSurge is 0.
By default, a fixed value of 1 is used.
Example: when this is set to 30%, the old replicas can be scaled down by 30%
immediately when the rolling update starts. Once new replicas are ready, old replicas
can be scaled down further, followed by scaling up the new replicas, ensuring
that at least 70% of original number of replicas are available at all times
during the update.</p>
</td>
</tr>
<tr><td><code>maxSurge</code> <B>[Required]</B><br/>
<code>k8s.io/apimachinery/pkg/util/intstr.IntOrString</code>
</td>
<td>
   <p>The maximum number of replicas that can be scheduled above the original number of
replicas.
Value can be an absolute number (ex: 5) or a percentage of total replicas at
the start of the update (ex: 10%).
Absolute number is calculated from percentage by rounding up.
By default, a value of 0 is used.
Example: when this is set to 30%, the new replicas can be scaled up by 30%
immediately when the rolling update starts. Once old replicas have been deleted,
new replicas can be scaled up further, ensuring that total number of replicas running
at any time during the update is at most 130% of original replicas.
When rolling update completes, replicas will fall back to the original replicas.</p>
</td>
</tr>
</tbody>
</table>

## `RolloutStrategy`     {#leaderworkerset-x-k8s-io-v1-RolloutStrategy}
    

**Appears in:**

- [LeaderWorkerSetSpec](#leaderworkerset-x-k8s-io-v1-LeaderWorkerSetSpec)


<p>RolloutStrategy defines the strategy that the leaderWorkerSet controller
will use to perform replica updates.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>type</code> <B>[Required]</B><br/>
<a href="#leaderworkerset-x-k8s-io-v1-RolloutStrategyType"><code>RolloutStrategyType</code></a>
</td>
<td>
   <p>Type defines the rollout strategy, it can only be “RollingUpdate” for now.</p>
</td>
</tr>
<tr><td><code>rollingUpdateConfiguration</code><br/>
<a href="#leaderworkerset-x-k8s-io-v1-RollingUpdateConfiguration"><code>RollingUpdateConfiguration</code></a>
</td>
<td>
   <p>RollingUpdateConfiguration defines the parameters to be used when type is RollingUpdateStrategyType.</p>
</td>
</tr>
</tbody>
</table>

## `RolloutStrategyType`     {#leaderworkerset-x-k8s-io-v1-RolloutStrategyType}
    
(Alias of `string`)

**Appears in:**

- [RolloutStrategy](#leaderworkerset-x-k8s-io-v1-RolloutStrategy)





## `StartupPolicyType`     {#leaderworkerset-x-k8s-io-v1-StartupPolicyType}
    
(Alias of `string`)

**Appears in:**

- [LeaderWorkerSetSpec](#leaderworkerset-x-k8s-io-v1-LeaderWorkerSetSpec)





## `SubGroupPolicy`     {#leaderworkerset-x-k8s-io-v1-SubGroupPolicy}
    

**Appears in:**

- [LeaderWorkerTemplate](#leaderworkerset-x-k8s-io-v1-LeaderWorkerTemplate)


<p>SubGroupPolicy describes the policy that will be applied when creating subgroups.</p>


<table class="table">
<thead><tr><th width="30%">Field</th><th>Description</th></tr></thead>
<tbody>
    
  
<tr><td><code>subGroupPolicyType</code><br/>
<a href="#leaderworkerset-x-k8s-io-v1-SubGroupPolicyType"><code>SubGroupPolicyType</code></a>
</td>
<td>
   <p>Defines what type of Subgroups to create. Defaults to
LeaderWorker</p>
</td>
</tr>
<tr><td><code>subGroupSize</code> <B>[Required]</B><br/>
<code>int32</code>
</td>
<td>
   <p>The number of pods per subgroup. This value is immutable,
and must not be greater than LeaderWorkerSet.Spec.Size.
Size must be divisible by subGroupSize in which case the
subgroups will be of equal size. Or size - 1 is divisible
by subGroupSize, in which case the leader is considered as
the extra pod, and will be part of the first subgroup.</p>
</td>
</tr>
</tbody>
</table>

## `SubGroupPolicyType`     {#leaderworkerset-x-k8s-io-v1-SubGroupPolicyType}
    
(Alias of `string`)

**Appears in:**

- [SubGroupPolicy](#leaderworkerset-x-k8s-io-v1-SubGroupPolicy)





## `SubdomainPolicy`     {#leaderworkerset-x-k8s-io-v1-SubdomainPolicy}
    
(Alias of `string`)

**Appears in:**

- [NetworkConfig](#leaderworkerset-x-k8s-io-v1-NetworkConfig)




  