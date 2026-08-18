---
title: "LeaderWorkerSet"
linkTitle: "LeaderWorkerSet"
weight: 10
description: >
  Core concepts of LeaderWorkerSet (LWS) — unit of replication, pod templates, startup policies, topology placement, subgroups, and lifecycle management.
---

**LeaderWorkerSet (LWS)** is a Kubernetes API designed to deploy and manage a group of pods as a single **unit of replication**. It addresses common deployment patterns of distributed AI/ML workloads — such as multi-host inference and distributed fine-tuning — where a model is sharded across multiple accelerators spanning multiple nodes that must be scheduled, scaled, and managed together.

<p align="center">
  <img src="/images/lws-concept.svg" width="550" alt="LWS Concept">
</p>
