#!/bin/bash

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

CRD_SRC_DIR=${CRD_SRC_DIR:-./config/crd/bases}
CRD_DST_DIR=${CRD_DST_DIR:-./charts/lws/templates/crds}
YQ=${YQ:-./bin/yq}

echo "1. Prepare temporary working directory"
TMP_CRD_DIR=$(mktemp -d)
conversion_file="${TMP_CRD_DIR}/conversion.yaml"

cleanup() {
  echo "🧹 Clean up temporary files..."
  rm -rf "${TMP_CRD_DIR}"
}

trap cleanup EXIT INT

cp -v "${CRD_SRC_DIR}"/*.yaml "${TMP_CRD_DIR}/"

echo "2. Format all CRD files"
${YQ} eval . -I2 -P -i "${TMP_CRD_DIR}"/*.yaml

# Prepare the conversion configuration content
conversion_file="${TMP_CRD_DIR}/conversion.tmp"
cat <<EOF > "${conversion_file}"
strategy: Webhook
webhook:
  clientConfig:
    service:
      name: lws-webhook-service
      namespace: '{{ .Release.Namespace }}'
      path: /convert
  conversionReviewVersions:
    - v1
EOF

echo "3. Inject the conversion webhook"
${YQ} -i 'with(.spec; . = {"conversion": load("'"${conversion_file}"'")} + .)' \
  "${TMP_CRD_DIR}/leaderworkerset.x-k8s.io_leaderworkersets.yaml"

echo "4. Replace old CRDs with updated ones"
rm -rf "${CRD_DST_DIR}"/*.yaml
mv "${TMP_CRD_DIR}"/*.yaml "${CRD_DST_DIR}/"

echo "✅ CRDs synced safely and conversion field injected successfully."
