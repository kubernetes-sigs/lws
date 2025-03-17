---
title: "Labels, Annotations and Environment Variables"
linkTitle: "Labels, Annotations and Environment Variables"
date: 2025-03-14
description:  A reference for all labels, annotations, and environment variables in LWS.
---

# Labels

| Key                                        | Description                                                       | Example                        | Applies to                   |
|--------------------------------------------|----------------------------------------------------------------------|--------------------------------|------------------------------|
| leaderworkerset.sigs.k8s.io/name           | The name of the LeaderWorkerSet object to which these resources belong. | leaderworkerset-multi-template | Pod, Statefulset, Service             |
| leaderworkerset.sigs.k8s.io/template-revision-hash | Hash used to track the controller revision that matches a LeaderWorkerSet object. | 5c5fcdfb44                     | Pod, Statefulset             |
| leaderworkerset.sigs.k8s.io/group-index    | The group to which it belongs.                                       | 0                              | Pod, Statefulset (only worker) |
| leaderworkerset.sigs.k8s.io/group-key      | Unique key identifying the group.                                    | 689ce1b5...b07                 | Pod, Statefulset (only worker) |
| leaderworkerset.sigs.k8s.io/worker-index   | The index or identity of the pod within the group.                   | 0                              | Pod                          |
| leaderworkerset.sigs.k8s.io/subgroup-index | Tracks which subgroup the pod is part of.                            | 0                              | Pod (only if SubGroup is set) |
| leaderworkerset.sigs.k8s.io/subgroup-key   | Pods that are part of the same subgroup will have the same unique hash value. | 92904e74...801                 | Pod (only if SubGroup is set) |

# Annotations

| Key                                          | Description                                                       | Example                        | Applies to                   |
|----------------------------------------------|----------------------------------------------------------------------|--------------------------------|------------------------------|
| leaderworkerset.sigs.k8s.io/size             | The total number of pods in each group.      | 4                              | Pod                          |
| leaderworkerset.sigs.k8s.io/replicas         | Replicas Number of leader-workers groups.                        | 3                              | Statefulset (only leader)    |
| leaderworkerset.sigs.k8s.io/leader-name      | The name of the leader pod.                                          | leaderworkerset-multi-template-0 | Pod (only worker)            |
| leaderworkerset.sigs.k8s.io/exclusive-topology | Specifies the topology for exclusive 1:1 scheduling.                 | cloud.google.com/gke-nodepool  | LeaderWorkerSet, Pod (only if exclusive-topology is used) |
| leaderworkerset.sigs.k8s.io/subdomainPolicy  | Determines what type of domain will be injected.   | UniquePerReplica               | Pod (only if leader and subdomainPolicy set to UniquePerReplica) |
| leaderworkerset.sigs.k8s.io/subgroup-size    | The number of pods per subgroup.                                     | 2                              | Pod (only if SubGroup is set) |
| leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology | Specifies the topology for exclusive 1:1 scheduling within a subgroup. | topologyKey                    | LeaderWorkerSet, Pod (only if SubGroup is set and subgroup-exclusive-topology is used) |
| leaderworkerset.sigs.k8s.io/leader-requests-tpus | Indicates if the leader pod requests TPU.                            | true                           | Pod (only if leader pod requests TPU) |

# Environment Variables

| Key              | Description                                                       | Example                                                                                       | Applies to |
|------------------|----------------------------------------------------------------------|-----------------------------------------------------------------------------------------------|------------|
| LWS_LEADER_ADDRESS | The address of the leader via the headless service.                  | leaderworkerset-multi-template-0.leaderworkerset-multi-template.default                       | Pod        |
| LWS_GROUP_SIZE     | Tracks the size of the LWS group.                                    | 4                                                                                             | Pod        |
| LWS_WORKER_INDEX   | The index or identity of the pod within the group.                   | 2                                                                                             | Pod        |
| TPU_WORKER_HOSTNAMES | Hostnames of TPU workers only in the same subgroup.                | test-sample-1-5.default,test-sample-1-6.default,test-sample-1-7.default,test-sample-1-8.default | Pod (only if TPU enabled) |
| TPU_WORKER_ID      | ID of the TPU worker.                                                | 0                                                                                             | Pod (only if TPU enabled) |
| TPU_NAME          | Name of the TPU.                                                     | test-sample-1                                                                                 | Pod (only if TPU enabled) |

If you want to use more environment variables, they are available in the labels or annotations but not listed in the Environment Variables section. 
We can obtain the index by using the [Downward API](https://kubernetes.io/docs/concepts/workloads/pods/downward-api/) to pass the Pod's label as an environment variable to the container.
