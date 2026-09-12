---
title: "Topology-aware scheduling"
linkTitle: "Topology-aware scheduling"
weight: 3
description: >
  Pin each replica group to a single topology domain, with native placement or Kueue.
aliases:
- /docs/examples/tas/
---

AI inference workloads need constant pod-to-pod communication, so network
bandwidth matters. The bandwidth between pods depends on how close their nodes
sit in the data center. Topology-aware scheduling keeps each replica group
within one topology domain to maximize it, which raises bandwidth for tensor and
pipeline parallelism.

There are two ways to do this. Pick based on how your cluster admits workloads:

- **Native placement policies** — LWS's built-in
  `leaderworkerset.sigs.k8s.io/exclusive-topology` annotation. No extra
  components. Use this when LWS schedules directly against the cluster.
- **Kueue** — hand admission and placement to
  [Kueue topology-aware scheduling (TAS)](https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/).
  Use this when Kueue already manages quota and admission for your cluster, so
  topology placement stays consistent with the rest of your gang scheduling.

## Option 1: Native placement policies

This guide is the [basic](../basic/) deployment plus topology-aware placement.
The `leaderworkerset.sigs.k8s.io/exclusive-topology` annotation keeps each
replica group within one topology domain and excludes other groups from it.

Set the annotation value to your cluster's topology key (the example uses
`cloud.google.com/gke-nodepool`). Nodes must be labeled with that key.

{{% tabpane text=true %}}
{{% tab header="vLLM" %}}
```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/topology-aware-scheduling/vllm.yaml -s | envsubst | kubectl apply -f -
```
{{% /tab %}}
{{% tab header="SGLang" %}}
```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/topology-aware-scheduling/sglang.yaml -s | envsubst | kubectl apply -f -
```
{{% /tab %}}
{{% /tabpane %}}

## Option 2: Kueue topology-aware scheduling

Use this when Kueue already manages quota and admission for your cluster.

[Kueue topology-aware scheduling (TAS)](https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/)
places the group through `kueue.x-k8s.io/podset-required-topology`. Unlike the
native `exclusive-topology` feature, this is **all-or-nothing**: Kueue only
admits the group if it fits entirely within one topology domain, and holds it
otherwise. Native placement instead schedules the group without that admission
gate. Prefer Kueue TAS when you want quota-gated, all-or-nothing placement;
prefer native placement when LWS schedules directly against the cluster.

### Define topology levels

In a yaml file, define the levels of your topology and the resource type you
schedule on.

```yaml
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

### Install the Kueue controller

```shell
helm install kueue oci://registry.k8s.io/kueue/charts/kueue --version=0.16.1 \
  --create-namespace --namespace=kueue-system
```

Now install kueue-populator, passing the topology definition:

```shell
helm install kueue-populator oci://registry.k8s.io/kueue/charts/kueue-populator \
  --version=0.16.1 --namespace=kueue-system --create-namespace --wait \
  -f <topology-yaml-file>
```

### Deploy

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/topology-aware-scheduling/vllm-kueue.yaml -s | envsubst | kubectl apply -f -
```

See [basic](../basic/) for how to reach the service once pods are running.
