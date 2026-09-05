---
title: "Topology-aware scheduling"
linkTitle: "Topology-aware scheduling"
weight: 4
description: >
  Co-locate a slice's roles in one topology domain with placementPolicy.
---

This guide is the [basic](../basic/) deployment plus topology-aware placement.
`spec.placementPolicy` with `type: ExclusiveSlice` co-locates a slice's roles in
one topology domain and spreads slices across domains, which raises bandwidth
between the prefill and decode roles that exchange the KV cache.

Set `placementPolicy.topology` to your cluster's topology key (the example uses
`cloud.google.com/gke-nodepool`). Nodes must be labeled with that key.

## Deploy

### vLLM

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/topology-aware-scheduling/vllm.yaml -s | envsubst | kubectl apply -f -
```

### SGLang

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/topology-aware-scheduling/sglang.yaml -s | envsubst | kubectl apply -f -
```

See the [placement policy concepts](../../../concepts/disaggregatedset/placement-policy/)
for the full set of placement types.
