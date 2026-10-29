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
readonly digest_regex='^sha256:[0-9a-f]{64}$'

fail() {
	echo "$*" >&2
	exit 1
}

image_repository=${IMAGE_REPO}
chart_version=${GIT_TAG}
if [[ ${EXTRA_TAG} =~ ${semver_regex} ]]
then
	image_repository=${k8s_registry}/lws
	chart_version=${EXTRA_TAG}
fi

# Package a copy so publishing never modifies the checkout.
chart_workdir=$(mktemp -d)
trap 'rm -rf -- "${chart_workdir}"' EXIT
cp -R charts/lws "${chart_workdir}/lws"
"${YQ}" e ".image.manager.repository = \"${image_repository}\" | .image.manager.tag = \"${chart_version}\" | .image.manager.pullPolicy = \"IfNotPresent\"" -i "${chart_workdir}/lws/values.yaml"

# Release branches and released charts retain their existing publishing path.
if [[ ${EXTRA_TAG} != main ]]; then
	"${HELM}" package --version "${chart_version}" --app-version "${chart_version}" "${chart_workdir}/lws" -d "${DEST_CHART_DIR}"
	"${HELM}" push "${DEST_CHART_DIR%/}/lws-${chart_version}.tgz" "oci://${HELM_CHART_REPO}"
	exit 0
fi

# Cloud Build has no .git. Resolve the exact source SHA from its image tag.
[[ ${GIT_TAG} =~ ^v[0-9]{8}-g([0-9a-f]{7,40})$ ]] || fail "Invalid source commit in GIT_TAG."
build_sha=${BASH_REMATCH[1]}
source_sha=$(curl --fail --silent --show-error --connect-timeout 10 --max-time 60 \
	"https://api.github.com/repos/kubernetes-sigs/lws/commits/${build_sha}" | "${YQ}" -r '.sha')
[[ ${source_sha} =~ ^[0-9a-f]{40}$ && ${source_sha} == "${build_sha}"* ]] || fail "Invalid source SHA."
chart_version="0.0.0-dev-${source_sha}"

[[ ${HELM_CHART_REPO} =~ ^([a-z0-9-]+)-docker.pkg.dev/([a-z0-9-]+)/([a-z0-9._-]+)/([a-z0-9._/-]+)$ ]] ||
	fail "Development charts require Google Artifact Registry."
location=${BASH_REMATCH[1]}
project=${BASH_REMATCH[2]}
repository=${BASH_REMATCH[3]}
package="${BASH_REMATCH[4]}/lws"
package=${package//\//%2F}
tag_name="projects/${project}/locations/${location}/repositories/${repository}/packages/${package}/tags/${chart_version}"
registry_args=(--project="${project}" --location="${location}" --repository="${repository}" --package="${package}")
chart_reference="${HELM_CHART_REPO}/lws"
existing_tag() {
	gcloud artifacts tags list "${registry_args[@]}" --filter="name=\"${tag_name}\"" --format='value(version)'
}

# A failed lookup must not be mistaken for an absent tag.
existing=$(existing_tag)
if [[ -n ${existing} ]]; then
	echo "Reusing ${chart_reference}:${chart_version}."
	exit 0
fi

# Pin the image produced by this build, not a mutable registry tag.
image_digest=$("${YQ}" -r '.["containerimage.digest"]' "${IMAGE_BUILD_METADATA:?Set IMAGE_BUILD_METADATA to the buildx metadata file}")
[[ ${image_digest} =~ ${digest_regex} ]] || fail "Invalid build image digest."
"${YQ}" e ".image.manager.digest = \"${image_digest}\"" -i "${chart_workdir}/lws/values.yaml"
"${YQ}" e ".annotations.\"org.opencontainers.image.revision\" = \"${source_sha}\"" -i "${chart_workdir}/lws/Chart.yaml"
"${HELM}" package --version "${chart_version}" --app-version "${GIT_TAG}" "${chart_workdir}/lws" -d "${DEST_CHART_DIR}"
chart_package="${DEST_CHART_DIR%/}/lws-${chart_version}.tgz"
"${HELM}" show chart "${chart_package}" | "${YQ}" -o=json '.' > "${chart_workdir}/config.json"

# Upload by digest only. CreateTag below is the sole, create-only tag write.
ORAS=$(command -v "${ORAS:-./bin/oras}")
[[ ${ORAS} == /* ]] || ORAS=${PWD}/${ORAS}
cp "${chart_package}" "${chart_workdir}/chart.tgz"
(
	cd "${chart_workdir}"
	gcloud auth print-access-token | "${ORAS}" push --username oauth2accesstoken --password-stdin \
		--image-spec v1.0 --format json \
		--config config.json:application/vnd.cncf.helm.config.v1+json \
		"${chart_reference}" chart.tgz:application/vnd.cncf.helm.chart.content.v1.tar+gzip > push.json
)
chart_digest=$("${YQ}" -r '.digest' "${chart_workdir}/push.json")
[[ ${chart_digest} =~ ${digest_regex} ]] || fail "Invalid uploaded chart digest."
if ! gcloud artifacts tags create "${chart_version}" "${registry_args[@]}" --version="${chart_digest}"; then
	# A concurrent build may have published first. Never replace its tag.
	existing=$(existing_tag)
	[[ -n ${existing} ]] || fail "Chart tag creation failed; no tag was overwritten."
fi
echo "Available: ${chart_reference}:${chart_version}."
