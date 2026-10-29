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

# Stop on unhandled errors, unset variables, or failures anywhere in a pipeline.
set -o errexit
set -o nounset
set -o pipefail

# ${NAME:-default} uses the default when NAME is unset or empty.
# $(command) captures a command's output; it is used below for Git and build data.
DEST_CHART_DIR=${DEST_CHART_DIR:-bin/}

EXTRA_TAG=${EXTRA_TAG:-$(git branch --show-current)}
GIT_TAG=${GIT_TAG:-$(date -u +"v%Y%m%d")-g$(git describe --tags --dirty --always --match="")}
# Normalize tags such as v20261028-v1.0.0-12-gabc1234 to v20261028-gabc1234.
# sed's \1 and \3 retain the date and commit; an optional -dirty suffix is preserved.
GIT_TAG=$(echo "${GIT_TAG}" | sed -E 's/^(v[0-9]{8})-(.*-[0-9]+-g|g?)([0-9a-f]+(-dirty)?)$/\1-g\3/')

STAGING_IMAGE_REGISTRY=${STAGING_IMAGE_REGISTRY:-us-central1-docker.pkg.dev/k8s-staging-images}
IMAGE_REGISTRY=${IMAGE_REGISTRY:-${STAGING_IMAGE_REGISTRY}/lws}
HELM_CHART_REPO=${HELM_CHART_REPO:-${STAGING_IMAGE_REGISTRY}/lws/charts}
IMAGE_REPO=${IMAGE_REPO:-${IMAGE_REGISTRY}/lws}

HELM=${HELM:-./bin/helm}
YQ=${YQ:-./bin/yq}
ORAS=${ORAS:-${PWD}/bin/oras}

readonly k8s_registry="registry.k8s.io/lws"
# Regexes match the whole value: v<major>.<minor>[.<patch>], or sha256:<64 hex digits>.
readonly semver_regex='^v([0-9]+)(\.[0-9]+){1,2}$'
readonly digest_regex='^sha256:[0-9a-f]{64}$'

fail() {
	# "$*" combines the arguments; >&2 writes the message to stderr.
	echo "$*" >&2
	exit 1
}

image_repository=${IMAGE_REPO}
chart_version=${GIT_TAG}
# Bash's =~ means "matches this regular expression"; release tags use the public image.
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
	# Cloud Build has no .git. Resolve the commit supplied in its image tag.
	# ##*-g removes everything through the last "-g", leaving the abbreviated commit.
	build_sha=${GIT_TAG##*-g}
	source_sha=$(curl --fail --silent --show-error --connect-timeout 10 --max-time 60 \
		"https://api.github.com/repos/kubernetes-sigs/lws/commits/${build_sha}" | "${YQ}" -r '.sha')
	# "!" negates the regex test: reject anything other than a full 40-digit hex SHA.
	if ! [[ ${source_sha} =~ ^[0-9a-f]{40}$ ]]; then
		fail "Invalid source SHA."
	fi
	# The unquoted * means "any suffix": the full SHA must start with the build SHA.
	if [[ ${source_sha} != "${build_sha}"* ]]; then
		fail "Resolved source SHA does not match the build SHA."
	fi
	# :0:12 takes the first 12 characters, matching the docs' chart-version shortcode.
	chart_version="0.0.0-dev-${source_sha:0:12}"
	# Store the full SHA for collision checks; NAME=value sets a variable just for yq.
	CHART_SOURCE_SHA=${source_sha} "${YQ}" e \
		'.annotations."org.opencontainers.image.revision" = strenv(CHART_SOURCE_SHA)' -i "${chart_workdir}/lws/Chart.yaml"

	# Pin the image produced by this build, not a mutable image tag.
	# :? stops if the metadata path is missing; the quoted key contains a literal dot.
	image_digest=$("${YQ}" -r '.["containerimage.digest"]' "${IMAGE_BUILD_METADATA:?}")
	# Require "sha256:" followed by exactly 64 lowercase hexadecimal characters.
	if ! [[ ${image_digest} =~ ${digest_regex} ]]; then
		fail "Invalid build image digest."
	fi
	# Set a variable for this command only; yq's strenv reads it as a literal string.
	CHART_IMAGE_DIGEST=${image_digest} "${YQ}" e \
		'.image.manager.digest = strenv(CHART_IMAGE_DIGEST)' -i "${chart_workdir}/lws/values.yaml"
fi

# These command-local variables feed yq; the quoted | operators chain YAML edits.
CHART_IMAGE_REPOSITORY=${image_repository} CHART_IMAGE_TAG=${image_tag} "${YQ}" e '
	.image.manager.repository = strenv(CHART_IMAGE_REPOSITORY) |
	.image.manager.tag = strenv(CHART_IMAGE_TAG) |
	.image.manager.pullPolicy = "IfNotPresent"
' -i "${chart_workdir}/lws/values.yaml"

"${HELM}" package --version "${chart_version}" --app-version "${image_tag}" \
	"${chart_workdir}/lws" -d "${DEST_CHART_DIR}"
# %/ removes one trailing slash before appending the archive filename.
chart_package="${DEST_CHART_DIR%/}/lws-${chart_version}.tgz"

if [[ ${EXTRA_TAG} != main ]]; then
	"${HELM}" push "${chart_package}" "oci://${HELM_CHART_REPO}"
	exit 0
fi

# Upload without a tag. Only the atomic create below makes the version visible.
# The shell pipe converts Helm metadata to JSON; > writes it to the config file.
"${HELM}" show chart "${chart_package}" | "${YQ}" -o=json '.' > "${chart_workdir}/config.json"
cp "${chart_package}" "${chart_workdir}/chart.tgz"
# This $() runs in a subshell, so cd stays local; the ORAS template returns only the digest.
chart_digest=$(
	cd "${chart_workdir}"
	# Pipe the token to standard input rather than exposing it as a command argument.
	gcloud auth print-access-token | "${ORAS}" push --username oauth2accesstoken --password-stdin \
		--image-spec v1.0 --format 'go-template={{.digest}}' \
		--config config.json:application/vnd.cncf.helm.config.v1+json \
		"${HELM_CHART_REPO}/lws" chart.tgz:application/vnd.cncf.helm.chart.content.v1.tar+gzip
)
# Validate the uploaded chart's digest with the same SHA-256 format check.
if ! [[ ${chart_digest} =~ ${digest_regex} ]]; then
	fail "Invalid uploaded chart digest."
fi

# Artifact Registry requires a resource name rather than an OCI URL.
# IFS=/ splits on slashes; read keeps backslashes literal (-r); <<< supplies the URL.
IFS=/ read -r host project repository package <<< "${HELM_CHART_REPO}"
# Remove the host suffix to get the region, e.g. us-central1-docker.pkg.dev -> us-central1.
registry_resource="projects/${project}/locations/${host%-docker.pkg.dev}/repositories/${repository}"
# // replaces every slash with %2F, the URL encoding used in package resource names.
tag_resource="${registry_resource}/packages/${package//\//%2F}%2Flws/tags/${chart_version}"
# "!" enters this branch when creation fails, for example because the tag already exists.
if ! gcloud artifacts tags create "${tag_resource}" --version="${chart_digest}"; then
	# On retries, verify the full SHA rather than trusting the short version alone.
	existing_sha=$("${HELM}" show chart "oci://${HELM_CHART_REPO}/lws" --version "${chart_version}" |
		"${YQ}" -r '.annotations."org.opencontainers.image.revision"')
	if [[ ${existing_sha} != "${source_sha}" ]]; then
		fail "Chart version already belongs to another commit."
	fi
fi
