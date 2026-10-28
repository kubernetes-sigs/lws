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

DEST_CHART_DIR=${DEST_CHART_DIR:-bin/}

EXTRA_TAG=${EXTRA_TAG:-$(git branch --show-current)}
GIT_TAG=${GIT_TAG:-$(date -u +"v%Y%m%d")-g$(git describe --tags --dirty --always --match="")}
GIT_TAG=$(echo "${GIT_TAG}" | sed -E 's/^(v[0-9]{8})-(.*-[0-9]+-g|g?)([0-9a-f]+(-dirty)?)$/\1-g\3/')

STAGING_IMAGE_REGISTRY=${STAGING_IMAGE_REGISTRY:-us-central1-docker.pkg.dev/k8s-staging-images}
IMAGE_REGISTRY=${IMAGE_REGISTRY:-${STAGING_IMAGE_REGISTRY}/lws}
HELM_CHART_REPO=${HELM_CHART_REPO:-${STAGING_IMAGE_REGISTRY}/lws/charts}
IMAGE_REPO=${IMAGE_REPO:-${IMAGE_REGISTRY}/lws}

HELM=${HELM:-./bin/helm}
YQ=${YQ:-./bin/yq}

readonly k8s_registry="registry.k8s.io/lws"
readonly semver_regex='^v([0-9]+)(\.[0-9]+){1,2}$'
readonly main_chart_version="0.0.0-main"

image_repository=${IMAGE_REPO}
chart_version=${GIT_TAG}
if [[ ${EXTRA_TAG} =~ ${semver_regex} ]]
then
	image_repository=${k8s_registry}/lws
	chart_version=${EXTRA_TAG}
fi

# Package a copy so failures never leave modified values in the source chart.
chart_workdir=$(mktemp -d)
trap 'rm -rf -- "${chart_workdir}"' EXIT
cp -R charts/lws "${chart_workdir}/lws"

# Both chart versions refer to the same build-specific controller image.
"${YQ}" e ".image.manager.repository = \"${image_repository}\" | .image.manager.tag = \"${chart_version}\" | .image.manager.pullPolicy = \"IfNotPresent\"" -i "${chart_workdir}/lws/values.yaml"

"${HELM}" package --version "${chart_version}" --app-version "${chart_version}" "${chart_workdir}/lws" -d "${DEST_CHART_DIR}"
"${HELM}" push "${DEST_CHART_DIR%/}/lws-${chart_version}.tgz" "oci://${HELM_CHART_REPO}"

if [[ ${EXTRA_TAG} != main ]]; then
	exit 0
fi

"${HELM}" package --version "${main_chart_version}" --app-version "${chart_version}" "${chart_workdir}/lws" -d "${DEST_CHART_DIR}"

# Cloud Build's source archive has no .git directory; use the source SHA from
# GIT_TAG. Check immediately before pushing to skip already superseded builds.
# This is a freshness check, not an atomic lock across concurrent publishers.
if [[ ! ${GIT_TAG} =~ ^v[0-9]{8}-g([0-9a-f]{7,40})$ ]]; then
	echo "Cannot publish the rolling chart: GIT_TAG has no verifiable source commit." >&2
	exit 1
fi
build_sha=${BASH_REMATCH[1]}
main_ref=$(git ls-remote https://github.com/kubernetes-sigs/lws.git refs/heads/main)
if [[ ! ${main_ref} =~ ^([0-9a-f]{40})[[:space:]]refs/heads/main$ ]]; then
	echo "Cannot publish the rolling chart: main's source commit could not be verified." >&2
	exit 1
fi
main_sha=${BASH_REMATCH[1]}
if [[ ${main_sha} != "${build_sha}"* ]]; then
	echo "Skipping ${main_chart_version}: build ${build_sha} has been superseded on main."
	exit 0
fi

"${HELM}" push "${DEST_CHART_DIR%/}/lws-${main_chart_version}.tgz" "oci://${HELM_CHART_REPO}"
