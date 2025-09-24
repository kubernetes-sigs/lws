---
title: "llama.cpp"
linkTitle: "llama.cpp"
weight: 3
description: >
  An example of using llama.cpp with LWS
---


## Deploy Distributed Inference Service with llama.cpp

In this example, we will use LeaderWorkerSet to deploy a distributed
inference service using [llama.cpp](https://github.com/ggerganov/llama.cpp).
llama.cpp began as a project to support CPU-only inference on a single node, but has
since expanded to support accelerators and distributed inference.


### Deploy LeaderWorkerSet of llama.cpp

We use LeaderWorkerSet to deploy a llama.cpp leader and two llama.cpp workers.
The leader pod loads the model and distributes layers to the workers; the workers
perform the majority of the computation.

Because the default configuration runs with CPU inference, this can be run on a kind cluster.

Create a kind cluster:

```shell
kind create cluster
```

Install LeaderWorkerSet; full details are in the [installation guide](../../installation) but running the e2e test suite is an easy way to get started in development:

```shell
USE_EXISTING_CLUSTER=true KIND_CLUSTER_NAME=kind make test-e2e
```

Finally, deploy the llama.cpp example to kind with [this script](https://github.com/kubernetes-sigs/lws/blob/main/docs/examples/llamacpp/dev/tasks/run-in-kind):
```shell
./docs/examples/llamacpp/dev/tasks/run-in-kind
```

After a lot of downloading and building (we build llama.cpp from source, and we download
a model), you should be running our example `clichat` pod that communicates with the llamap.cpp
Service to give a basic LLM chat experience:

```
...
+ kubectl apply --server-side -f k8s/lws.yaml
leaderworkerset.leaderworkerset.x-k8s.io/llamacpp-llama3-8b-instruct-bartowski-q5km serverside-applied
service/llamacpp serverside-applied
+ kubectl run clichat --image=clichat:latest --rm=true -it --image-pull-policy=IfNotPresent --env=LLM_ENDPOINT=http://llamacpp:80
If you don't see a command prompt, try pressing enter.
What is the capital of France?
        The capital of France is Paris.
```

(Feel free to do your own chat messages, not just "What is the capital of France?")

If you want to see the pods:

```shell
> kubectl get pods
NAME                                             READY   STATUS    RESTARTS   AGE
llamacpp-llama3-8b-instruct-bartowski-q5km-0     1/1     Running   0          2m42s
llamacpp-llama3-8b-instruct-bartowski-q5km-0-1   1/1     Running   0          2m42s
llamacpp-llama3-8b-instruct-bartowski-q5km-0-2   1/1     Running   0          2m42s
llamacpp-llama3-8b-instruct-bartowski-q5km-0-3   1/1     Running   0          2m42s
llamacpp-llama3-8b-instruct-bartowski-q5km-0-4   1/1     Running   0          2m42s
```

To see the endpoints for the services:

```
> kubectl get endpoints
NAME                                         ENDPOINTS                                         AGE
kubernetes                                   192.168.8.3:6443                                  16m
llamacpp                                     10.244.0.90:8080                                  4m
llamacpp-llama3-8b-instruct-bartowski-q5km   10.244.0.90,10.244.0.91,10.244.0.92 + 2 more...   3m59s
```

The service `llamacpp-llama3-8b-instruct-bartowski-q5km` is the service that backs the StatefulSet.

The service `llamacpp` is the real service, and targets the Leader pod only.  The Leader then distributes work to the Workers.
