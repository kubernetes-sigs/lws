# Deploy distributed vLLM with LWS on GPUs

In this example, we will use LeaderWorkerSet to deploy a distributed inference instance with vLLM on GPUs.    
[vLLM](https://docs.vllm.ai/en/latest/index.html) supports distributed tensor-parallel inference and serving. Currently, it supports Megatron-LM’s tensor parallel algorithm. It manages the distributed runtime with [Ray](https://docs.ray.io/en/latest/index.html). See the doc [vLLM Distributed Inference and Serving](https://docs.vllm.ai/en/latest/serving/distributed_serving.html) for more details.

## Install LeaderWorkerSet

Follow the step-by-step guide on how to install LWS. [View installation guide](https://github.com/kubernetes-sigs/lws/blob/main/docs/setup/install.md)

## Deploy LeaderWorkerSet Deployment
We use LeaderWorkerSet to deploy two vLLM model replicas, and each vLLM replicas has two pods (tp=2). 
The leader pod runs the Ray head and the http server, while the workers run the Ray workers.

```shell
kubectl apply -f lws.yaml
```

Verify the status of the vLLM Deployment
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
The total weights of the facebook/opt-125m model is about 0.25GB, and each pod loads 0.1189GB.

# Use vLLM

## Deploy ClusterIP Service

Apply the `service.yaml` manifest

```shell
kubectl apply -f service.yaml
```

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
    "model": "facebook/opt-125m",
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
  "model": "facebook/opt-125m",
  "choices": [
    {
      "index": 0,
      "text": " great place to live.  I",
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