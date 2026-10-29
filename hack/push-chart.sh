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

resolve_development_chart() {
	local build_sha registry_host location project repository package
	# Cloud Build has no .git. Expand the source SHA embedded in its image tag.
	[[ ${GIT_TAG} =~ ^v[0-9]{8}-g[0-9a-f]{7,40}$ ]] || fail "Invalid source commit in GIT_TAG."
	build_sha=${GIT_TAG##*-g}
	source_sha=$(curl --fail --silent --show-error --connect-timeout 10 --max-time 60 \
		"https://api.github.com/repos/kubernetes-sigs/lws/commits/${build_sha}" | "${YQ}" -r '.sha')
	[[ ${source_sha} =~ ^[0-9a-f]{40}$ && ${source_sha} == "${build_sha}"* ]] || fail "Invalid source SHA."
	chart_version="0.0.0-dev-${source_sha}"

	IFS=/ read -r registry_host project repository package <<< "${HELM_CHART_REPO}"
	location=${registry_host%-docker.pkg.dev}
	[[ ${registry_host} == *-docker.pkg.dev && ${location} =~ ^[a-z0-9-]+$ ]] ||
		fail "Development charts require Google Artifact Registry."
	[[ ${project} =~ ^[a-z0-9-]+$ && ${repository} =~ ^[a-z0-9._-]+$ &&
		${package} =~ ^[a-z0-9._/-]+$ ]] ||
		fail "Invalid Artifact Registry project, repository or package."
	package="${package}/lws"
	package=${package//\//%2F} # Artifact Registry uses URL-encoded package names.
	tag_name="projects/${project}/locations/${location}/repositories/${repository}/packages/${package}/tags/${chart_version}"
	registry_args=(--project="${project}" --location="${location}" --repository="${repository}" --package="${package}")
	chart_reference="${HELM_CHART_REPO}/lws"
}

published_chart() {
	gcloud artifacts tags list "${registry_args[@]}" --filter="name=\"${tag_name}\"" --format='value(version)'
}

publish_development_chart() {
	local existing image_digest chart_package chart_digest oras
	# Keep lookup failures distinct from an absent tag.
	existing=$(published_chart)
	if [[ -n ${existing} ]]; then
		echo "Reusing ${chart_reference}:${chart_version}."
		return
	fi

	# Package with the image digest produced by this build, not a mutable tag.
	image_digest=$("${YQ}" -r '.["containerimage.digest"]' "${IMAGE_BUILD_METADATA:?Set IMAGE_BUILD_METADATA to the buildx metadata file}")
	[[ ${image_digest} =~ ${digest_regex} ]] || fail "Invalid build image digest."
	CHART_IMAGE_DIGEST=${image_digest} "${YQ}" e '.image.manager.digest = strenv(CHART_IMAGE_DIGEST)' -i "${chart_workdir}/lws/values.yaml"
	CHART_SOURCE_SHA=${source_sha} "${YQ}" e '.annotations."org.opencontainers.image.revision" = strenv(CHART_SOURCE_SHA)' -i "${chart_workdir}/lws/Chart.yaml"
	"${HELM}" package --version "${chart_version}" --app-version "${GIT_TAG}" "${chart_workdir}/lws" -d "${DEST_CHART_DIR}"
	chart_package="${DEST_CHART_DIR%/}/lws-${chart_version}.tgz"
	"${HELM}" show chart "${chart_package}" | "${YQ}" -o=json '.' > "${chart_workdir}/config.json"

	# Upload without a tag, then atomically create it. Never overwrite a version.
	oras=$(command -v "${ORAS:-./bin/oras}")
	[[ ${oras} == /* ]] || oras=${PWD}/${oras}
	cp "${chart_package}" "${chart_workdir}/chart.tgz"
	(
		cd "${chart_workdir}"
		gcloud auth print-access-token | "${oras}" push --username oauth2accesstoken --password-stdin \
			--image-spec v1.0 --format json \
			--config config.json:application/vnd.cncf.helm.config.v1+json \
			"${chart_reference}" chart.tgz:application/vnd.cncf.helm.chart.content.v1.tar+gzip > push.json
	)
	chart_digest=$("${YQ}" -r '.digest' "${chart_workdir}/push.json")
	[[ ${chart_digest} =~ ${digest_regex} ]] || fail "Invalid uploaded chart digest."
	if ! gcloud artifacts tags create "${chart_version}" "${registry_args[@]}" --version="${chart_digest}"; then
		# A concurrent build may have published first. Never replace its tag.
		existing=$(published_chart)
		[[ -n ${existing} ]] || fail "Chart tag creation failed; no tag was overwritten."
	fi
	echo "Available: ${chart_reference}:${chart_version}."
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
CHART_IMAGE_REPOSITORY=${image_repository} CHART_IMAGE_TAG=${chart_version} "${YQ}" e '
	.image.manager.repository = strenv(CHART_IMAGE_REPOSITORY) |
	.image.manager.tag = strenv(CHART_IMAGE_TAG) |
	.image.manager.pullPolicy = "IfNotPresent"
' -i "${chart_workdir}/lws/values.yaml"

if [[ ${EXTRA_TAG} == main ]]; then
	resolve_development_chart
	publish_development_chart
else
	# Release branches and released charts retain their existing publishing path.
	"${HELM}" package --version "${chart_version}" --app-version "${chart_version}" "${chart_workdir}/lws" -d "${DEST_CHART_DIR}"
	"${HELM}" push "${DEST_CHART_DIR%/}/lws-${chart_version}.tgz" "oci://${HELM_CHART_REPO}"
fi
