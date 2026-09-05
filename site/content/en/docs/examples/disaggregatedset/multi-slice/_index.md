---
title: "Multi-slice"
linkTitle: "Multi-slice"
weight: 3
description: >
  Fan a disaggregated role set out into independent slices with spec.slices.
---

This guide is the [basic](../basic/) deployment plus slice fan-out. `spec.slices:
2` replicates the whole prefill/decode role set into two independent slices, so
each slice is a self-contained prefill+decode unit. Combine this with
[topology-aware scheduling](../topology-aware-scheduling/) to co-locate each
slice's roles and spread slices across domains.

Slice fan-out and external role autoscaling are mutually exclusive: external
scaling requires `spec.slices: 1`.

## Deploy

### vLLM

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/multi-slice/vllm.yaml -s | envsubst | kubectl apply -f -
```

### SGLang

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/multi-slice/sglang.yaml -s | envsubst | kubectl apply -f -
```

Verify the slices:

```shell
kubectl get leaderworkersets
```

See the [slices concepts](../../../concepts/disaggregatedset/slices/) for details.
