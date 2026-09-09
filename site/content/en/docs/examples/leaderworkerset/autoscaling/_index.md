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

{{% tabpane text=true %}}
{{% tab header="vLLM" %}}
```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/autoscaling/vllm.yaml -s | envsubst | kubectl apply -f -
```
{{% /tab %}}
{{% tab header="SGLang" %}}
```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/autoscaling/sglang.yaml -s | envsubst | kubectl apply -f -
```
{{% /tab %}}
{{% tab header="nginx (no GPU)" %}}
```shell
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/autoscaling/nginx.yaml
```
{{% /tab %}}
{{% /tabpane %}}

The vLLM and SGLang examples need GPUs and a Hugging Face token. The nginx one
runs on any cluster with metrics-server, including kind, so it is the quickest
way to see the behavior described here.

Watch the HPA react to load:

```shell
kubectl get hpa -w
```

See [basic](../basic/) for how to reach the service once pods are running.

## Resource requests are required

The HPA computes utilization as a percentage of a pod's resource *requests*.
If a container has no request for the metric being targeted, the HPA reports
`<unknown>` for it and will not scale. Every container in these examples sets
both requests and limits for that reason.

## Scaling on other metrics

The examples above scale on CPU. Two variants of the same HPA are included for
the nginx deployment, one on memory and one on CPU and memory together.

An HPA takes ownership of its target's replica count, so pointing a second one
at the same LeaderWorkerSet makes the two fight. Delete the CPU HPA before
applying a variant:

```shell
kubectl delete hpa lws-hpa
```

{{% tabpane text=true %}}
{{% tab header="Memory" %}}
```shell
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/autoscaling/nginx-memory-hpa.yaml
```
{{% /tab %}}
{{% tab header="CPU + memory" %}}
```shell
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/autoscaling/nginx-multi-metric-hpa.yaml
```
{{% /tab %}}
{{% /tabpane %}}

With several metrics the HPA computes a desired replica count for each and
takes the largest, so either one alone can scale the deployment up.

The CPU + memory variant also sets `behavior`, which is worth tuning for LWS in
particular: each replica is a whole group, so scaling down aggressively tears
down a leader and its workers together. It scales up immediately but waits five
minutes below target before scaling down.

## Generating load

The HPA reads *leader* pods only, so load has to land on a leader to trigger
scaling. With the nginx deployment running:

```shell
kubectl exec leaderworkerset-sample-0 -- /bin/sh -c "for i in \$(seq 1 4); do yes > /dev/null & done"
```

CPU utilization should cross the 50% target within a couple of minutes and the
replica count should climb. To clear the load, restart the pod, since the nginx
image has no `pkill`:

```shell
kubectl delete pod leaderworkerset-sample-0
```

Scale-down waits for the HPA stabilization window, five minutes by default,
before it starts.

## Troubleshooting

`kubectl describe hpa <name>` reports why a decision was or was not made.

- **Targets show `<unknown>`**: metrics-server is not running, or a container
  is missing the resource request for the metric being targeted.
- **Load does not trigger scaling**: confirm it is on a leader pod. Worker pod
  usage is not read by the HPA.
- **Replica count oscillates**: raise `behavior.scaleDown.stabilizationWindowSeconds`,
  as the CPU + memory variant does.

## Cleanup

```shell
kubectl delete hpa lws-hpa
kubectl delete leaderworkerset leaderworkerset-sample
```
