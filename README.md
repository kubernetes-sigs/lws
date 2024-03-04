# lws

LeaderWorkerSet: An API for deploying a group of pods as a unit of replication. It aims to address common deployment patterns of AI/ML inference workloads, especially multi-host inference workloads where the LLM will be sharded and run across devices. 
Design and proposal can be found here: http://bit.ly/k8s-LWS.

## Feature overview

- **Group of Pods as a unit:** A tightly managed group of pods that represent a “super pod” with the requirements
  - **Unique pod identity:** Each pod in the group has a unique index from 0 to n-1.
  - **Parallel creation:** Pods in the group will have the same lifecycle and be created in parallel.
- **Dual-template, one for leader and one for the workers:** The group is composed of a single leader and a set of workers, and the api should allow to specify a template for the workers and optionally use a second one for the leader pod.
- **Multiple groups with identical specifications:** We should be able to create multiple “replicas” of the above mentioned group. Each group is a single unit for rolling update, scaling, and maps to a single exclusive topology for placement. 
- **A scale subresource:** We will expose a scale endpoint for HPA to dynamically scale the number replicas (aka number of groups)
- **Rollout and Rolling update:** It will be performed at the group level, which means we upgrade the groups one by one as a unit (i.e. the pods within a group are updated together).
- **Topology-aware placement:** Be able to ensure that pods in the same group will be co-located in the same topology.
- **All-or-nothing restart for failure handling:** Certain ML inference stacks require all pods in the group to be recreated if one pod in the group failed or one container in the pods is restarted.

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack](https://kubernetes.slack.com/messages/sig-apps)
- [Mailing List](https://groups.google.com/g/kubernetes-sig-apps)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

[owners]: https://git.k8s.io/community/contributors/guide/owners.md
[Creative Commons 4.0]: https://git.k8s.io/website/LICENSE
