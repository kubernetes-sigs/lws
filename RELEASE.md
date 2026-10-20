# Release Process

The Kubernetes Template Project is released on an as-needed basis. The process is as follows:

1. An issue is proposing a new release with a changelog since the last release
1. All [OWNERS](OWNERS) must LGTM this release
1. An OWNER runs `git tag -s $VERSION` and inserts the changelog and pushes the tag with `git push $VERSION`
1. The release issue is closed
1. An announcement email is sent to `dev@kubernetes.io` with the subject `[ANNOUNCE] kubernetes-template-project $VERSION is released`

## Documentation

After the release and Helm chart are available, trigger a website rebuild.
The build discovers stable tags and refreshes each `/vX.Y/docs/` snapshot from
`release-X.Y`; the latest tagged release becomes the default. Backport relevant
documentation fixes to the release branch before rebuilding. Keep unreleased
feature documentation on `main`. See [the local generation and preview guide](site/README.md).

Automatic rebuilds for release-branch changes and new tags still need to be
arranged with the Netlify administrators. Until then, these changes appear on
the next `main` deployment or after an administrator manually rebuilds `main`.
