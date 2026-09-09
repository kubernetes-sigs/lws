---
title: "Topology-aware scheduling"
linkTitle: "Topology-aware scheduling"
weight: 4
description: >
  Co-locate a slice's roles in one topology domain, with native placement or Kueue.
---

Disaggregated prefill and decode roles exchange the KV cache, so bandwidth
between them depends on how close their nodes sit in the data center.
Topology-aware scheduling co-locates each slice's roles in one topology domain
to raise that bandwidth.

There are two ways to do this. Pick based on how your cluster admits workloads:

- **Native placement policies** — `spec.placementPolicy` built into
  DisaggregatedSet. No extra components. Use this when LWS schedules directly
  against the cluster.
- **Kueue** — hand admission and placement to
  [Kueue topology-aware scheduling (TAS)](https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/).
  Use this when Kueue already manages quota and admission for your cluster.

## Option 1: Native placement policies

This guide is the [basic](../basic/) deployment plus topology-aware placement.
`spec.placementPolicy` with `type: ExclusiveSlice` co-locates a slice's roles in
one topology domain and spreads slices across domains.

Set `placementPolicy.topology` to your cluster's topology key (the example uses
`cloud.google.com/gke-nodepool`). Nodes must be labeled with that key.

{{% tabpane text=true %}}
{{% tab header="vLLM" %}}
```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/topology-aware-scheduling/vllm.yaml -s | envsubst | kubectl apply -f -
```
{{% /tab %}}
{{% tab header="SGLang" %}}
```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/topology-aware-scheduling/sglang.yaml -s | envsubst | kubectl apply -f -
```
{{% /tab %}}
{{% /tabpane %}}

See the [placement policy concepts](../../../concepts/disaggregatedset/placement-policy/)
for the full set of placement types.

## Option 2: Kueue topology-aware scheduling

Kueue admits the DisaggregatedSet and asks the scheduler to pack each role
(prefill and decode) into one topology domain. Use this when Kueue already
manages quota and admission for your cluster.

Unlike the native `placementPolicy` feature, `kueue.x-k8s.io/podset-required-topology`
is **all-or-nothing**: Kueue only admits a role if it fits entirely within one
topology domain, and holds it otherwise. Prefer Kueue TAS when you want
quota-gated, all-or-nothing placement; prefer native placement when LWS
schedules directly against the cluster.

### Install the Kueue controller

```shell
helm install kueue oci://registry.k8s.io/kueue/charts/kueue --version=0.16.1 \
  --create-namespace --namespace=kueue-system
```

### Define topology levels

Give kueue-populator the levels of your data center topology and the node label
that marks schedulable capacity. It creates the `Topology`, `ResourceFlavor`,
`ClusterQueue`, and a `default` `LocalQueue`. The example uses GKE labels; set
them to your cluster's.

```yaml
# topology.yaml
kueuePopulator:
  config:
    topology:
      levels:
        - nodeLabel: "cloud.google.com/gce-topology-block"
        - nodeLabel: "cloud.google.com/gce-topology-subblock"
        - nodeLabel: "cloud.google.com/gce-topology-host"
        - nodeLabel: "kubernetes.io/hostname"
    resourceFlavor:
      nodeLabels:
        cloud.google.com/gke-gpu: "true"
```

```shell
helm install kueue-populator oci://registry.k8s.io/kueue/charts/kueue-populator \
  --version=0.16.1 --namespace=kueue-system --create-namespace --wait \
  -f topology.yaml
```

### Deploy

The DisaggregatedSet carries `kueue.x-k8s.io/queue-name: default` so Kueue admits
it, and `kueue.x-k8s.io/podset-required-topology` on each role's worker template
to require that role inside one block. Set that annotation to the topology level
you want.

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/disaggregatedset/topology-aware-scheduling/vllm-kueue.yaml -s | envsubst | kubectl apply -f -
```
