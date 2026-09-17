---
title: "Contribution Guidelines"
linkTitle: "Contribution Guidelines"
weight: 100
description: >
  How to contribute to LWS
---

Welcome to Kubernetes. We are excited about the prospect of you joining our [community](https://git.k8s.io/community)! The Kubernetes community abides by the CNCF [code of conduct](https://github.com/kubernetes/community/blob/master/code-of-conduct.md). Here is an excerpt:

_As contributors and maintainers of this project, and in the interest of fostering an open and welcoming community, we pledge to respect all people who contribute through reporting issues, posting feature requests, updating documentation, submitting pull requests or patches, and other activities._

## Getting Started

We have full documentation on how to get started contributing here:

<!---
If your repo has certain guidelines for contribution, put them here ahead of the general k8s resources
-->

- [Contributor License Agreement](https://git.k8s.io/community/CLA.md) Kubernetes projects require that you sign a Contributor License Agreement (CLA) before we can accept your pull requests
- [Kubernetes Contributor Guide](https://git.k8s.io/community/contributors/guide) - Main contributor documentation, or you can just jump directly to the [contributing section](https://git.k8s.io/community/contributors/guide#contributing)
- [Contributor Cheat Sheet](https://git.k8s.io/community/contributors/guide/contributor-cheatsheet) - Common resources for existing developers

## Documentation examples

Concept pages do not embed YAML manifests directly. Put the manifest under
`site/static/examples/<api>/<topic>/` and render it with the `include`
shortcode:

```
{{</* include file="examples/leaderworkerset/failure-handling/none.yaml" lang="yaml" */>}}
```

The site build fails if the referenced file is missing. Every manifest under
`site/static/examples`, `docs/examples` and `config/samples` is decoded with
strict field checking and run through the admission webhooks by
`test/examples`, which runs as part of `make test`, so a snippet cannot drift
from the API without failing CI.

## Mentorship

- [Mentoring Initiatives](https://git.k8s.io/community/mentoring) - We have a diverse set of mentorship programs available that are always looking for volunteers!

## Contact Information

- [Slack](https://kubernetes.slack.com/archives/C071WA7R9LY)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/wg-serving)