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

image_repository=${IMAGE_REPO}
chart_version=${GIT_TAG}
if [[ ${EXTRA_TAG} =~ ${semver_regex} ]]
then
	image_repository=${k8s_registry}/lws
	chart_version=${EXTRA_TAG}
fi

# Keep credentials and packaging changes out of the checkout.
umask 077
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

# Cloud Build receives an archive without .git and only the abbreviated SHA in
# GIT_TAG. Resolve that exact commit, never the moving main branch tip.
if [[ ! ${GIT_TAG} =~ ^v[0-9]{8}-g([0-9a-f]{7,40})$ ]]; then
	echo "Cannot publish a development chart: invalid source commit in GIT_TAG." >&2
	exit 1
fi
build_sha=${BASH_REMATCH[1]}
source_sha=$(curl --fail --silent --show-error --connect-timeout 10 --max-time 60 \
	"https://api.github.com/repos/kubernetes-sigs/lws/commits/${build_sha}" | "${YQ}" -r '.sha')
if [[ ! ${source_sha} =~ ^[0-9a-f]{40}$ || ${source_sha} != "${build_sha}"* ]]; then
	echo "Cannot publish a development chart: source commit could not be verified." >&2
	exit 1
fi
chart_version="0.0.0-dev-${source_sha}"

# Read the digest produced by this build, not a registry tag another build can
# move. The digest may differ on a later rebuild of the same source commit.
image_digest=$("${YQ}" -r '.["containerimage.digest"]' "${IMAGE_BUILD_METADATA:?Set IMAGE_BUILD_METADATA to the buildx metadata file}")
if [[ ! ${image_digest} =~ ${digest_regex} ]]; then
	echo "Cannot publish a development chart: missing or invalid build image digest." >&2
	exit 1
fi

# Artifact Registry's CreateTag is create-only, unlike an OCI tag PUT. This is
# what makes first publication atomic across concurrent jobs and retries.
if [[ ! ${HELM_CHART_REPO} =~ ^([a-z0-9-]+)-docker.pkg.dev/([a-z0-9-]+)/([a-z0-9._-]+)/([a-z0-9._/-]+)$ ]]; then
	echo "Development charts require a Google Artifact Registry repository." >&2
	exit 1
