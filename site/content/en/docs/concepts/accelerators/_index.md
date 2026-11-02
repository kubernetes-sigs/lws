---
title: "Accelerators"
linkTitle: "Accelerators"
weight: 40
description: >
  Accelerator-specific integrations across LeaderWorkerSet and DisaggregatedSet.
---

Core [LeaderWorkerSet](../leaderworkerset/) and [DisaggregatedSet](../disaggregatedset/) primitives — such as group lifecycle management, [subgroups](../leaderworkerset/subgroups/), [exclusive topology placement](../../examples/leaderworkerset/topology-aware-scheduling/), [slice placement policies](../disaggregatedset/placement-policy/), and [gang scheduling](../leaderworkerset/gang-scheduling/) — are hardware-neutral and work across GPUs, TPUs, and other AI accelerators using standard Kubernetes resources and node labels.

In addition to those portable primitives, LWS provides built-in integrations for accelerator platforms that require vendor-specific pod wiring or cluster resources across both `LeaderWorkerSet` and `DisaggregatedSet` workloads.
