---
title: "Examples"
linkTitle: "Examples"
weight: 6
description: >
  This section contains examples of using LWS with or without specific inference runtime.
---

Below are some examples to use different features of LeaderWorkerSet.

## A Minimal Sample

Deploy a LeaderWorkerSet with 3 groups and 4 workers per group. You can find an example [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/sample/lws.yaml).

## Multi-template for Pods

LWS support using different templates for leader and worker pods. You can find the example [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/sample/lws-multi-template.yaml), leader pod's spec is specified in leaderTemplate, and worker pods' spec is specified in workerTemplate.

## Restart Policy

You could specify the RestartPolicy to define the failure handling schematics for the pod group.
By default, only failed pods will be automatically restarted. When the RestartPolicy is set to `RecreateGroupOnRestart`, it will recreate
the pod group on container/pod restarts. All the worker pods will be recreated after the new leader pod is started.
You can find an example [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/sample/lws-restart-policy.yaml).

## Horizontal Pod AutoScaler (HPA)

LWS supports the scale subresource for [HPA](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/) to manage workload autoscaling. An example HPA yaml for LWS can be found [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/sample/horizontal-pod-autoscaler.yaml).

## Exclusive Placement

LeaderWorkerSet supports exclusive placement through pod affinity/anti-affinity where pods in the same group will be scheduled on the same accelerator island (such as a TPU slice or a GPU clique), but on different nodes. This ensures 1:1 LWS replica to accelerator island placement.
This feature can be enabled by adding the exclusive topology annotation **leaderworkerset.sigs.k8s.io/exclusive-topology:** as shown [here](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/sample/lws-exclusive-placement.yaml).
