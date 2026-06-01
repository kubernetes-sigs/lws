# Copyright (c) 2024, NVIDIA CORPORATION. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Adapted from https://github.com/triton-inference-server/tutorials/blob/main/Deployment/Kubernetes/TensorRT-LLM_Multi-Node_Distributed_Models/containers/triton_trt-llm.containerfile

ARG BASE_CONTAINER_IMAGE=nvcr.io/nvidia/tritonserver:24.07-trtllm-python-py3
FROM ${BASE_CONTAINER_IMAGE}


# Stop APT (Debian package manager) from complaining about interactivity.
ENV DEBIAN_FRONTEND=noninteractive
# Set additional environment values that make usage more pleasant.
ENV TERM=xterm-256color

RUN apt update \
 && apt install --yes \
    apt-transport-https \
    ca-certificates \
    curl \
    gnupg \
    cgroup-tools \
 && rm -rf /var/lib/apt/lists/*

# Install kubectl because server.py script depends on it.
# Step 1: acquire the Kubernetes APT GPG key.
RUN curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key \
  | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg \
 && chmod 644 /etc/apt/keyrings/kubernetes-apt-keyring.gpg

# Step 2: Acquire the API sources list for Kubernetes.
RUN echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.30/deb/ /' \
  | tee /etc/apt/sources.list.d/kubernetes.list \
 && chmod 644 /etc/apt/sources.list.d/kubernetes.list

# Step 3: Install kubectl.
RUN apt update \
 && apt install --yes \
    kubectl \
 && apt autoremove --yes \
 && apt purge --yes \
 && rm -rf /var/lib/apt/lists/*


# Set the active working directory.
WORKDIR /workspace

# Copy kubessh script w/ executable permissions for everyone.
# This enables the script to be executed no matter the user the container is run as.
# This works around the issue of the file being non-executable when the container is build on a Windows host.
COPY --chmod=555 kubessh .
COPY server.py .

RUN apt list --installed \
 && pip list --version

COPY install_model.sh .
RUN chmod +x install_model.sh 

ENTRYPOINT [ "/bin/bash" ]
