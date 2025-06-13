# KEP-511: Partition Update

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
  - [LeaderWorkerSet API](#leaderworkerset-api)
  - [Implementation](#implementation)
    - [lws controller](#lws-controller)
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

This KEP is to enable lws to have the ability to perform partitioned updates by adding an API field that defines the groups whose ordinal is not less than `partition` can be updated.

## Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this KEP.  Describe why the change is important and the benefits to users. The
motivation section can optionally provide links to [experience reports] to
demonstrate the interest in a KEP within the wider Kubernetes community.

[experience reports]: https://github.com/golang/go/wiki/ExperienceReports
-->

This KEP is to enable lws to have the ability to perform partitioned updates, facilitating canary or gray release. In disaggregated prefilling cases, it can be utilized to align the version ratio between p and d.

### Goals

- Add partitionfield to ebable partitioned updates for LWS, similar to the [StatefuleSet partition](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#partitions).
<!--
List the specific goals of the KEP. What is it trying to achieve? How will we
know that this has succeeded?
-->

### Non-Goals

<!--
What is out of scope for this KEP? Listing non-goals helps to focus discussion
and make progress.
-->

## Proposal

<!--
This is where we get down to the specifics of what the proposal actually is.
This should have enough detail that reviewers can understand exactly what
you're proposing, but should not include things like API designs or
implementation. What is the desired outcome and how do we measure success?.
The "Design Details" section below is for the real
nitty-gritty.
-->
We propose a new field `Partition` to `RollingUpdateConfiguration` struct like `sts.Partition` indicates the ordinal at which the LWS should be partitioned for updates. During a rolling update, the groups from `Replicas-1` to `Partition` are updated.

### User Stories (Optional)

<!--
Detail the things that people will be able to do if this KEP is implemented.
Include as much detail as possible so that people can understand the "how" of
the system. The goal here is to make this feel real for users without getting
bogged down.
-->

#### Story 1
In the disaggregated prefilling case, the prefill role and decode role are deployed using two separate lws. The prefill role `Replicas` is 4 and decode is 2. When there is a new release, we can set prefill role `Partition` to 2 and set decode to 1. Half of the prefill and decoder groups will update to the new version.

#### Story 2
For the canary deployment, through the partitioned update and custom routing strategy, only some user requests can be served by the new version endpoints to observe whether the feature release meets expectations.

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
### LeaderWorkerSet API
```go
// leaderworkerset_types.go
type RollingUpdateConfiguration struct {
    // Partition indicates the ordinal at which 
    // the LWS should be partitioned for updates. 
    // The default value is 0.
    // +optional
    Partition *int32
    
    MaxUnavailable intstr.IntOrString `json:"maxUnavailable,omitempty"`
    
    MaxSurge intstr.IntOrString `json:"maxSurge,omitempty"`
}
```

### Implementation
#### lws controller
In the `rollingUpdatePartition`function, an intermediate variable "partition" is calculated, which means the groups from `partition`to `replicas-1` will be updated. The function returns `min(partition, currentPartition)` that means Partition moves in one direction to make it simple. 

Now, we set `lwsPartition = lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition` and reset `partition = max(partition, lwsPartition)`.

```go
func rollingUpdatePartition(...) int32 {
    // ...
    // ...
    partition = ...

    // new code, to implement lws partition update
    lwsPartition = getLwsPartition(lws)

    return max(lwsPartition, partition)
}
```

During a rolling update, the `partition` field updated to the leader sts will not be smaller than `lwsPartition`, which keeps the groups smaller than `lwsPartition` will not be updated.

Next, The conditions which indicates whether the rolling update is completed need to be updated. The previous logic was that before all replicas were modified, `UpdateInProgress` is true. Now update to before all replicas whose index no less than `lwsPartition` are modified. The `Available` Condition is set to `True` when all replicas whose index no less than `lwsPartition` are updated and at least the minimum available replicas are up and running.

We alse need to change `updateConditions` func to return `allUpdateDone` which means all replicas have been updated and completed, not `updateDone`. The adjustment is made to clean up other revisions when all replicas have been updated, rather than immediately upon completion of the partition update.
```go
// old code
if updateDone {
    if err := revisionutils.TruncateRevisions(ctx, r.Client, lws, revisionutils.GetRevisionKey(revision)); err != nil {
        return ctrl.Result{}, err
    }
}

// new code
if allUpdateDone {
    if err := revisionutils.TruncateRevisions(ctx, r.Client, lws, revisionutils.GetRevisionKey(revision)); err != nil {
        return ctrl.Result{}, err
    }
}
```

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

<!-- - `<package>`: `<date>` - `<test coverage>` -->
- Test whether the `partition` value is correct.

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

- Test the partitioned rolling update is working as expected with maxUnavailable step by step.

##### e2e tests

<!--
This question should be filled when targeting a release.
For Alpha, describe what tests will be added to ensure proper quality of the enhancement.

For Beta and GA, add links to added tests together with links to k8s-triage for those tests:
https://storage.googleapis.com/k8s-triage/index.html

We expect no non-infra related flakes in the last month as a GA graduation criteria.
-->

<!-- - <test>: <link to test coverage> -->
- Test whether the updated group during the rolling conforms to the partition and verify that replicas whose index less than `lwsPartition` are always populated with the old version.

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
- 2025-05-29: KEP initialized and submitted for review
- 2025-06-04: Update the logic that determines when a rolling update is done

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