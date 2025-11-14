---
title: "SGLang"
linkTitle: "SGLang"
weight: 4
description: >
  An example of using SGLang with LWS
---

## Deploy Distributed Inference Service with SGLang and LWS on GPUs

In this example, we demonstrate how to deploy a distributed inference service using LeaderWorkerSet (LWS) with [SGLang](https://docs.sglang.ai/) on GPU clusters.

SGLang provides native support for distributed tensor-parallel inference and serving, enabling efficient deployment of large language models (LLMs) such as DeepSeek-R1 671B and Llama-3.1-405B across multiple nodes. This example uses the [meta-llama/Meta-Llama-3.1-8B-Instruct](https://huggingface.co/meta-llama/Llama-3.1-8B-Instruct) model to demonstrate multi-node serving capabilities. For implementation details on distributed execution, see the SGLang docs [Run Multi-Node Inference](https://docs.sglang.ai/references/multi_node_deployment/multi_node.html).

Since SGLang employs tensor parallelism for multi-node inference, which requires more frequent communications than pipeline parallelism, ensure high-speed bandwidth between nodes to avoid poor performance.

### Deploy LeaderWorkerSet of SGLang
We use LeaderWorkerSet to deploy 2 SGLang replicas, and each replica has 2 Pods, 1 GPU per Pod. Set the `--tp` to 2 to enable inference across two pods.
The leader pod runs the HTTP server, with a ClusterIP Service exposing the port.

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/sglang/lws.yaml | envsubst | kubectl apply -f -
```

Verify the status of the SGLang pods
```shell
kubectl get pods
```

Should get an output similar to this
```shell
NAME         READY   STATUS    RESTARTS   AGE
sglang-0     1/1     Running   0          3m55s
sglang-0-1   1/1     Running   0          3m55s
sglang-1     1/1     Running   0          3m55s
sglang-1-1   1/1     Running   0          3m55s
```

Verify that the distributed tensor-parallel inference works
```shell
kubectl logs sglang-0 |grep -C 2 -i "Application startup complete"
```
Should get an output similar to this
```text
[2025-02-11 09:06:55] INFO:     Started server process [1]
[2025-02-11 09:06:55] INFO:     Waiting for application startup.
[2025-02-11 09:06:55] INFO:     Application startup complete.
[2025-02-11 09:06:55] INFO:     Uvicorn running on http://0.0.0.0:40000 (Press CTRL+C to quit)
[2025-02-11 09:06:56] INFO:     127.0.0.1:40048 - "GET /get_model_info HTTP/1.1" 200 OK
```

### Access ClusterIP Service

Use `kubectl port-forward` to forward local port 40000 to a pod.
```shell
# Listen on port 40000 locally, forwarding to the targetPort of the service's port 40000 in a pod selected by the service
kubectl port-forward svc/sglang-leader 40000:40000
```

The output should be similar to the following
```shell
Forwarding from 127.0.0.1:40000 -> 40000
```

### Serve the Model

Open another terminal and send a request
```shell
curl http://localhost:40000/v1/completions \
-H "Content-Type: application/json" \
-d '{
    "model": "meta-llama/Meta-Llama-3.1-8B-Instruct",
    "role": "user",
    "prompt": "What is the meaning of life?"
}'
```

The output should be similar to the following
```json
{
  "id": "ae241aa13d2f473ab69a9b0d84eabe8b",
  "object": "text_completion",
  "created": 1739265029,
  "model": "meta-llama/Meta-Llama-3.1-8B-Instruct",
  "choices": [
    {
      "index": 0,
      "text": " A question that has puzzled humanity for thousands of years. Philosophers, scientists,",
      "logprobs": null,
      "finish_reason": "length",
      "matched_stop": null
    }
  ],
  "usage": {
    "prompt_tokens": 8,
    "total_tokens": 24,
    "completion_tokens": 16,
    "prompt_tokens_details": null
  }
}
```
