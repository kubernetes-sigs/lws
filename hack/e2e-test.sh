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
function cleanup {
    if [ $USE_EXISTING_CLUSTER == 'false' ]
    then
        if [ ! -d "$ARTIFACTS" ]; then
            mkdir -p "$ARTIFACTS"
        fi
        $KUBECTL logs -n lws-system deployment/lws-controller-manager > "$ARTIFACTS"/lws-controller-manager.log || true
        $KUBECTL describe pods -n lws-system > "$ARTIFACTS"/lws-system-pods.log || true
        $KIND export logs "$ARTIFACTS" || true
        $KIND delete cluster --name $KIND_CLUSTER_NAME
    fi
    (cd $CWD/config/manager && $KUSTOMIZE edit set image controller=us-central1-docker.pkg.dev/k8s-staging-images/lws:main)
}
function startup {
    if [ $USE_EXISTING_CLUSTER == 'false' ]
    then
        if [ ! -d "$ARTIFACTS" ]; then
            mkdir -p "$ARTIFACTS"
        fi
        $KIND create cluster --name $KIND_CLUSTER_NAME --image $E2E_KIND_VERSION --wait 1m
        $KUBECTL get nodes > $ARTIFACTS/kind-nodes.log || true
        $KUBECTL describe pods -n kube-system > $ARTIFACTS/kube-system-pods.log || true
    fi
}
function deploy_cert_manager() {
    if [ "${USE_CERT_MANAGER:-false}" == "true" ]; then
      $KUBECTL apply -f https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml
      $KUBECTL -n cert-manager wait --for condition=ready pod -l app.kubernetes.io/instance=cert-manager --timeout=5m
    fi
}
function kind_load {
    $KIND load docker-image $IMAGE_TAG --name $KIND_CLUSTER_NAME
}
function lws_deploy {
    if [ "${USE_CERT_MANAGER:-false}" == "true" ]; then
      pushd "$CWD/config/manager"
        $KUSTOMIZE edit set image controller=$IMAGE_TAG
        echo "apiVersion: config.lws.x-k8s.io/v1alpha1
kind: Configuration
internalCertManagement:
  enable: false
leaderElection:
  leaderElect: true" > controller_manager_config.yaml
      popd
      pushd "$CWD/config/crd"
        $KUSTOMIZE edit add patch --path "patches/cainjection_in_leaderworkersets.yaml"
      popd
      pushd "$CWD/config/default"
        $KUSTOMIZE edit add patch --path "webhookcainjection_patch.yaml"
        $KUSTOMIZE edit add patch --path "cert_metrics_manager_patch.yaml" --kind Deployment
        $KUSTOMIZE edit add resource "../certmanager"
        $KUSTOMIZE edit remove resource "../internalcert"
        $KUSTOMIZE build $CWD/test/e2e/config/certmanager | $KUBECTL apply --server-side -f -
      popd
    else
      cd $CWD/config/manager && $KUSTOMIZE edit set image controller=$IMAGE_TAG
      $KUSTOMIZE build $CWD/test/e2e/config | $KUBECTL apply --server-side -f -
    fi
}
trap cleanup EXIT
startup
kind_load
deploy_cert_manager
lws_deploy
$GINKGO --junit-report=junit.xml --output-dir=$ARTIFACTS -v $CWD/test/e2e/...
