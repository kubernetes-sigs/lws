# KEP-552: allow worker size updates

<!--
This is the title of your KEP. Keep it short, simple, and descriptive. A good
title can help communicate what the KEP is and should be considered as part of
any review.
-->

<!--
A table of contents is helpful for quickly jumping to sections of a KEP and for
highlighting any additional information provided beyond the standard KEP
template.

Ensure the TOC is wrapped with
  <code>&lt;!-- toc --&rt;&lt;!-- /toc --&rt;</code>
tags, and then generate with `hack/update-toc.sh`.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories (Optional)](#user-stories-optional)
    - [Story 1](#story-1)
    - [Story 2](#story-2)
  - [Notes/Constraints/Caveats (Optional)](#notesconstraintscaveats-optional)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Test Plan](#test-plan)
      - [Prerequisite testing updates](#prerequisite-testing-updates)
      - [Unit tests](#unit-tests)
      - [Integration tests](#integration-tests)
      - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

<!--
This section is incredibly important for producing high-quality, user-focused
documentation such as release notes or a development roadmap. It should be
possible to collect this information before implementation begins, in order to
avoid requiring implementors to split their attention between writing release
notes and implementing the feature itself. KEP editors and SIG Docs
should help to ensure that the tone and content of the `Summary` section is
useful for a wide audience.

A good summary is probably at least a paragraph in length.

Both in this section and below, follow the guidelines of the [documentation
style guide]. In particular, wrap lines to a reasonable length, to make it
easier for reviewers to cite specific portions, and to minimize diff churn on
updates.

[documentation style guide]: https://github.com/kubernetes/community/blob/master/contributors/guide/style-guide.md
-->

This KEP aims to allow resizing of workers by introducing a new field that defines the type of resize strategy. The current strategy will become `None` to keep the same behavior. `Recreate` will be added. The `spec.leaderWorkerTemplate.size` field will become editable by relaxing the validating webhook.

## Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this KEP.  Describe why the change is important and the benefits to users. The
motivation section can optionally provide links to [experience reports] to
demonstrate the interest in a KEP within the wider Kubernetes community.

[experience reports]: https://github.com/golang/go/wiki/ExperienceReports
-->

It is not currently possible to update the `spec.leaderWorkerTemplate.size` field. This complicates updates to existing `LeaderWorkerSets` by forcing to recreate resources, as well a being a blocker for more innovative topologies where in-place resize of worker pools is supported beyond `spec.replicas`.

### Goals

<!--
List the specific goals of the KEP. What is it trying to achieve? How will we
know that this has succeeded?
-->

The current goals are:
- Allow to update the `spec.leaderWorkerTemplate.size` field.
- Add an optional field to control the way replicas are resized.


### Non-Goals

<!--
What is out of scope for this KEP? Listing non-goals helps to focus discussion
and make progress.
-->

- This KEP doesn't propose a way to target `spec.leaderWorkerTemplate.size` with an autoscaling mechanism.
- InPlace update policy is not part of this KEP since it adds more complexities like dynamically changing the topology envs at runtime. We may visit this in the future if highly required by the community.

## Proposal

<!--
This is where we get down to the specifics of what the proposal actually is.
This should have enough detail that reviewers can understand exactly what
you're proposing, but should not include things like API designs or
implementation. What is the desired outcome and how do we measure success?.
The "Design Details" section below is for the real
nitty-gritty.
-->

### User Stories (Optional)

<!--
Detail the things that people will be able to do if this KEP is implemented.
Include as much detail as possible so that people can understand the "how" of
the system. The goal here is to make this feel real for users without getting
bogged down.
-->

#### Story 1

As a develop running inference workloads with `LeaderWorkerSet`, I should be able to quickly iterate on my deployments configuration during development. Currently, the only way to update the size of the `LeaderWorkerSet` is to delete and recreate the resource. If `spec.leaderWorkerTemplate.size` is made editable, I will be able to simply edit and apply the `LeaderWorkerSet` instances using kubectl rather than deleting and recreating them. This would improve my developer experience.

#### Story 2

As a system administrator providing GitOps deployment systems to my developers, I currently have to either manually delete `LeaderWorkerSets` when a developer want to update the `spec.leaderWorkerTemplate.size` field, or ask them to do a 2 commit update, first delete the resource in our GitOps repo, then recreate it with a new size.
If `spec.leaderWorkerTemplate.size` is made editable, `LeaderWorkerSet` would be much more ideal to work with in a GitOps environment.

### Notes/Constraints/Caveats (Optional)

<!--
What are the caveats to the proposal?
What are some important details that didn't come across above?
Go in to as much detail as necessary here.
This might be a good place to talk about core concepts and how they relate.
-->

### Risks and Mitigations

<!--
What are the risks of this proposal, and how do we mitigate? Think broadly.
For example, consider both security and how this will impact the larger
Kubernetes ecosystem.

How will security be reviewed, and by whom?

How will UX be reviewed, and by whom?

Consider including folks who also work outside the SIG or subproject.
-->

## Design Details

<!--
This section should contain enough information that the specifics of your
change are understandable. This may include API specs (though not always
required) or even code snippets. If there's any ambiguity about HOW your
proposal will be implemented, this is the place to discuss them.
-->

A new field is added to the `LeaderWorkerTemplate` struct: `ResizePolicy`. It controls the way updates to `spec.leaderWorkerTemplate.size` are handled

```go
type ResizePolicyType string

const ResizePolicyNone ResizePolicyType = "None"
const ResizePolicyRecreate ResizePolicyType = "Recreate"

```

```go
type LeaderWorkerTemplate struct {

    // ResizePolicy defines how to handle worker size updates.
    // None doesn't allow to update spec.leaderWorkerTemplate.size.
    // Recreate updates the size and restarts existing pods.
	// +kubebuilder:default=None
	// +kubebuilder:validation:Enum={None,Recreate}
	// +optional
    ResizePolicy ResizePolicyType `json:"resizePolicy,omitempty`
}
```

An update will be made to the `LeaderWorkerSet` validating webhook to retain its current behavior which prevents updates to `spec.leaderWorkerTemplate.size` only if the `ResizePolicy` is `None`.
For the `Recreate` policy, a new annotation `leaderworkerset.sigs.k8s.io/size` describing the targeted size will be added to the `StatefulSets` pod template to trigger a rollout of the worker pods. If `RollingUpdate` is set, an update to `spec.leaderWorkerTemplate.size` will trigger a rolling update. 

### Test Plan

<!--
**Note:** *Not required until targeted at a release.*
The goal is to ensure that we don't accept enhancements with inadequate testing.

All code is expected to have adequate tests (eventually with coverage
expectations). Please adhere to the [Kubernetes testing guidelines][testing-guidelines]
when drafting this test plan.

[testing-guidelines]: https://git.k8s.io/community/contributors/devel/sig-testing/testing.md
-->

[x] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

##### Prerequisite testing updates

<!--
Based on reviewers feedback describe what additional tests need to be added prior
implementing this enhancement to ensure the enhancements have also solid foundations.
-->

##### Unit tests

<!--
In principle every added code should have complete unit test coverage, so providing
the exact set of tests will not bring additional value.
However, if complete unit test coverage is not possible, explain the reason of it
together with explanation why this is acceptable.
-->

<!--
Additionally, for Alpha try to enumerate the core package you will be touching
to implement this enhancement and provide the current unit coverage for those
in the form of:
- <package>: <date> - <current test coverage>
The data can be easily read from:
https://testgrid.k8s.io/sig-testing-canaries#ci-kubernetes-coverage-unit

This can inform certain test coverage improvements that we want to do before
extending the production code to implement this enhancement.
-->

- `<package>`: `<date>` - `<test coverage>`

##### Integration tests

<!--
Integration tests are contained in k8s.io/kubernetes/test/integration.
Integration tests allow control of the configuration parameters used to start the binaries under test.
This is different from e2e tests which do not allow configuration of parameters.
Doing this allows testing non-default options and multiple different and potentially conflicting command line options.
-->

<!--
This question should be filled when targeting a release.
For Alpha, describe what tests will be added to ensure proper quality of the enhancement.

For Beta and GA, add links to added tests together with links to k8s-triage for those tests:
https://storage.googleapis.com/k8s-triage/index.html
-->

Three additional integration tests will be added:

- Once resizePolicy=recreate, resize will trigger the recreate process successfully
- Once resizePolicy=recreate, resize in rolling update will trigger the recreate process successfully
- Once resizePolicy=recreate, the validating webhook will allow modifications to `spec.leaderWorkerTemplate.size`


##### e2e tests

<!--
This question should be filled when targeting a release.
For Alpha, describe what tests will be added to ensure proper quality of the enhancement.

For Beta and GA, add links to added tests together with links to k8s-triage for those tests:
https://storage.googleapis.com/k8s-triage/index.html

We expect no non-infra related flakes in the last month as a GA graduation criteria.
-->

A new end-to-end test will be added:

- Resize will trigger the recreate process once resizePolicy=recreate and recover the service as expected.

### Graduation Criteria

<!--

Clearly define what it means for the feature to be implemented and
considered stable.

If the feature you are introducing has high complexity, consider adding graduation
milestones with these graduation criteria:
- [Maturity levels (`alpha`, `beta`, `stable`)][maturity-levels]
- [Feature gate][feature gate] lifecycle
- [Deprecation policy][deprecation-policy]

[feature gate]: https://git.k8s.io/community/contributors/devel/sig-architecture/feature-gates.md
[maturity-levels]: https://git.k8s.io/community/contributors/devel/sig-architecture/api_changes.md#alpha-beta-and-stable-versions
[deprecation-policy]: https://kubernetes.io/docs/reference/using-api/deprecation-policy/
-->

## Implementation History

<!--
Major milestones in the lifecycle of a KEP should be tracked in this section.
Major milestones might include:
- the `Summary` and `Motivation` sections being merged, signaling SIG acceptance
- the `Proposal` section being merged, signaling agreement on a proposed design
- the date implementation started
- the first Kubernetes release where an initial version of the KEP was available
- the version of Kubernetes where the KEP graduated to general availability
- when the KEP was retired or superseded
-->

2025-06-09: KEP initialized and submitted for review
2025-07-10: KEP implementation submitted for review
2025-08-05: Implementation revised to avoid additions to the API surface

## Drawbacks

<!--
Why should this KEP _not_ be implemented?
-->

## Alternatives

<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->
