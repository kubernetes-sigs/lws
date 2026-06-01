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

SCHEDULER_PROVIDER=${SCHEDULER_PROVIDER:-""}
export CWD=$(pwd)
function cleanup {
    if [ $USE_EXISTING_CLUSTER == 'false' ]
    then
        if [ ! -d "$ARTIFACTS" ]; then
            mkdir -p "$ARTIFACTS"
        fi
        $KUBECTL logs -n lws-system deployment/lws-controller-manager > "$ARTIFACTS"/lws-controller-manager.log || true
        $KUBECTL describe pods -n lws-system > "$ARTIFACTS"/lws-system-pods.log || true
        
        if [ "$SCHEDULER_PROVIDER" == "volcano" ]; then
            $KUBECTL logs -n volcano-system deployment/volcano-scheduler > "$ARTIFACTS"/volcano-scheduler.log || true
            $KUBECTL logs -n volcano-system deployment/volcano-controllers > "$ARTIFACTS"/volcano-controller-manager.log || true
            $KUBECTL describe pods -n volcano-system > "$ARTIFACTS"/volcano-system-pods.log || true
        fi
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
function deploy_gang_scheduler() {
    if [ "$SCHEDULER_PROVIDER" == "volcano" ]; then
        echo "Deploying Volcano ${VOLCANO_VERSION}..."
        $KUBECTL apply -f https://raw.githubusercontent.com/volcano-sh/volcano/${VOLCANO_VERSION}/installer/volcano-development.yaml
        $KUBECTL -n volcano-system wait --for condition=ready pod -l app=volcano-scheduler --timeout=5m
        $KUBECTL -n volcano-system wait --for condition=ready pod -l app=volcano-controller --timeout=5m
        $KUBECTL -n volcano-system wait --for condition=ready pod -l app=volcano-admission --timeout=5m
        echo "Volcano deployed successfully"
    fi
}
function kind_load {
    $KIND load docker-image $IMAGE_TAG --name $KIND_CLUSTER_NAME
}
function lws_deploy {
    pushd "$CWD/config/manager"
    $KUSTOMIZE edit set image controller=$IMAGE_TAG
    # Base configuration
    config_content="apiVersion: config.lws.x-k8s.io/v1alpha1
kind: Configuration
leaderElection:
  leaderElect: true"
    # Add cert manager configuration if enabled
    if [ "${USE_CERT_MANAGER:-false}" == "true" ]; then
        config_content="$config_content
internalCertManagement:
  enable: false"
    fi
    # Add gang scheduling configuration if scheduler provider is specified
    if [ -n "$SCHEDULER_PROVIDER" ]; then
        config_content="$config_content
gangSchedulingManagement:
  schedulerProvider: $SCHEDULER_PROVIDER"
    fi
    echo "$config_content" > controller_manager_config.yaml
    popd
    # Add Volcano clusterrole permissions
    if [ "$SCHEDULER_PROVIDER" == "volcano" ]; then
        if ! grep -q "scheduling.volcano.sh" "$CWD/config/rbac/role.yaml"; then
            cat >> "$CWD/config/rbac/role.yaml" << 'EOF'
- apiGroups:
  - scheduling.volcano.sh
  resources:
  - podgroups
  verbs:
  - create
  - get
  - list
  - watch
EOF
        fi
    fi
    if [ "${USE_CERT_MANAGER:-false}" == "true" ]; then
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
      $KUSTOMIZE build $CWD/test/e2e/config | $KUBECTL apply --server-side -f -
    fi
}

function run_tests() {
    if [ -n "$SCHEDULER_PROVIDER" ]; then
        # Run gang scheduling tests
        $GINKGO --junit-report=junit.xml --output-dir=$ARTIFACTS -v --focus="leaderWorkerSet e2e gang scheduling tests" $CWD/test/e2e/...
    else
        # Run normal tests, skip gang scheduling tests
        $GINKGO --junit-report=junit.xml --output-dir=$ARTIFACTS -v --skip="leaderWorkerSet e2e gang scheduling tests" $CWD/test/e2e/...
    fi
}

trap cleanup EXIT
startup
kind_load
deploy_cert_manager
deploy_gang_scheduler
lws_deploy
run_tests
