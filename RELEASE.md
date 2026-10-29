# Release Process

The Kubernetes Template Project is released on an as-needed basis. The process is as follows:

1. An issue is proposing a new release with a changelog since the last release
1. All [OWNERS](OWNERS) must LGTM this release
1. An OWNER runs `git tag -s $VERSION` and inserts the changelog and pushes the tag with `git push $VERSION`
1. The release issue is closed
1. An announcement email is sent to `dev@kubernetes.io` with the subject `[ANNOUNCE] kubernetes-template-project $VERSION is released`

## Documentation

When preparing a release branch, set `RELEASE_BRANCH=release-X.Y` and
`RELEASE_VERSION=vX.Y.Z` in the Makefile, then run `make prepare-release-branch`.
This also updates the branch's installation examples through `site/hugo.toml`.
On `main`, advance the separate `DEVELOPMENT_DOCS_VERSION` to the next planned
release, run `make prepare-docs-release`, and commit both files. This changes only
the documentation target, not chart or image versions; individual installation
commands need no version edits.

After the release and Helm chart are available, trigger a website rebuild.
The build discovers stable tags and refreshes snapshots from `release-X.Y`.
The latest tagged release is served at `/stable/docs/`; older releases keep
`/vX.Y/docs/`. Backport relevant
documentation fixes to the release branch before rebuilding. Keep unreleased
feature documentation on `main`. See [the local generation and preview guide](site/README.md).

Automatic rebuilds for release-branch changes and new tags still need to be
arranged with the Netlify administrators. Until then, these changes appear on
the next `main` deployment or after an administrator manually rebuilds `main`.

## Development Helm chart

The existing image-publishing job also runs `make helm-chart-push`. Builds from
`main` publish one chart version, `0.0.0-dev-<full-commit-sha>`, to
`oci://us-central1-docker.pkg.dev/k8s-staging-images/lws/charts/lws`. They no longer
publish a dated chart or a rolling alias. Release branches and release tags
retain their existing chart versions.

The publisher resolves the exact abbreviated commit in `GIT_TAG` through
GitHub's commits API. It never looks up the current tip of `main`. The chart's
`appVersion` and image tag retain the existing dated build tag, but
`image.manager.digest` pins the controller to the digest emitted by that
build's `docker buildx --metadata-file` output. Retagging or rebuilding an image
cannot change what an already published chart installs.

The publisher uploads Helm-compatible OCI content by digest, then uses Google
Artifact Registry's create-only tag API. If concurrent builds race, only the
first can create the commit's chart tag. A retry verifies the existing chart's
source commit, version, repository and pinned image digest, then reuses it
without overwriting it, even if the rebuild produced a different image digest.
An unsuccessful attempt can leave untagged content for registry cleanup, but
does not create another named chart version. Authentication, network and
validation errors fail closed rather than being treated as an absent chart.
Development artifacts remain subject to the staging registry's retention
policy; this does not promise permanent availability after registry cleanup.

This uses the publishing job's existing Google credentials and requires its
Artifact Registry tag create/read permissions. No Netlify token, new Prow
configuration or serialized build is needed. A website build can show the
commit-specific command before publication finishes; it becomes usable once
the chart is published, without rebuilding the website.

Run `make test-chart-publishing` to check packaging and branch selection locally.
The tests package real charts but replace registry operations and commit lookups
with local test doubles; nothing is uploaded.
