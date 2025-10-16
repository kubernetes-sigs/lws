---
title: "vLLM"
linkTitle: "vLLM"
weight: 1
description: >
  An example of using vLLM with LWS
---

In this example, we will use LeaderWorkerSet to deploy a distributed inference service with vLLM on GPUs.
[vLLM](https://docs.vllm.ai/en/latest/index.html) supports distributed tensor-parallel inference and serving. Currently, it supports Megatron-LM’s tensor parallel algorithm. It manages the distributed runtime with [Ray](https://docs.ray.io/en/latest/index.html). See the doc [vLLM Distributed Inference and Serving](https://docs.vllm.ai/en/latest/serving/distributed_serving.html) for more details.

## Deploy LeaderWorkerSet of vLLM
We use LeaderWorkerSet to deploy two vLLM model replicas. We have two flavors of the deployment:
- GPU: Each vLLM replica has 2 pods (`pipeline_parallel_size=2`) and 8 GPUs per pod (`tensor_parallel_size=8`).
- TPU: The example assumes that you have a GKE cluster with two TPU v5e-16 slices. You can view how to create a cluster with multiple TPU slices [here](https://cloud.google.com/kubernetes-engine/docs/how-to/tpus). Each TPU slice has 4 hosts, and each host has 4 TPUs. The vLLM server is deployed on the TPU slice with `pipeline_parallel_size=2` and `tensor_parallel_size=16`.

In both examples, Ray uses the leader pod as the head node and the worker pods as the worker nodes. The leader pod runs the vLLM server, with a ClusterIP Service exposing the port.

{{< tabpane >}}
{{< tab header="GPU" lang="shell" >}}
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/vllm/GPU/lws.yaml -s | envsubst | kubectl apply -f -
{{< /tab >}}
{{< tab header="TPU" lang="shell" >}}
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/vllm/TPU/lws.yaml -s | envsubst | kubectl apply -f -
{{< /tab >}}
{{< /tabpane >}}

Verify the status of the vLLM pods
```shell
kubectl get pods
```

Should get an output similar to this
```shell
NAME       READY   STATUS    RESTARTS   AGE
vllm-0     1/1     Running   0          2s
vllm-0-1   1/1     Running   0          2s
vllm-1     1/1     Running   0          2s
vllm-1-1   1/1     Running   0          2s
```

Verify that the distributed tensor-parallel inference works
```shell
kubectl logs vllm-0 |grep -i "Loading model weights took"
```
Should get an output similar to this
```text
INFO 05-08 03:20:24 model_runner.py:173] Loading model weights took 0.1189 GB
(RayWorkerWrapper pid=169, ip=10.20.0.197) INFO 05-08 03:20:28 model_runner.py:173] Loading model weights took 0.1189 GB
```


## Access ClusterIP Service

Use `kubectl port-forward` to forward local port 8080 to a pod.
```shell
# Listen on port 8080 locally, forwarding to the targetPort of the service's port 8080 in a pod selected by the service
kubectl port-forward svc/vllm-leader 8080:8080
```

The output should be similar to the following
```shell
Forwarding from 127.0.0.1:8080 -> 8080
Forwarding from [::1]:8080 -> 8080
```

## Serve the Model

Open another terminal and send a request
```shell
curl http://localhost:8080/v1/completions \
-H "Content-Type: application/json" \
-d '{
    "model": "meta-llama/Meta-Llama-3.1-405B-Instruct",
    "prompt": "San Francisco is a",
    "max_tokens": 7,
    "temperature": 0
}'
```

The output should be similar to the following
```json
{
  "id": "cmpl-1bb34faba88b43f9862cfbfb2200949d",
  "object": "text_completion",
  "created": 1715138766,
  "model": "meta-llama/Meta-Llama-3.1-405B-Instruct",
  "choices": [
    {
      "index": 0,
      "text": " top destination for foodies, with",
      "logprobs": null,
      "finish_reason": "length",
      "stop_reason": null
    }
  ],
  "usage": {
    "prompt_tokens": 5,
    "total_tokens": 12,
    "completion_tokens": 7
  }
}
```
