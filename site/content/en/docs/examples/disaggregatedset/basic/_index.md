---
title: "Basic"
linkTitle: "Basic"
weight: 1
description: >
  Minimal prefill/decode disaggregation on DisaggregatedSet with vLLM and SGLang.
---

The basic guide runs a disaggregated prefill/decode deployment where each role
is its own child LeaderWorkerSet. Prompt processing (`prefill`) and token
generation (`decode`) scale and roll out on their own.

- **vLLM** streams the KV cache from prefill to decode via
  `--kv-transfer-config`. The connector (NIXL, LMCache, etc.) and the
  prefill&lt;-&gt;decode wiring are deployment-specific; the config here is a
  starting point, not a turnkey value.
- **SGLang** runs PD disaggregation via `--disaggregation-mode`. It needs a
  router/load balancer (for example `sglang-router` in mini-lb mode) to connect
  prefill and decode. That wiring is deployment-specific and omitted here. See
  the [llm-d P/D disaggregation guide](https://github.com/llm-d/llm-d/blob/main/guides/pd-disaggregation/README.md).

The other guides build on this one: [autoscaling](../autoscaling/) adds a
per-role scaler and HPA, [multi-slice](../multi-slice/) fans the role set out
into independent slices, and [topology-aware scheduling](../topology-aware-scheduling/)
co-locates a slice's roles in one topology domain.

## Deploy

### vLLM

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/basic/vllm.yaml -s | envsubst | kubectl apply -f -
```

### SGLang

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/basic/sglang.yaml -s | envsubst | kubectl apply -f -
```

Verify the child LeaderWorkerSets and pods:

```shell
kubectl get leaderworkersets
kubectl get pods
```

Roll out a role by editing its container spec; the per-role `rolloutStrategy`
upgrades that role independently of the others. See the
[DisaggregatedSet concepts](../../../concepts/disaggregatedset/) for the API
details.
