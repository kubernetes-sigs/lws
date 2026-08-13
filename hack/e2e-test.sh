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
LWS_UPGRADE_FROM_VERSION=${LWS_UPGRADE_FROM_VERSION:-""}
LWS_NAMESPACE=${LWS_NAMESPACE:-"lws-system"}
export CWD=$(pwd)

KUBECONFIG_PATH=""
OLD_MANIFEST=""

if [ -n "$LWS_UPGRADE_FROM_VERSION" ]; then
    OLD_CONTROLLER_IMAGE="registry.k8s.io/lws/lws:${LWS_UPGRADE_FROM_VERSION}"
    OLD_MANIFEST_URL="https://github.com/kubernetes-sigs/lws/releases/download/${LWS_UPGRADE_FROM_VERSION}/manifests.yaml"
    PAUSE_IMAGE="registry.k8s.io/pause:3.10"
    TEST_NAMESPACE="lws-upgrade-test"
    SNAPSHOT_PATH="${ARTIFACTS}/upgrade-snapshot.json"
fi

function save_controller_logs {
    local output="$1"
    $KUBECTL logs -n "$LWS_NAMESPACE" deployment/lws-controller-manager \
        --all-pods=true --prefix=true > "$output" 2>&1 || true
}

function cleanup {
    if [ $USE_EXISTING_CLUSTER == 'false' ]
    then
        if [ ! -d "$ARTIFACTS" ]; then
            mkdir -p "$ARTIFACTS"
        fi
        $KUBECTL logs -n "$LWS_NAMESPACE" deployment/lws-controller-manager > "$ARTIFACTS"/lws-controller-manager.log || true
        $KUBECTL describe pods -n "$LWS_NAMESPACE" > "$ARTIFACTS"/"${LWS_NAMESPACE}"-pods.log || true
        
        if [ "$SCHEDULER_PROVIDER" == "volcano" ]; then
            $KUBECTL logs -n volcano-system deployment/volcano-scheduler > "$ARTIFACTS"/volcano-scheduler.log || true
            $KUBECTL logs -n volcano-system deployment/volcano-controllers > "$ARTIFACTS"/volcano-controller-manager.log || true
            $KUBECTL describe pods -n volcano-system > "$ARTIFACTS"/volcano-system-pods.log || true
        fi

        if [ -n "$LWS_UPGRADE_FROM_VERSION" ]; then
            $KUBECTL get events -A --sort-by=.lastTimestamp > "$ARTIFACTS"/events.log 2>&1 || true
            $KUBECTL get leaderworkersets,disaggregatedsets,pods,statefulsets,services \
                -n "$TEST_NAMESPACE" -o yaml > "$ARTIFACTS"/upgrade-workloads.yaml 2>&1 || true
        fi

        $KIND export logs "$ARTIFACTS" --name "$KIND_CLUSTER_NAME" || true
        $KIND delete cluster --name $KIND_CLUSTER_NAME
    fi

    if [ -n "$KUBECONFIG_PATH" ]; then
        rm -f "$KUBECONFIG_PATH"
    fi
    if [ -n "$OLD_MANIFEST" ]; then
        rm -f "$OLD_MANIFEST"
    fi
    if [ -z "$LWS_UPGRADE_FROM_VERSION" ]; then
        (cd $CWD/config/manager && $KUSTOMIZE edit set image controller=us-central1-docker.pkg.dev/k8s-staging-images/lws:main)
    fi
}

function pull_upgrade_images {
    if [ -n "$LWS_UPGRADE_FROM_VERSION" ]; then
        docker pull --platform=linux/amd64 "$OLD_CONTROLLER_IMAGE"
        docker pull --platform=linux/amd64 "$PAUSE_IMAGE"
    fi
}

