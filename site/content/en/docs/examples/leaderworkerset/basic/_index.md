---
title: "Basic"
linkTitle: "Basic"
weight: 1
description: >
  Minimal multi-node inference on LeaderWorkerSet with vLLM and SGLang.
aliases:
- /docs/examples/vllm/
- /docs/examples/sglang/
---

The basic guide deploys a distributed, multi-node inference service with
LeaderWorkerSet: 2 replica groups of 2 pods each. Tensor and pipeline
parallelism spread across the leader and worker pods. Neither runtime uses Ray.

- **vLLM** runs multi-node with its native multiprocessing backend. The leader
  runs `vllm serve` and the OpenAI server; workers run `vllm serve --headless`.
  Nodes coordinate through `--nnodes`, `--node-rank`, and `--master-addr`, wired
  to the LWS environment variables.
- **SGLang** has native distributed support. Nodes coordinate through
  `--dist-init-addr`, `--nnodes`, and `--node-rank`.

The other guides build on this one: [autoscaling](../autoscaling/) adds an HPA
and [topology-aware scheduling](../topology-aware-scheduling/) pins each group
to one topology domain.

## Deploy

Pick a runtime. Both read a Hugging Face token from `HF_TOKEN`.

### vLLM

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/basic/vllm.yaml -s | envsubst | kubectl apply -f -
```

### SGLang

```shell
export HF_TOKEN=<your-hf-token>
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/leaderworkerset/basic/sglang.yaml -s | envsubst | kubectl apply -f -
```

Verify the pods (each replica group has one leader and one worker):

```shell
kubectl get pods
```

## Access the service

### vLLM

```shell
kubectl port-forward svc/vllm-leader 8080:8080
curl http://localhost:8080/v1/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "meta-llama/Llama-3.1-405B-Instruct", "prompt": "What is the meaning of life?", "max_tokens": 16}'
```

### SGLang

```shell
kubectl port-forward svc/sglang-leader 40000:40000
curl http://localhost:40000/v1/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "meta-llama/Meta-Llama-3.1-8B-Instruct", "prompt": "What is the meaning of life?", "max_tokens": 16}'
```

## TPU (vLLM)

To run vLLM on TPUs, swap the image for `vllm/vllm-tpu`, add the TPU
`nodeSelector` (e.g. `cloud.google.com/gke-tpu-accelerator` and
`cloud.google.com/gke-tpu-topology`), and change the GPU resource limit to
`google.com/tpu`. The LeaderWorkerSet structure is otherwise identical.