fi
location=${BASH_REMATCH[1]}
project=${BASH_REMATCH[2]}
repository=${BASH_REMATCH[3]}
package="${BASH_REMATCH[4]}/lws"
package_path="projects/${project}/locations/${location}/repositories/${repository}/packages/${package//\//%2F}"
package_name=${package_path}
tag_url="https://artifactregistry.googleapis.com/v1/${package_path}/tags/${chart_version}"
chart_reference="${HELM_CHART_REPO}/lws"
ORAS=$(command -v "${ORAS:-./bin/oras}")
if [[ ${ORAS} != /* ]]; then
	ORAS=${PWD}/${ORAS}
fi

access_token=$(gcloud auth print-access-token)
if [[ -z ${access_token} || ${access_token} == *$'\n'* || ${access_token} == *$'\r'* ||
	${access_token} == *'"'* || ${access_token} == *'\'* ]]; then
	echo "Cannot authenticate to Artifact Registry." >&2
	exit 1
fi
printf 'header = "Authorization: Bearer %s"\n' "${access_token}" > "${chart_workdir}/curl-auth"
printf '%s' "${access_token}" | "${ORAS}" login "${HELM_CHART_REPO%%/*}" \
	--username oauth2accesstoken --password-stdin --registry-config "${chart_workdir}/registry.json" >/dev/null
unset access_token

registry_request() {
	curl --silent --show-error --connect-timeout 10 --max-time 60 \
		--config "${chart_workdir}/curl-auth" --output "${chart_workdir}/response.json" \
		--write-out '%{http_code}' "$@"
}

read_tag() {
	local status version
	status=$(registry_request "${tag_url}")
	if [[ ${status} == 404 && $("${YQ}" -r '.error.status' "${chart_workdir}/response.json") == NOT_FOUND ]]; then
		return 1
	fi
	if [[ ${status} != 200 ]]; then
		echo "Cannot read development chart tag (HTTP ${status}); refusing to publish." >&2
		exit 1
	fi
	version=$("${YQ}" -r '.version' "${chart_workdir}/response.json")
	existing_digest=${version##*/versions/}
	if [[ ${version%/versions/*} != "${package_name}" || ! ${existing_digest} =~ ${digest_regex} ]]; then
		echo "Invalid Artifact Registry version for development chart." >&2
		exit 1
	fi
}

reuse_existing() {
	local layer_digest existing_sha existing_version existing_repository existing_image_digest existing_image_tag existing_app_version
	"${ORAS}" manifest fetch --registry-config "${chart_workdir}/registry.json" \
		"${chart_reference}@${existing_digest}" > "${chart_workdir}/manifest.json"
	if [[ $("${YQ}" -r '.config.mediaType' "${chart_workdir}/manifest.json") != application/vnd.cncf.helm.config.v1+json ]]; then
		echo "Existing development artifact is not a Helm chart." >&2
		exit 1
	fi
	layer_digest=$("${YQ}" -r '.layers[] | select(.mediaType == "application/vnd.cncf.helm.chart.content.v1.tar+gzip") | .digest' "${chart_workdir}/manifest.json")
	if [[ ! ${layer_digest} =~ ${digest_regex} ]]; then
		echo "Existing development chart has no unique valid chart layer." >&2
		exit 1
	fi
	"${ORAS}" blob fetch --registry-config "${chart_workdir}/registry.json" \
		--output "${chart_workdir}/existing.tgz" "${chart_reference}@${layer_digest}"
	"${HELM}" show chart "${chart_workdir}/existing.tgz" > "${chart_workdir}/existing-chart.yaml"
	"${HELM}" show values "${chart_workdir}/existing.tgz" > "${chart_workdir}/existing-values.yaml"
	existing_sha=$("${YQ}" -r '.annotations."org.opencontainers.image.revision"' "${chart_workdir}/existing-chart.yaml")
	existing_version=$("${YQ}" -r '.version' "${chart_workdir}/existing-chart.yaml")
	existing_repository=$("${YQ}" -r '.image.manager.repository' "${chart_workdir}/existing-values.yaml")
	existing_image_digest=$("${YQ}" -r '.image.manager.digest' "${chart_workdir}/existing-values.yaml")
	existing_image_tag=$("${YQ}" -r '.image.manager.tag' "${chart_workdir}/existing-values.yaml")
	existing_app_version=$("${YQ}" -r '.appVersion' "${chart_workdir}/existing-chart.yaml")
	if [[ ${existing_sha} != "${source_sha}" || ${existing_version} != "${chart_version}" ||
		${existing_repository} != "${image_repository}" || ! ${existing_image_digest} =~ ${digest_regex} ||
		${existing_image_tag} != "${existing_app_version}" || ! ${existing_image_tag} =~ ^v[0-9]{8}-g([0-9a-f]{7,40})$ ]]; then
		echo "Existing development chart does not match the source commit and pinned controller image; refusing to overwrite it." >&2
		exit 1
	fi
	if [[ ${source_sha} != "${BASH_REMATCH[1]}"* ]]; then
		echo "Existing development chart's controller image refers to another source commit." >&2
		exit 1
	fi
	echo "Reusing ${chart_version}, already pinned to ${existing_repository}@${existing_image_digest}."
}

if read_tag; then
	reuse_existing
	exit 0
fi

"${YQ}" e ".image.manager.digest = \"${image_digest}\"" -i "${chart_workdir}/lws/values.yaml"
"${YQ}" e ".annotations.\"org.opencontainers.image.revision\" = \"${source_sha}\"" -i "${chart_workdir}/lws/Chart.yaml"
"${HELM}" package --version "${chart_version}" --app-version "${GIT_TAG}" "${chart_workdir}/lws" -d "${DEST_CHART_DIR}"
chart_package="${DEST_CHART_DIR%/}/lws-${chart_version}.tgz"
"${HELM}" show chart "${chart_package}" | "${YQ}" -o=json '.' > "${chart_workdir}/config.json"

# Omitting the ORAS reference pushes content by digest only, without writing a
# temporary or canonical tag. Helm recognizes these config and layer types.
cp "${chart_package}" "${chart_workdir}/chart.tgz"
(
	cd "${chart_workdir}"
	"${ORAS}" push --registry-config registry.json --image-spec v1.0 --format json \
		--config config.json:application/vnd.cncf.helm.config.v1+json \
		"${chart_reference}" chart.tgz:application/vnd.cncf.helm.chart.content.v1.tar+gzip > push.json
)
chart_digest=$("${YQ}" -r '.digest' "${chart_workdir}/push.json")
if [[ ! ${chart_digest} =~ ${digest_regex} ]]; then
	echo "Development chart upload did not return a valid manifest digest." >&2
	exit 1
fi
printf '{"version":"%s/versions/%s"}\n' "${package_name}" "${chart_digest}" > "${chart_workdir}/create-tag.json"
status=$(registry_request --request POST --header 'Content-Type: application/json' \
	--data-binary "@${chart_workdir}/create-tag.json" \
	"https://artifactregistry.googleapis.com/v1/${package_path}/tags?tagId=${chart_version}")
if [[ ${status} == 200 || ${status} == 201 ]]; then
	if [[ $("${YQ}" -r '.version' "${chart_workdir}/response.json") != "${package_name}/versions/${chart_digest}" ]]; then
		echo "Development chart tag creation returned an unexpected version." >&2
		exit 1
	fi
	echo "Published ${chart_reference}:${chart_version}, pinned to ${image_repository}@${image_digest}."
elif [[ ${status} == 409 && $("${YQ}" -r '.error.status' "${chart_workdir}/response.json") == ALREADY_EXISTS ]]; then
	# Another build won the create-only race. Never replace its image with ours.
	if ! read_tag; then
		echo "Concurrent chart publication could not be verified." >&2
		exit 1
	fi
	reuse_existing
else
	echo "Cannot create development chart tag (HTTP ${status}); no tag was overwritten." >&2
	exit 1
fi
