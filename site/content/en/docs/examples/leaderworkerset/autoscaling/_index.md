---
title: "Autoscaling"
linkTitle: "Autoscaling"
weight: 2
description: >
  Scale LeaderWorkerSet replica groups with a HorizontalPodAutoscaler.
aliases:
- /docs/examples/hpa/
---

This guide is the [basic](../basic/) deployment plus a HorizontalPodAutoscaler.
The HPA scales the *number of replica groups* through the LWS `scale`
subresource (it monitors leader pods only), between `minReplicas: 2` and
`maxReplicas: 5` at 50% CPU utilization. It needs
[metrics-server](https://github.com/kubernetes-sigs/metrics-server).

## Deploy

### vLLM

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/autoscaling/vllm.yaml -s | envsubst | kubectl apply -f -
```

### SGLang

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/autoscaling/sglang.yaml -s | envsubst | kubectl apply -f -
```

Watch the HPA react to load:

```shell
kubectl get hpa -w
```

See [basic](../basic/) for how to reach the service once pods are running.
