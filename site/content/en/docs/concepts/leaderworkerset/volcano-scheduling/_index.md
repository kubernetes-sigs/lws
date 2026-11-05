---
title: "Volcano Network Topology"
linkTitle: "Volcano Network Topology"
weight: 36
description: >
  Configure network topology for per-replica Volcano PodGroups.
---

The Volcano provider creates one PodGroup for each LWS replica. The annotations
below let you keep each replica's pods within a rack or another topology domain.

## Prerequisites

Volcano must have network topology scheduling and HyperNodes configured, and
the selected Queue must be open. Choose the tier that represents the desired
domain in your cluster. The example below assumes tier `1` represents a rack.

Enable the LWS Volcano provider with these Helm values:

```yaml
gangSchedulingManagement:
  schedulerProvider: volcano
```

When you enable this provider, LWS also creates Volcano PodGroups for newly
created pods that use other schedulers. Without the topology annotations, those
PodGroups have no `networkTopology` setting.

## Configure a LeaderWorkerSet

Add these fields to your LWS manifest. Containers and resource requests are
omitted here:

```yaml
metadata:
  annotations:
    scheduling.volcano.sh/queue-name: default
    volcano.sh/network-topology-highest-tier-allowed: "1"
    volcano.sh/network-topology-mode: hard
spec:
  replicas: 2
  startupPolicy: LeaderCreated
  leaderWorkerTemplate:
    size: 4
    leaderTemplate:
      spec:
        schedulerName: volcano
    workerTemplate:
      spec:
        schedulerName: volcano
```

This creates two PodGroups, each with `minMember: 4` and:

```yaml
spec:
  networkTopology:
    mode: hard
    highestTierAllowed: 1
```

Each replica's four pods must fit within one rack. The two replicas can use
different racks. Use `LeaderCreated` to schedule the leader and workers as a
gang. `LeaderReady` sets `minMember` to `1`.

The tier must be a non-negative integer. Mode defaults to `hard`. Set mode to
`soft` to allow placement across topology domains. Mode requires the tier
annotation.

When the Volcano provider is enabled, the LWS webhook rejects invalid topology
annotations on create and when these annotations change on update. If an
existing LWS has invalid values, updates that leave both annotations unchanged
are still allowed, including scaling. Invalid values stop PodGroup creation.
For workloads with `spec.scheduling`, the LWS controller also reports
`WorkloadSchedulingCreated=False` with reason `InvalidSchedulingConfiguration`.

## Existing workloads

Annotation updates do not change current PodGroups or restart their pods. LWS
applies the new values only when it creates new PodGroups. To change the
topology policy for the whole workload, create a new LWS.
