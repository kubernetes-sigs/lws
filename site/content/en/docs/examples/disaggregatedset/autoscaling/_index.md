---
title: "Autoscaling"
linkTitle: "Autoscaling"
weight: 2
description: >
  Autoscale a disaggregated role with a DisaggregatedSetRoleScaler and HPA.
---

This guide is the [basic](../basic/) deployment plus per-role autoscaling. The
`prefill` role sets `scaling.mode: External`, so the controller creates a
`DisaggregatedSetRoleScaler` named `<ds>-prefill` with a `/scale` subresource.
The bundled HorizontalPodAutoscaler targets that scaler (stable across
rollouts), reads leader pod metrics, and scales between 2 and 8 replicas at 70%
CPU. It needs [metrics-server](https://github.com/kubernetes-sigs/metrics-server).

External scaling requires `spec.slices: 1`.

## Deploy

### vLLM

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/autoscaling/vllm.yaml -s | envsubst | kubectl apply -f -
```

### SGLang

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/autoscaling/sglang.yaml -s | envsubst | kubectl apply -f -
```

Watch the scaler and HPA:

```shell
kubectl get disaggregatedsetrolescaler
kubectl get hpa -w
```

KEDA can drive the same `DisaggregatedSetRoleScaler` in place of the HPA: point a
KEDA `ScaledObject` at the `<ds>-prefill` scaler. Don't target the role's child
LeaderWorkerSet directly — the DisaggregatedSet controller owns each role's
replica count and overwrites external changes on every reconcile. See the
[role scaler concepts](../../../concepts/disaggregatedset/role-scaler/).
