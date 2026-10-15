#!/usr/bin/env bash

# Copyright 2026 The Kubernetes Authors.
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

# Adapted from kubernetes-sigs/kueue's hack/releasing/sync-notes.sh.
# Requires gh, jq, and Kubernetes release-notes v0.20.1 on PATH.

set -o errexit
set -o nounset
set -o pipefail

if [[ "$#" -ne 1 ]]; then
  echo "Usage: $0 <version>"
  echo "Example: $0 v0.11.2"
  echo "Set UPSTREAM_REMOTE (default: upstream) to the kubernetes-sigs/lws remote."
  exit 2
fi

RELEASE_VERSION="$1"
VERSION_PATTERN='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
if [[ ! "${RELEASE_VERSION}" =~ ${VERSION_PATTERN} ]]; then
  echo "Invalid release version. Expected a version such as v0.11.2." >&2
  exit 2
fi

for tool in git gh jq release-notes; do
  if ! command -v "${tool}" > /dev/null; then
    echo "Required tool not found: ${tool}. Install gh, jq, and release-notes v0.20.1." >&2
    exit 1
  fi
done

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "${REPO_ROOT}"
UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
MAIN_REPO_ORG=kubernetes-sigs
MAIN_REPO_NAME=lws
MAIN_REPO="${MAIN_REPO_ORG}/${MAIN_REPO_NAME}"

case "$(git remote get-url "${UPSTREAM_REMOTE}")" in
  "https://github.com/${MAIN_REPO}"|"https://github.com/${MAIN_REPO}.git"|"git@github.com:${MAIN_REPO}.git"|"ssh://git@github.com/${MAIN_REPO}.git") ;;
  *)
    echo "UPSTREAM_REMOTE must point to ${MAIN_REPO} on GitHub." >&2
    exit 1
    ;;
esac

export GH_TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-$(gh auth token --hostname github.com)}}"
export GITHUB_TOKEN="${GH_TOKEN}"
gh auth status --hostname github.com
git fetch "${UPSTREAM_REMOTE}" --tags

find_previous_version() {
  local major minor patch tag t_major t_minor t_patch
  IFS='.' read -r major minor patch <<< "${RELEASE_VERSION#v}"
  for tag in $(git tag --sort=-version:refname); do
    [[ "${tag}" =~ ${VERSION_PATTERN} ]] || continue
    IFS='.' read -r t_major t_minor t_patch <<< "${tag#v}"
    # Major and minor releases compare against the previous minor's .0 tag.
    if (( patch == 0 && t_patch != 0 )); then
      continue
    fi
    if (( t_major < major ||
          (t_major == major && t_minor < minor) ||
          (t_major == major && t_minor == minor && t_patch < patch) )); then
      echo "${tag}"
      return 0
    fi
  done
  echo "No previous release tag found for ${RELEASE_VERSION}." >&2
  return 1
}

IFS='.' read -r MAJOR MINOR PATCH <<< "${RELEASE_VERSION#v}"
HEAD_BRANCH="release-${MAJOR}.${MINOR}"
if ! git show-ref --verify --quiet "refs/remotes/${UPSTREAM_REMOTE}/${HEAD_BRANCH}"; then
  if (( PATCH != 0 )); then
    echo "Release branch ${UPSTREAM_REMOTE}/${HEAD_BRANCH} not found." >&2
    exit 1
  fi
  HEAD_BRANCH=main
fi

PREVIOUS_VERSION="$(find_previous_version)"
START_SHA="$(git rev-parse "${PREVIOUS_VERSION}^{commit}")"
END_SHA="$(git rev-parse "refs/remotes/${UPSTREAM_REMOTE}/${HEAD_BRANCH}^{commit}")"
echo "+++ Generating notes for ${PREVIOUS_VERSION}..${END_SHA} on ${HEAD_BRANCH}"

RELEASE_ISSUE_NAME="Release ${RELEASE_VERSION}"
RELEASE_ISSUE_NUMBER="$(gh issue list --repo="${MAIN_REPO}" --state=open \
  --search="in:title \"${RELEASE_ISSUE_NAME}\"" --json number,title |
  jq -r --arg title "${RELEASE_ISSUE_NAME}" '
    [.[] | select(.title == $title)] |
    if length == 1 then .[0].number
    else error("Expected exactly one open issue titled " + $title)
    end')"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
CHANGELOG_FILE="${TMP_DIR}/changelog.md"
gh issue view "${RELEASE_ISSUE_NUMBER}" --repo="${MAIN_REPO}" \
  --json body --jq '.body' > "${TMP_DIR}/issue.md"

GITHUB_TOKEN="${GITHUB_TOKEN}" release-notes \
  --org="${MAIN_REPO_ORG}" \
  --repo="${MAIN_REPO_NAME}" \
  --branch="${HEAD_BRANCH}" \
  --start-sha="${START_SHA}" \
  --end-sha="${END_SHA}" \
  --dependencies=false \
  --required-author="" \
  --output="${CHANGELOG_FILE}"

if [[ ! -s "${CHANGELOG_FILE}" ]]; then
  echo "The release notes generator did not produce a non-empty changelog." >&2
  exit 1
fi

# Replace only the marked changelog section, preserving the release checklist.
if ! awk -v previous_version="${PREVIOUS_VERSION}" -v changelog_file="${CHANGELOG_FILE}" '
  /^[[:space:]]*<!-- release-changelog-start -->[[:space:]]*$/ {
    if (seen_start || seen_end) exit 1
    seen_start = 1
    in_changelog_section = 1
    print
    while ((getline line < changelog_file) > 0) print line
    close(changelog_file)
    next
  }
  /^[[:space:]]*<!-- release-changelog-end -->[[:space:]]*$/ {
    if (!in_changelog_section || seen_end) exit 1
    seen_end = 1
    in_changelog_section = 0
  }
  !in_changelog_section { print }
  END {
    if (!seen_start || !seen_end || in_changelog_section) exit 1
  }
' "${TMP_DIR}/issue.md" > "${TMP_DIR}/issue-new.md"; then
  echo "The release issue must contain one release-changelog-start/end marker pair; see the New Release template." >&2
  exit 1
fi

cat "${TMP_DIR}/issue-new.md"
echo
read -r -p "Update ${MAIN_REPO}#${RELEASE_ISSUE_NUMBER} (${RELEASE_ISSUE_NAME})? [y/N] " REPLY
if [[ ! "${REPLY}" =~ ^[yY]$ ]]; then
  echo "Aborting."
  exit 1
fi

gh issue view "${RELEASE_ISSUE_NUMBER}" --repo="${MAIN_REPO}" \
  --json body --jq '.body' > "${TMP_DIR}/issue-latest.md"
if ! cmp -s "${TMP_DIR}/issue.md" "${TMP_DIR}/issue-latest.md"; then
  echo "The release issue changed during generation. Run the script again." >&2
  exit 1
fi
gh issue edit "${RELEASE_ISSUE_NUMBER}" --repo="${MAIN_REPO}" --body-file="${TMP_DIR}/issue-new.md"
