---
title: "Topology-aware scheduling"
linkTitle: "Topology-aware scheduling"
weight: 3
description: >
  Pin each replica group to a single topology domain with exclusive-topology.
aliases:
- /docs/examples/tas/
---

This guide is the [basic](../basic/) deployment plus topology-aware placement.
The `leaderworkerset.sigs.k8s.io/exclusive-topology` annotation keeps each
replica group within one topology domain and excludes other groups from it,
which raises pod-to-pod bandwidth for tensor and pipeline parallelism.

Set the annotation value to your cluster's topology key (the example uses
`cloud.google.com/gke-nodepool`). Nodes must be labeled with that key.

## Deploy

### vLLM

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/topology-aware-scheduling/vllm.yaml -s | envsubst | kubectl apply -f -
```

### SGLang

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/topology-aware-scheduling/sglang.yaml -s | envsubst | kubectl apply -f -
```

See [basic](../basic/) for how to reach the service once pods are running.
