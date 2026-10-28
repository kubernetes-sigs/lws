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

The existing image-publishing job also runs `make helm-chart-push`. Every build
keeps its build-specific chart. Only builds from `main` additionally publish
`0.0.0-main` to `oci://us-central1-docker.pkg.dev/k8s-staging-images/lws/charts/lws`.
This rolling chart keeps the build-specific `appVersion` and controller image
tag, so its chart and image come from the same build. Release branches and
release tags never update `0.0.0-main`.

The publisher checks the upstream `main` commit immediately before updating
the rolling chart. Superseded builds skip that update, and an unverifiable
commit fails closed. The lookup and registry push are not atomic; strictly
ordered updates across concurrent builds would require serialized publication
or a shared lock. The rolling chart can lag `main` while a newer build runs or
fails. Use a build-specific chart version when reproducibility is required.

The staging registry must allow mutable tags. No new Prow trigger is required;
the existing job passes the source branch through `EXTRA_TAG`.

Run `make test-chart-publishing` to check packaging and branch selection locally.
The tests package real charts but replace registry pushes and upstream lookups
with local test doubles; nothing is uploaded.
