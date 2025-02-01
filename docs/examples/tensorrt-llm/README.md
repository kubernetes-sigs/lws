# Deploy Distributed Inference Service with Triton TensorRT-LLM and LWS on GPUs

In this example, we will use LeaderWorkerSet to deploy a distributed inference service with Triton TensorRT-LLM on GPUs.
[TensorRT-LLM](https://nvidia.github.io/TensorRT-LLM/) supports multinode serving using tensor and pipeline parallelism. It manages the distributed runtime with [MPI](https://www.open-mpi.org/)

## Install LeaderWorkerSet

Follow the step-by-step guide on how to install LWS. [View installation guide](https://github.com/kubernetes-sigs/lws/blob/main/docs/setup/install.md)

## Build the Triton TensorRT-LLM image

We provide a dockerfile to build the image. The dockerfile contains an installation script to download any Llama model from hugging face and prepare it to be used by TensorRT-LLM. It also has a python script to initialize MPI and start the server.

## Create service account

The script requires access to kubectl to determine when the workers are in a ready state, so a service account with access to it is needed to run the server.

```shell
kubectl apply -f rbac.yaml
```

## Deploy LeaderWorkerSet of TensorRT-LLM

We use LeaderWorkerSet to deploy the TensorRT-LLM server, each replica has 2 pods (pipeline_parallel_size=2) and 8 GPUs (tensor_parallel_size=8) per pod. The leader pod runs the http server. 

```shell
kubectl apply -f lws.yaml
```

Verify the status of the pods:

```shell
kubectl get pods
```

Should get an output similar to this:

```shell
NAME                                       READY   STATUS    RESTARTS   AGE
tensorrt-0                                 1/1     Running   0          31m
tensorrt-0-1                               1/1     Running   0          31m
```

## Deploy cluster IP service

Apply the `service.yaml` manifest:

```shell
kubectl apply -f service.yaml
```

Use `kubectl port-forward` to forward local port 8000 to a pod.

```shell
kubectl port-forward svc/vllm-leader 8000:8000
```

The output should be similar to the following
```shell
Forwarding from 127.0.0.1:8000 -> 8000
Forwarding from [::1]:8000 -> 8000
```

## Serve the model

Open another terminal and serve the request

```shell
$ USER_PROMPT="I'm new to coding. If you could only recommend one programming language to start with, what would it be and why?"
$ curl -X POST localhost:8000/v2/models/ensemble/generate   -H "Content-Type: application/json"   -d @- <<EOF
{
    "text_input": "<start_of_turn>user\n${USER_PROMPT}<end_of_turn>\n",
    "temperature": 0.9,
    "max_tokens": 128
}
EOF
```

The output should be similar to the following: 

```shell

```
