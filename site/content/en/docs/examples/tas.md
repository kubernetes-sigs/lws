---
title: "Topology Aware Scheduling with Kueue"
weight: 5
description: >
  An example on using topology aware scheduling with LWS and Kueue, using vLLM
---

AI Inference workloads require constant Pod-to-Pod communication. This makes the network bandwidth an important requirement of 
running workloads efficiently. The bandwidth between the Pods depends on the placement of the Nodes in the data center. Topology Aware Scheduling (TAS), looks to place the pods as closely as possible to maximize the network bandwidth. To learn more about TAS, visit the page in the [Kueue website](https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/).

This example will cover how to deploy a vLLM multi-host workload using TAS.

## Install the Kueue Controller

```
kubectl apply --server-side -f https://github.com/kubernetes-sigs/kueue/releases/download/v0.14.1/manifests.yaml
```

## Apply the TAS configuration 

```
apiVersion: kueue.x-k8s.io/v1beta1
kind: Topology
metadata:
  name: "default"
spec:
  levels:
  - nodeLabel: "cloud.provider.com/topology-block"
  - nodeLabel: "cloud.provider.com/topology-rack"
  - nodeLabel: "kubernetes.io/hostname"
---
kind: ResourceFlavor
apiVersion: kueue.x-k8s.io/v1beta1
metadata:
  name: "tas-flavor"
spec:
  nodeLabels:
    cloud.provider.com/node-group: "tas-group"
  topologyName: "default"
  tolerations:
  - key: "nvidia.com/gpu"
    operator: "Exists"
    effect: "NoSchedule"
---
apiVersion: kueue.x-k8s.io/v1beta1
kind: ClusterQueue
metadata:
  name: "tas-cluster-queue"
spec:
  namespaceSelector: {} # match all.
  resourceGroups:
  - coveredResources: ["nvidia.com/gpu"]
    flavors:
    - name: "tas-flavor"
      resources:
      - name: "nvidia.com/gpu"
        nominalQuota: 16
---
apiVersion: kueue.x-k8s.io/v1beta1
kind: LocalQueue
metadata:
  namespace: "default"
  name: "tas-user-queue"
spec:
  clusterQueue: "tas-cluster-queue"
```

## Apply the following LWS yaml

```
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: vllm
  labels:
    kueue.x-k8s.io/queue-name: tas-user-queue
spec:
  replicas: 1
  leaderWorkerTemplate:
    size: 2
    restartPolicy: RecreateGroupOnPodRestart
    leaderTemplate:
      metadata:
        labels:
          role: leader
        annotations: 
          kueue.x-k8s.io/podset-required-topology: "cloud.provider.com/topology-block"
          kueue.x-k8s.io/podset-group-name: "vllm-multi-host"
      spec:
        containers:
          - name: vllm-leader
            image: vllm/vllm-openai:v0.8.5
            env:
              - name: HUGGING_FACE_HUB_TOKEN
                valueFrom:
                  secretKeyRef:
                    name: hf-secret
                    key: hf_api_token
            command:
              - sh
              - -c
              - "bash /vllm-workspace/examples/online_serving/multi-node-serving.sh leader --ray_cluster_size=$(LWS_GROUP_SIZE);
                python3 -m vllm.entrypoints.openai.api_server --port 8080 --model deepseek-ai/DeepSeek-R1 --tensor-parallel-size 8 --pipeline-parallel-size 2 --trust-remote-code --max-model-len 4096"
            resources:
              limits:
                nvidia.com/gpu: "8"
            ports:
              - containerPort: 8080
            readinessProbe:
              tcpSocket:
                port: 8080
              initialDelaySeconds: 15
              periodSeconds: 10
            volumeMounts:
              - mountPath: /dev/shm
                name: dshm
        volumes:
        - name: dshm
          emptyDir:
            medium: Memory
            sizeLimit: 15Gi
    workerTemplate:
      metadata:
        annotations: 
          kueue.x-k8s.io/podset-required-topology: "cloud.provider.com/topology-block"
          kueue.x-k8s.io/podset-group-name: "vllm-multi-host"
      spec:
        containers:
          - name: vllm-worker
            image: vllm/vllm-openai:v0.8.5
            command:
              - sh
              - -c
              - "bash /vllm-workspace/examples/online_serving/multi-node-serving.sh worker --ray_address=$(LWS_LEADER_ADDRESS)"
            resources:
              limits:
                nvidia.com/gpu: "8"
            env:
              - name: HUGGING_FACE_HUB_TOKEN
                valueFrom:
                  secretKeyRef:
                    name: hf-secret
                    key: hf_api_token
            volumeMounts:
              - mountPath: /dev/shm
                name: dshm   
        volumes:
        - name: dshm
          emptyDir:
            medium: Memory
            sizeLimit: 15Gi
```