---
title: "Distributed Inference APIs"
linkTitle: "Overview"
weight: 1
menu:
  main:
    weight: 20
description: >
  An overview of the LeaderWorkerSet (LWS) and DisaggregatedSet (DS) APIs
---

<img src="/images/lws-ds-logos.svg" width="300" alt="lws logo">

**[LeaderWorkerSet (LWS)](../concepts/leaderworkerset/):** An API for deploying a group of pods as a unit of replication. It aims to address common deployment patterns of AI/ML inference workloads, especially multi-host inference workloads where the LLM will be sharded and run across multiple devices on multiple nodes.

**[DisaggregatedSet (DS)](../concepts/disaggregatedset/):** An API to support advanced multi-node inference. LWS forms the core API for multi-node while DisaggregatedSet builds on it to add advanced disaggregated workload deployment with support for autoscaling, rollouts and failure handling.

Both APIs are being co-designed with [<img src="https://raw.githubusercontent.com/llm-d/llm-d/main/docs/assets/images/llm-d-logo.png" width="60" style="vertical-align: middle" alt="llm-d logo">](https://github.com/llm-d/llm-d) (CNCF sandbox project). llm-d is a high-performance distributed inference serving stack optimized for production deployments. This collaboration ensures that the APIs are optimized for real-world serving frameworks and disaggregated architectures, helping achieve state-of-the-art performance across hardware accelerators.

Read the [documentation](/docs/) or watch the [related talks & presentations](../adoption/#talks-and-presentations) to learn more.

## Feature Overview

### Core Features: LeaderWorkerSet
- **Group of Pods as a unit:** Supports a tightly managed group of pods that represent a “super pod”
  - **Unique pod identity:** Each pod in the group has a unique index from 0 to n-1.
  - **Parallel creation:** Pods in the group will have the same lifecycle and be created in parallel.
  - **Gang Scheduling:** Each replica with a group of pods can be scheduled in an all-or-nothing manner (Alpha level, API may change in the future).
- **Dual-template, one for leader and one for the workers:** A replica is a group of a single leader and a set of workers, and allow to specify a template for the workers and optionally use a second one for the leader pod.
- **Multiple groups with identical specifications:** Supports creating multiple “replicas” of the above mentioned group. Each group is a single unit for rolling update, scaling, and maps to a single exclusive topology for placement.
- **A scale subresource:** A scale endpoint is exposed for HPA to dynamically scale the number replicas (aka number of groups)
- **Rollout and Rolling update:** Supports performing rollout and rolling update at the group level, which means the groups are upgraded one by one as a unit (i.e. the pods within a group are updated together).
- **Topology-aware placement:** Opt-in support for pods in the same group to be co-located in the same topology.
- **All-or-nothing restart for failure handling:** Opt-in support for all pods in the group to be recreated if one pod in the group failed or one container in the pods is restarted.

<p align="center">
  <img src="/images/lws-concept.svg" width="550" alt="LWS Concept">
</p>

### Advanced Features: DisaggregatedSet
- **Disaggregated Architecture Support:** Specifically designed for workloads where different phases (e.g., prefill and decode) run on separate infrastructure.
- **Coordinated N-Dimensional Rollouts:** Updates multiple roles (2-10) in lockstep, preserving capacity ratios throughout the update process.
- **Unified Lifecycle Management:** Manages multiple underlying LeaderWorkerSets as a single logical unit.
- **Automatic Service Orchestration:** Automatically creates and manages headless services for each role to facilitate discovery and revision-aware routing.
- **Advanced Failure Handling:** Coordinated drain and restart policies across all roles in the disaggregated set.

<p align="center">
  <img src="/images/ds-concept.svg" width="700" alt="DisaggregatedSet Concept">
</p>

## Installation

Read the [installation guide](../installation/) to learn more.

## Examples

Read the [examples](../examples/) to learn more.

Also discover adopters, integrations, and talks [here](../adoption/#talks-and-presentations).

## Community, Discussion, Contribution, and Support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack](https://kubernetes.slack.com/messages/sig-apps)
- [Mailing List](https://groups.google.com/g/kubernetes-sig-apps)

### Code of Conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](https://github.com/kubernetes/community/blob/master/code-of-conduct.md).
