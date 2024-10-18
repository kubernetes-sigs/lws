# Deploy Distributed Inference Service with vLLM and LWS on TPUs

In this example, we will use LeaderWorkerSet to deploy a distributed inference service with vLLM on TPUs. It manages the distributed runtime with [Ray](https://docs.ray.io/en/latest/index.html).

## Install LeaderWorkerSet

Follow the step-by-step guide on how to install LWS. [View installation guide](https://github.com/kubernetes-sigs/lws/blob/main/docs/setup/install.md)


## Deploy LeaderWorkerSet of vLLM
We use LeaderWorkerSet to deploy two vLLM model replicas, and each vLLM replica has 4 pods, and 4 TPUs per pod (tensor_parallel_size=16). 
The leader pod runs the Ray head and the http server, while the workers run the Ray workers.

```shell
kubectl apply -f lws.yaml
```

Verify the status of the vLLM pods
```shell
kubectl get pods
```

Should get an output similar to this
```shell
NAME       READY   STATUS    RESTARTS   AGE
vllm-0     1/1     Running   0          2s
vllm-0-1   1/1     Running   0          2s
vllm-0-2   1/1     Running   0          2s
vllm-0-3   1/1     Running   0          2s
vllm-1     1/1     Running   0          2s
vllm-1-1   1/1     Running   0          2s
vllm-1-2   1/1     Running   0          2s
vllm-1-3   1/1     Running   0          2s
```

Verify that it works by viewing the leader logs
```shell
kubectl logs vllm-0 -c vllm-leader
```

The output should be similar to the following 
```
INFO 10-02 00:21:40 launcher.py:20] Available routes are:
INFO 10-02 00:21:40 launcher.py:28] Route: /openapi.json, Methods: GET, HEAD
INFO 10-02 00:21:40 launcher.py:28] Route: /docs, Methods: GET, HEAD
INFO 10-02 00:21:40 launcher.py:28] Route: /docs/oauth2-redirect, Methods: GET, HEAD
INFO 10-02 00:21:40 launcher.py:28] Route: /redoc, Methods: GET, HEAD
INFO 10-02 00:21:40 launcher.py:28] Route: /health, Methods: GET
INFO 10-02 00:21:40 launcher.py:28] Route: /tokenize, Methods: POST
INFO 10-02 00:21:40 launcher.py:28] Route: /detokenize, Methods: POST
INFO 10-02 00:21:40 launcher.py:28] Route: /v1/models, Methods: GET
INFO 10-02 00:21:40 launcher.py:28] Route: /version, Methods: GET
INFO 10-02 00:21:40 launcher.py:28] Route: /v1/chat/completions, Methods: POST
INFO 10-02 00:21:40 launcher.py:28] Route: /v1/completions, Methods: POST
INFO 10-02 00:21:40 launcher.py:28] Route: /v1/embeddings, Methods: POST
INFO 10-02 00:21:40 launcher.py:33] Launching Uvicorn with --limit_concurrency 32765. To avoid this limit at the expense of performance run with --disable-frontend-multiprocessing
INFO:     Started server process [6803]
INFO:     Waiting for application startup.
INFO:     Application startup complete.
INFO:     Uvicorn running on http://0.0.0.0:8000 (Press CTRL+C to quit)
```

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

## Serve the model

Open another terminal and send a request
```shell
curl http://localhost:8080/v1/completions -H "Content-Type: application/json" -d '{
    "model": "meta-llama/Meta-Llama-3-70B",
    "prompt": "San Francisco is a",
    "max_tokens": 7,
    "temperature": 0
}'
```

The output should be similar to the following
```json
{
    "id":"cmpl-7e795f36f17545eabd451a6dd8f70ce2",
    "object":"text_completion",
    "created":1727733988,
    "model":"meta-llama/Meta-Llama-3-70B",
    "choices":[
        {
            "index":0,
            "text":" top holiday destination featuring scenic beauty and",
            "logprobs":null,
            "finish_reason":"length",
            "stop_reason":null,
            "prompt_logprobs":null
        }
    ],
    "usage":{
        "prompt_tokens":5,
        "total_tokens":12,
      "completion_tokens":7
    }
}
```