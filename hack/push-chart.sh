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
# Normalize tags such as v20261028-v1.0.0-12-gabc1234 to v20261028-gabc1234.
GIT_TAG=$(echo "${GIT_TAG}" | sed -E 's/^(v[0-9]{8})-(.*-[0-9]+-g|g?)([0-9a-f]+(-dirty)?)$/\1-g\3/')

STAGING_IMAGE_REGISTRY=${STAGING_IMAGE_REGISTRY:-us-central1-docker.pkg.dev/k8s-staging-images}
IMAGE_REGISTRY=${IMAGE_REGISTRY:-${STAGING_IMAGE_REGISTRY}/lws}
HELM_CHART_REPO=${HELM_CHART_REPO:-${STAGING_IMAGE_REGISTRY}/lws/charts}
IMAGE_REPO=${IMAGE_REPO:-${IMAGE_REGISTRY}/lws}

HELM=${HELM:-./bin/helm}
YQ=${YQ:-./bin/yq}
ORAS=${ORAS:-${PWD}/bin/oras}

readonly k8s_registry="registry.k8s.io/lws"
readonly semver_regex='^v([0-9]+)(\.[0-9]+){1,2}$'
readonly digest_regex='^sha256:[0-9a-f]{64}$'

fail() {
	echo "$*" >&2
	exit 1
}

image_repository=${IMAGE_REPO}
chart_version=${GIT_TAG}
# Release tags use the public image.
if [[ ${EXTRA_TAG} =~ ${semver_regex} ]]
then
	image_repository=${k8s_registry}/lws
	chart_version=${EXTRA_TAG}
fi
image_tag=${chart_version}

# Package a copy so publishing never modifies the checkout.
chart_workdir=$(mktemp -d)
# Run this cleanup on any exit, including errors; only the temporary copy is removed.
trap 'rm -rf -- "${chart_workdir}"' EXIT
cp -R charts/lws "${chart_workdir}/lws"

if [[ ${EXTRA_TAG} == main ]]; then
	# Resolve the exact build commit, even if main has advanced since this build started.
	build_sha=${GIT_TAG##*-g}
	source_sha=${PULL_BASE_SHA:-}
	if [[ -z ${source_sha} ]]; then
		# The current Cloud Build job omits .git and PULL_BASE_SHA. Fetch only commit
		# history, avoiding GitHub's unauthenticated API quota and source downloads.
		git clone --quiet --bare --filter=tree:0 --single-branch --branch main --no-tags \
			https://github.com/kubernetes-sigs/lws.git "${chart_workdir}/source.git"
		source_sha=$(git --git-dir="${chart_workdir}/source.git" rev-parse --verify "${build_sha}^{commit}")
	fi
	if ! [[ ${source_sha} =~ ^[0-9a-f]{40}$ ]]; then
		fail "Invalid source SHA."
	fi
	if [[ ${source_sha} != "${build_sha}"* ]]; then
		fail "Resolved source SHA does not match the build SHA."
	fi
	chart_version="0.0.0-main-${source_sha:0:7}"
	# Store the full SHA for collision checks.
	CHART_SOURCE_SHA=${source_sha} "${YQ}" e \
		'.annotations."org.opencontainers.image.revision" = strenv(CHART_SOURCE_SHA)' -i "${chart_workdir}/lws/Chart.yaml"

	# Pin the image produced by this build, not a mutable image tag.
	image_digest=$("${YQ}" -r '.["containerimage.digest"]' "${IMAGE_BUILD_METADATA:?}")
	if ! [[ ${image_digest} =~ ${digest_regex} ]]; then
		fail "Invalid build image digest."
	fi
	CHART_IMAGE_DIGEST=${image_digest} "${YQ}" e \
		'.image.manager.digest = strenv(CHART_IMAGE_DIGEST)' -i "${chart_workdir}/lws/values.yaml"
fi

CHART_IMAGE_REPOSITORY=${image_repository} CHART_IMAGE_TAG=${image_tag} "${YQ}" e '
	.image.manager.repository = strenv(CHART_IMAGE_REPOSITORY) |
	.image.manager.tag = strenv(CHART_IMAGE_TAG) |
	.image.manager.pullPolicy = "IfNotPresent"
' -i "${chart_workdir}/lws/values.yaml"

"${HELM}" package --version "${chart_version}" --app-version "${image_tag}" \
	"${chart_workdir}/lws" -d "${DEST_CHART_DIR}"
chart_package="${DEST_CHART_DIR%/}/lws-${chart_version}.tgz"

if [[ ${EXTRA_TAG} != main ]]; then
	"${HELM}" push "${chart_package}" "oci://${HELM_CHART_REPO}"
	exit 0
fi

# Upload without a tag. Only the atomic create below makes the version visible.
"${HELM}" show chart "${chart_package}" | "${YQ}" -o=json '.' > "${chart_workdir}/config.json"
cp "${chart_package}" "${chart_workdir}/chart.tgz"
chart_digest=$(
	cd "${chart_workdir}"
	# Pipe the token to standard input rather than exposing it as a command argument.
	gcloud auth print-access-token | "${ORAS}" push --username oauth2accesstoken --password-stdin \
		--image-spec v1.0 --format 'go-template={{.digest}}' \
		--config config.json:application/vnd.cncf.helm.config.v1+json \
		"${HELM_CHART_REPO}/lws" chart.tgz:application/vnd.cncf.helm.chart.content.v1.tar+gzip
)
if ! [[ ${chart_digest} =~ ${digest_regex} ]]; then
	fail "Invalid uploaded chart digest."
fi

# Artifact Registry requires a resource name rather than an OCI URL.
IFS=/ read -r host project repository package <<< "${HELM_CHART_REPO}"
registry_resource="projects/${project}/locations/${host%-docker.pkg.dev}/repositories/${repository}"
tag_resource="${registry_resource}/packages/${package//\//%2F}%2Flws/tags/${chart_version}"
if ! gcloud artifacts tags create "${tag_resource}" --version="${chart_digest}"; then
	# On retries, verify the full SHA rather than trusting the short version alone.
	existing_sha=$("${HELM}" show chart "oci://${HELM_CHART_REPO}/lws" --version "${chart_version}" |
		"${YQ}" -r '.annotations."org.opencontainers.image.revision"')
	if [[ ${existing_sha} != "${source_sha}" ]]; then
		fail "Chart version already belongs to another commit."
	fi
fi
