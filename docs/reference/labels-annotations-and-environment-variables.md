---
title: "Labels, Annotations and Environment Variables"
linkTitle: "Labels, Annotations and Environment Variables"
date: 2025-03-14
---

This page serves as a reference for all labels, annotations and environment variables in LWS.

# Labels

| Label                                      | Note                                                                 | Example                        | Applies to                   |
|--------------------------------------------|----------------------------------------------------------------------|--------------------------------|------------------------------|
| leaderworkerset.sigs.k8s.io/name           | The name of the LeaderWorkerSet object to which these resources belong. | leaderworkerset-multi-template | Pod, Statefulset, Service             |
| leaderworkerset.sigs.k8s.io/template-revision-hash | Hash used to track the controller revision that matches a LeaderWorkerSet object. | 5c5fcdfb44                     | Pod, Statefulset             |
| leaderworkerset.sigs.k8s.io/group-index    | The group to which it belongs.                                       | 0                              | Pod, Statefulset (only worker) |
| leaderworkerset.sigs.k8s.io/group-key      | Unique key identifying the group.                                    | 689ce1b5...b07                 | Pod, Statefulset (only worker) |
| leaderworkerset.sigs.k8s.io/worker-index   | The index or identity of the pod within the group.                   | 0                              | Pod                          |
| leaderworkerset.sigs.k8s.io/subgroup-index | Tracks which subgroup the pod is part of.                            | 0                              | Pod (only if SubGroup is set) |
| leaderworkerset.sigs.k8s.io/subgroup-key   | Pods that are part of the same subgroup will have the same unique hash value. | 92904e74...801                 | Pod (only if SubGroup is set) |

# Annotations

| Label                                        | Note                                                                 | Example                        | Applies to                   |
|----------------------------------------------|----------------------------------------------------------------------|--------------------------------|------------------------------|
| leaderworkerset.sigs.k8s.io/size             | Corresponds to LeaderWorkerSet.Spec.LeaderWorkerTemplate.Size.       | 4                              | Pod                          |
| leaderworkerset.sigs.k8s.io/replicas         | Corresponds to LeaderWorkerSet.Spec.Replicas.                        | 3                              | Statefulset (only leader)    |
| leaderworkerset.sigs.k8s.io/leader-name      | The name of the leader pod.                                          | leaderworkerset-multi-template-0 | Pod (only worker)            |
| leaderworkerset.sigs.k8s.io/exclusive-topology | Specifies the topology for 1:1 exclusive scheduling.                 | cloud.google.com/gke-nodepool  | LeaderWorkerSet, Pod (only if exclusive-topology is used) |
| leaderworkerset.sigs.k8s.io/subdomainPolicy  | Corresponds to LeaderWorkerSet.Spec.NetworkConfig.SubdomainPolicy.   | UniquePerReplica               | Pod (only if leader and subdomainPolicy set to UniquePerReplica) |
| leaderworkerset.sigs.k8s.io/subgroup-size      | Corresponds to LeaderWorkerSet.Spec.SubGroupPolicy.SubGroupSize.     | 2                              | Pod (only if SubGroup is set) |
| leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology | Specifies the topology for 1:1 exclusive scheduling in a given subgroup. | topologyKey                    | LeaderWorkerSet, Pod (only if SubGroup is set and subgroup-exclusive-topology is used) |
| leaderworkerset.sigs.k8s.io/leader-requests-tpus | Whether the leader pod requests TPU.                                 | true                           | Pod (only if leader pod requests TPU) |

# Environment Variables

| Label            | Note                                                                 | Example                                                                                       | Applies to |
|------------------|----------------------------------------------------------------------|-----------------------------------------------------------------------------------------------|------------|
| LwsLeaderAddress | The address of the leader via the headless service.                  | leaderworkerset-multi-template-0.leaderworkerset-multi-template.default                       | Pod        |
| LwsGroupSize     | Tracks the size of the LWS group.                                    | 4                                                                                             | Pod        |
| LwsWorkerIndex   | The index or identity of the pod within the group.                   | 2                                                                                             | Pod        |
| TpuWorkerHostNames | Hostnames of TPU workers only in the same subgroup.                | test-sample-1-5.default,test-sample-1-6.default,test-sample-1-7.default,test-sample-1-8.default | Pod (only if TPU enabled) |
| TpuWorkerId      | ID of the TPU worker.                                                | 0                                                                                             | Pod (only if TPU enabled) |
| TpuName          | Name of the TPU.                                                     | test-sample-1                                                                                 | Pod (only if TPU enabled) |

If you want to use more environment variables, they are available in the labels or annotations but not listed in the Environment Variables section. We can obtain the index by using the [Downward API](https://kubernetes.io/docs/concepts/workloads/pods/downward-api/) to pass the Pod's label as an environment variable to the container:
