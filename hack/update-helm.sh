#!/usr/bin/env bash

# Copyright 2025 The Kubernetes Authors.
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

# Set the source and destination directories
SRC_CRD_DIR=config/crd/bases

DEST_CRD_DIR=charts/lws/templates/crds

YQ=./bin/yq
SED=${SED:-/usr/bin/sed}

# Create the destination directory if it doesn't exist
mkdir -p ${DEST_CRD_DIR}

# Add more excluded files separated by spaces
EXCLUDE_FILES='kustomization.yaml kustomizeconfig.yaml'
# shellcheck disable=SC2086
EXCLUDE_FILES_ARGS=$(printf "! -name %s " $EXCLUDE_FILES)

# Copy all YAML files from the source directory to the destination directory
cp ${SRC_CRD_DIR}/*.yaml ${DEST_CRD_DIR}

search_webhook_line="spec:"
replace_webhook_line=$(cat <<'EOF'
  conversion:
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
)

# Add webhook values in the YAML files
for output_file in "${DEST_CRD_DIR}"/*.yaml; do
  input_file="${output_file%.yaml}.yaml.test"
  mv "$output_file" "$input_file"
  : > "$output_file"
  while IFS= read -r line; do
    echo "$line" >>"$output_file"
    if [[ $line == "$search_webhook_line" ]]; then
      echo "$replace_webhook_line" >>"$output_file"
    fi
  done < "$input_file"
  rm "$input_file"
  $SED -i '/^metadata:.*/a\  labels:\n  {{- include "lws.labels" . | nindent 4 }}' "$output_file"
done
