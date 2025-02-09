# Adoption, Integration, and Presentation

## Adopters

*This is based on public documentations, please open an issue if you would like to be added or removed the list.*

**AWS**: Amazon EKS supports to run *superpod* with LeaderWorkerSet (LWS) to server large LLMs, see blog [here](https://aws.amazon.com/blogs/hpc/scaling-your-llm-inference-workloads-multi-node-deployment-with-tensorrt-llm-and-triton-on-amazon-eks/).

**DaoCloud**: LeaderWorkerSet (LWS) is the default deployment method to run large models crossing multiple nodes on Kubernetes.

**Google Cloud**: GKE leverages LeaderWorkerSet (LWS) to deploy and serve multi-host gen AI large open models, see blog [here](https://cloud.google.com/blog/products/ai-machine-learning/deploy-and-serve-open-models-over-google-kubernetes-engine?e=48754805).

**Nvidia**: LeaderWorkerSet (LWS) deployments are the recommended method for deploying Multi-Node models with NIM, see document [here](https://docs.nvidia.com/nim/large-language-models/1.5.0/deploy-helm.html#multi-node-models).

## Integrations

*Feel free to submit a PR if you use LeaderWorkerSet (LWS) in your project and want to be added here.*

[**llmaz**](https://github.com/InftyAI/llmaz): llmaz, serving as an easy to use and advanced inference platform, uses LeaderWorkerSet (LWS) as the underlying workload to support both single-host and multi-host inference scenarios.

[**vLLM**](https://github.com/vllm-project/vllm): vLLM is a fast and easy-to-use library for LLM inference, it can be deployed with LWS on Kubernetes for distributed model serving, see documentation [here](https://docs.vllm.ai/en/stable/deployment/frameworks/lws.html).

## Talks and Presentations

- KubeCon NA 2024: [Distributed Multi-Node Model Inference Using the LeaderWorkerSet API](https://www.youtube.com/watch?v=Al51wafTrRE) by @ahg-g @liurupeng