function startup {
    if [ $USE_EXISTING_CLUSTER == 'false' ]
    then
        if [ ! -d "$ARTIFACTS" ]; then
            mkdir -p "$ARTIFACTS"
        fi

        if [ -n "$LWS_UPGRADE_FROM_VERSION" ]; then
            KUBECONFIG_PATH="$(mktemp)"
            export KUBECONFIG="$KUBECONFIG_PATH"
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

# Avoid kind's --all-platforms import path for multi-arch release images.
function kind_load_image {
    local image="$1"
    local archive
    archive="$(mktemp)"
    trap 'rm -f "$archive"' RETURN
    docker save "$image" -o "$archive"

    while IFS= read -r node; do
        docker exec -i "$node" ctr --namespace=k8s.io images import \
            --digests --snapshotter=overlayfs - < "$archive"
    done < <($KIND get nodes --name "$KIND_CLUSTER_NAME")

    rm -f "$archive"
}

function kind_load {
    $KIND load docker-image $IMAGE_TAG --name $KIND_CLUSTER_NAME

    if [ -n "$LWS_UPGRADE_FROM_VERSION" ]; then
        kind_load_image "$OLD_CONTROLLER_IMAGE"
        kind_load_image "$PAUSE_IMAGE"
    fi
}

function install_old_release {
    OLD_MANIFEST="$(mktemp)"
    curl --fail --location --silent --show-error \
        --connect-timeout 10 --max-time 120 \
        --retry 5 --retry-delay 2 \
        --output "$OLD_MANIFEST" "$OLD_MANIFEST_URL"

    $KUBECTL apply --server-side -f "$OLD_MANIFEST"
    $KUBECTL rollout status deployment/lws-controller-manager \
        -n "$LWS_NAMESPACE" --timeout=5m
}

function run_upgrade_phase {
    local phase="$1"
    LWS_UPGRADE_PHASE="$phase" \
    LWS_UPGRADE_SNAPSHOT_PATH="$SNAPSHOT_PATH" \
    IMAGE_TAG="$IMAGE_TAG" \
    LWS_NAMESPACE="$LWS_NAMESPACE" \
        $GINKGO \
        --junit-report="junit-upgrade-${phase}.xml" \
        --output-dir="$ARTIFACTS" \
        -v "$CWD/test/e2e/upgrade"
}

function upgrade_to_current {
    (
        # Set the canonical image name before parent kustomizations transform it.
        local manager_kustomization="$CWD/config/manager/kustomization.yaml"
        local manager_kustomization_backup
        manager_kustomization_backup="$(mktemp)"
        cp "$manager_kustomization" "$manager_kustomization_backup"
        trap 'cp "$manager_kustomization_backup" "$manager_kustomization"; rm -f "$manager_kustomization_backup"' EXIT

        (
            cd "$CWD/config/manager"
            $KUSTOMIZE edit set image controller="$IMAGE_TAG"
        )
        $KUSTOMIZE build "$CWD/test/e2e/config" \
            | $KUBECTL apply --server-side --force-conflicts -f -
    )

    $KUBECTL rollout status deployment/lws-controller-manager \
        -n "$LWS_NAMESPACE" --timeout=5m
}

function upgrade_test_flow {
    echo "Upgrade test: $LWS_UPGRADE_FROM_VERSION -> current"
    install_old_release
    run_upgrade_phase before
    save_controller_logs "$ARTIFACTS/old-controller.log"
    upgrade_to_current
}

function lws_deploy {
    if [ -n "$LWS_UPGRADE_FROM_VERSION" ]; then
        upgrade_test_flow
        return
    fi

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
    if [ -n "$LWS_UPGRADE_FROM_VERSION" ]; then
        run_upgrade_phase after
        return
    fi

    if [ -n "$SCHEDULER_PROVIDER" ]; then
        # Run gang scheduling tests
        $GINKGO --junit-report=junit.xml --output-dir=$ARTIFACTS -v --skip-package=upgrade --focus="leaderWorkerSet e2e gang scheduling tests" $CWD/test/e2e/...
    else
        # Run normal tests, skip gang scheduling tests
        $GINKGO --junit-report=junit.xml --output-dir=$ARTIFACTS -v --skip-package=upgrade --skip="leaderWorkerSet e2e gang scheduling tests" $CWD/test/e2e/...
    fi
}

trap cleanup EXIT
startup
pull_upgrade_images
kind_load
deploy_cert_manager
deploy_gang_scheduler
lws_deploy
run_tests
