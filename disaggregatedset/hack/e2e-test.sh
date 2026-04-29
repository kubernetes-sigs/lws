#!/usr/bin/env bash

# Copyright 2024 The Kubernetes Authors.
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

set -o errexit
set -o nounset
set -o pipefail

export CWD=$(pwd)

# LWS version to install
LWS_VERSION=${LWS_VERSION:-"v0.8.0"}

function cleanup {
    if [ "$USE_EXISTING_CLUSTER" == 'false' ]
    then
        if [ ! -d "$ARTIFACTS" ]; then
            mkdir -p "$ARTIFACTS"
        fi
        # Export disaggregatedset controller logs
        $KUBECTL logs -n disaggregatedset-system deployment/disaggregatedset-controller-manager > "$ARTIFACTS"/disaggregatedset-controller-manager.log || true
        $KUBECTL describe pods -n disaggregatedset-system > "$ARTIFACTS"/disaggregatedset-system-pods.log || true
        # Export LWS controller logs
        $KUBECTL logs -n lws-system deployment/lws-controller-manager > "$ARTIFACTS"/lws-controller-manager.log || true
        $KUBECTL describe pods -n lws-system > "$ARTIFACTS"/lws-system-pods.log || true
        # Export kind logs and delete cluster
        $KIND export logs "$ARTIFACTS" || true
        $KIND delete cluster --name $KIND_CLUSTER_NAME
    fi
    (cd $CWD/config/manager && $KUSTOMIZE edit set image controller=controller:latest)
}

function startup {
    if [ "$USE_EXISTING_CLUSTER" == 'false' ]
    then
        if [ ! -d "$ARTIFACTS" ]; then
            mkdir -p "$ARTIFACTS"
        fi
        $KIND create cluster --name $KIND_CLUSTER_NAME --image $E2E_KIND_VERSION --wait 1m
        $KUBECTL get nodes > "$ARTIFACTS"/kind-nodes.log || true
        $KUBECTL describe pods -n kube-system > "$ARTIFACTS"/kube-system-pods.log || true
    fi
}

function docker_build {
    echo "Building disaggregatedset operator image..."
    docker build -t "$IMAGE_TAG" .
}

function kind_load {
    echo "Loading operator image to Kind..."
    $KIND load docker-image "$IMAGE_TAG" --name "$KIND_CLUSTER_NAME"
}

function install_lws {
    echo "Installing LWS ${LWS_VERSION}..."
    $KUBECTL apply --server-side -f "https://github.com/kubernetes-sigs/lws/releases/download/${LWS_VERSION}/manifests.yaml"
    echo "Waiting for LWS controller to be ready..."
    $KUBECTL -n lws-system wait --for=condition=available --timeout=5m deployment/lws-controller-manager
}

function deploy_operator {
    echo "Deploying disaggregatedset operator..."
    pushd "$CWD/config/manager"
    $KUSTOMIZE edit set image controller="$IMAGE_TAG"
    popd
    $KUSTOMIZE build "$CWD/config/default" | $KUBECTL apply --server-side -f -
    echo "Waiting for disaggregatedset operator to be ready..."
    $KUBECTL -n disaggregatedset-system wait --for=condition=available --timeout=2m deployment/disaggregatedset-controller-manager
}

function run_tests {
    echo "Running e2e tests..."
    # Set LWS_INSTALL_SKIP since we already installed LWS
    # Set KIND_CLUSTER for the test code (it expects KIND_CLUSTER, not KIND_CLUSTER_NAME)
    # Use --tags=e2e to include files with //go:build e2e constraint
    LWS_INSTALL_SKIP=true KIND_CLUSTER="$KIND_CLUSTER_NAME" $GINKGO --tags=e2e --junit-report=junit.xml --output-dir="$ARTIFACTS" -v ${GINKGO_FOCUS:+--focus="$GINKGO_FOCUS"} "$CWD/test/e2e/..."
}

trap cleanup EXIT
startup
docker_build
kind_load
install_lws
deploy_operator
run_tests
