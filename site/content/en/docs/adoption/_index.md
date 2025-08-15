---
title: "Adopters"
linkTitle: "Adopters"
weight: 30
menu:
  main:
    weight: 30
description: >
  Where LWS is used and integrated in
aliases:
- /adopters
---

# Adopters, Integrations and Presentations

## Adopters

*This is based on public documentations, please open an issue if you would like to be added or removed the list.*

**AWS**:
   * Amazon EKS supports to run *superpod* with LeaderWorkerSet to server large LLMs, see blog [here](https://aws.amazon.com/blogs/hpc/scaling-your-llm-inference-workloads-multi-node-deployment-with-tensorrt-llm-and-triton-on-amazon-eks/).
   * A Terraform based EKS Blueprints pattern can be found [here](https://aws-ia.github.io/terraform-aws-eks-blueprints/patterns/machine-learning/multi-node-vllm/). This pattern demonstrates an Amazon EKS Cluster with an EFA-enabled nodegroup that support multi-node inference using vLLM and LeaderWorkerSet.

**DaoCloud**: LeaderWorkerSet is the default deployment method to run large models crossing multiple nodes on Kubernetes.

**Google Cloud**:
   * GKE leverages LeaderWorkerSet to deploy and serve multi-host gen AI large open models, see blog [here](https://cloud.google.com/blog/products/ai-machine-learning/deploy-and-serve-open-models-over-google-kubernetes-engine?e=48754805).
   * A guide to serve DeepSeek-R1 671B or Llama 3.1 405B on GKE, see guide [here](https://cloud.google.com/kubernetes-engine/docs/tutorials/serve-multihost-gpu)

**Nvidia**: LeaderWorkerSet deployments are the recommended method for deploying Multi-Node models with NIM, see document [here](https://docs.nvidia.com/nim/large-language-models/1.5.0/deploy-helm.html#multi-node-models).

## Integrations

*Feel free to submit a PR if you use LeaderWorkerSet in your project and want to be added here.*

[**Axlearn**](https://github.com/apple/axlearn): Axlearn is a library built on top of JAX and XLA to support the development of large-scale deep learning models. It uses LeaderWorkerSet to deploy multi-host
inference workloads to use during training workflows.

[**llmaz**](https://github.com/InftyAI/llmaz): llmaz, serving as an easy to use and advanced inference platform, uses LeaderWorkerSet as the underlying workload to support both single-host and multi-host inference scenarios.

[**NVIDIA Dynamo**](https://github.com/ai-dynamo/dynamo): NVIDIA Dynamo is a high-throughput low-latency inference framework designed for serving generative AI and reasoning models in multi-node distributed environments especially the disaggregated prefill & decode inference. It uses LeaderWorkerSet to support multi-node deployment on Kubernetes.

[**OME**](https://github.com/sgl-project/ome): OME is a Kubernetes operator for enterprise-grade management and serving of LLMs.
it leverages LWS for multi-node inference, see documentation [here](https://docs.sglang.ai/ome/docs/concepts/inference_service/#multi-node-mode)

[**SGLang**](https://github.com/sgl-project/sglang): SGLang, a fast serving framework for large language models and vision language models. It can be deployed with LWS on Kubernetes for
distributed model serving, see documentation [here](https://docs.sglang.ai/references/deploy_on_k8s.html#deploy-on-kubernetes)

[**vLLM**](https://github.com/vllm-project/vllm): vLLM is a fast and easy-to-use library for LLM inference, it can be deployed with LWS on Kubernetes for distributed model serving, see documentation [here](https://docs.vllm.ai/en/stable/deployment/frameworks/lws.html).


## Talks and Presentations

- KubeCon NA 2024: [Distributed Multi-Node Model Inference Using the LeaderWorkerSet API](https://www.youtube.com/watch?v=Al51wafTrRE) by @ahg-g @liurupeng
- KubeCon EU 2025: [Project Lighting Talk: Sailing Multi-Host Inference with LWS](https://www.youtube.com/watch?v=PJ8qgKEwDyM) by @kerthcet
- KubeCon HK 2025: [More Than Model Sharding: LWS & Distributed Inference](https://www.youtube.com/watch?v=Yzk30z_exIs)(In Chinese) by @panpan0000 @nicole-lihui
- KubeCon HK 2025: [New Pattern for Sailing Multi-host LLM Inference](https://youtu.be/Jou7j-X_VJA) by @kerthcet
- KubeCon JP 2025: [Sailing Multi-host Inference for LLM on Kubernetes](https://youtu.be/PBJk2UqF_-k) by @yankay
