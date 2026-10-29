# Release Process

The Kubernetes Template Project is released on an as-needed basis. The process is as follows:

1. An issue is proposing a new release with a changelog since the last release
1. All [OWNERS](OWNERS) must LGTM this release
1. An OWNER runs `git tag -s $VERSION` and inserts the changelog and pushes the tag with `git push $VERSION`
1. The release issue is closed
1. An announcement email is sent to `dev@kubernetes.io` with the subject `[ANNOUNCE] kubernetes-template-project $VERSION is released`

## Documentation

After the release and Helm chart are available, trigger a website rebuild.
The build discovers stable tags and refreshes snapshots from `release-X.Y`.
The latest tagged release is served at `/stable/docs/`; older releases keep
`/vX.Y/docs/`. Backport relevant
documentation fixes to the release branch before rebuilding. Keep unreleased
feature documentation on `main`. See [the local generation and preview guide](site/README.md).

Automatic rebuilds for release-branch changes and new tags still need to be
arranged with the Netlify administrators. Until then, these changes appear on
the next `main` deployment or after an administrator manually rebuilds `main`.

## Development Helm charts

Main publishes `0.0.0-dev-<full-commit-sha>` with the controller image pinned by
its build digest. Artifact Registry's create-only tag command prevents retries
or concurrent builds from overwriting an existing version. The website uses
the same commit, so its installation command works once publishing finishes,
without a Netlify rebuild. Staging artifacts are eligible for deletion after
[90 days](https://github.com/kubernetes/k8s.io/blob/main/infra/gcp/terraform/k8s-staging-images/registries.tf#L131-L140).
