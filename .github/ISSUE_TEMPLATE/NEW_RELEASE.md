---
name: New Release
about: Propose a new release
title: Release v0.x.0
labels: ''
assignees: ''

---

## Release Checklist
<!--
Please do not remove items from the checklist
-->
- [ ] All [OWNERS](https://github.com/kubernetes-sigs/lws/blob/main/OWNERS) must LGTM the release proposal
- [ ] Verify that the changelog in this issue is up-to-date
- [ ] For major or minor releases (v$MAJ.$MIN.0), create a new release branch.
  - [ ] an OWNER creates a vanilla release branch with
        `git branch release-$MAJ.$MIN main`
  - [ ] An OWNER pushes the new release branch with
        `git push --set-upstream upstream release-$MAJ.$MIN`
- [ ] Update the release branch:
  - [ ] Update `RELEASE_BRANCH` and `RELEASE_VERSION` in `Makefile` and run `make prepare-release-branch`
  - [ ] Submit a pull request with the changes: <!-- example kubernetes-sigs/kueue#4698 -->
- [ ] An OWNER [prepares a draft release](https://github.com/kubernetes-sigs/lws/releases)
  - [ ] Write the change log into the draft release.
  - [ ] Run
      `make artifacts IMAGE_REGISTRY=registry.k8s.io/lws GIT_TAG=$VERSION`
      to generate the artifacts and upload the files in the `artifacts` folder
      to the draft release.
- [ ] An OWNER creates a signed tag running
     `git tag -s $VERSION`
      and inserts the changelog into the tag description.
      To perform this step, you need [a PGP key registered on github](https://docs.github.com/en/authentication/managing-commit-signature-verification/checking-for-existing-gpg-keys).
- [ ] An OWNER pushes the tag with
      `git push upstream $VERSION`
  - Triggers prow to build and publish a staging container image
      `us-central1-docker.pkg.dev/k8s-staging-images/lws/lws:$VERSION` and helm chart
      `us-central1-docker.pkg.dev/k8s-staging-images/lws/charts/lws:$CHART_VERSION`.
  - Verify that the image and helm chart is available at [console](https://console.cloud.google.com/artifacts/docker/k8s-staging-images/us-central1/lws)
- [ ] Submit a PR against [k8s.io](https://github.com/kubernetes/k8s.io) ([example PR](https://github.com/kubernetes/k8s.io/pull/7985)) to update image and helm charts: <!-- example kubernetes/k8s.io#3612-->
- [ ] Wait for the PR to be merged and verify that the image `registry.k8s.io/lws/lws:$VERSION` is available, as well as the helm chart `registry.k8s.io/lws/charts/lws:$CHART_VERSION`.
- [ ] Publish the draft release prepared at the [Github releases page](https://github.com/kubernetes-sigs/lws/releases).
- [ ] Add a link to the tagged release in this issue: <!-- example https://github.com/kubernetes-sigs/lws/releases/tag/v0.1.0 -->
- [ ] Update the `main` branch :
  - [ ] Update `RELEASE_VERSION` in `Makefile` and run `make prepare-release-branch`
  - [ ] Submit a pull request with the changes: <!-- example kubernetes-sigs/kueue#4891 -->
- [ ] Send an announcement email to `sig-apps@kubernetes.io`, `sig-scheduling@kubernetes.io`, `wg-serving@kubernetes.io`, and `wg-batch@kubernetes.io` with the subject `[ANNOUNCE] LeaderWorkerSet $VERSION is released`
- [ ] Add a link to the release announcement in this issue: <!-- example https://groups.google.com/a/kubernetes.io/g/wg-batch/c/-gZOrSnwDV4 -->
- [ ] Close this issue


## Changelog
<!--
Describe changes since the last release here.
-->
