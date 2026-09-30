---
title: "LeaderWorkerSet"
linkTitle: "LeaderWorkerSet"
weight: 1
description: >
  LeaderWorkerSet examples, including multi-node inference, autoscaling,
  topology-aware placement, and native gang scheduling.
---

These guides deploy a distributed, multi-node inference service with
LeaderWorkerSet, spreading tensor and pipeline parallelism across the leader and
worker pods. Each guide isolates one feature and ships both a `vllm.yaml` and a
`sglang.yaml`.

- [Basic](basic/): minimal multi-node deployment (no Ray).
- [Autoscaling](autoscaling/): scale replica groups with a HorizontalPodAutoscaler.
- [Topology-aware scheduling](topology-aware-scheduling/): pin each group to one
  topology domain with `exclusive-topology`.
- [Gang scheduling](gang-scheduling/): schedule each leader-worker replica as
  an all-or-nothing gang with native Kubernetes scheduling.
