#!/usr/bin/env bash

# Copyright 2023 The Kubernetes Authors.
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

cd "$(dirname "${0}")/.."
GO_CMD=${1:-go}
CODEGEN_PKG=${2:-bin}
REPO_ROOT="$(git rev-parse --show-toplevel)"


source "${CODEGEN_PKG}/kube_codegen.sh"

# TODO: remove the workaround when the issue is solved in the code-generator
# (https://github.com/kubernetes/code-generator/issues/165).
# Here, we create the soft link named "x-k8s.io" to the parent directory of
# LeaderWorkerSet to ensure the layout required by the kube_codegen.sh script.
ln -s .. sigs.k8s.io
trap "rm sigs.k8s.io" EXIT

kube::codegen::gen_helpers \
    --input-pkg-root sigs.k8s.io/lws/api \
    --output-base "${REPO_ROOT}" \
    --boilerplate "${REPO_ROOT}/hack/boilerplate.go.txt"

kube::codegen::gen_client \
    --with-watch \
    --with-applyconfig \
    --input-pkg-root sigs.k8s.io/lws/api \
    --output-base "$REPO_ROOT" \
    --output-pkg-root sigs.k8s.io/lws/client-go \
    --boilerplate "${REPO_ROOT}/hack/boilerplate.go.txt"