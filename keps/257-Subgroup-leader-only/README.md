# KEP-257: SubGroup Leader Only

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
  - [Notes/Constraints/Caveats (Optional)](#notesconstraintscaveats-optional)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Subgroup Creation](#subgroup-creation)
  - [TPU Environment Injection](#tpu-environment-injection)
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
avoid requiring implementers to split their attention between writing release
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
This KEP aims to extend the SubGroup feature by adding an API field that defines the type of Subgroup that will be created. The existing version
will become "LeaderWorker", and a new type which will exclude the leader from any subgroup.


## Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this KEP.  Describe why the change is important and the benefits to users. The
motivation section can optionally provide links to [experience reports] to
demonstrate the interest in a KEP within the wider Kubernetes community.

[experience reports]: https://github.com/golang/go/wiki/ExperienceReports
-->

There are cases where the resource requirements for the leader are different from the ones by the worker, while still being able to guarantee exclusive placement
on the workers. Having a `LeaderExcluded` subgroup means that the placement of the leader will be independent of the workers, and by adding the `subgroup-exclusive-topology`, the 
workers can be guaranteed to be scheduled on the same topology.

### Goals

<!--
List the specific goals of the KEP. What is it trying to achieve? How will we
know that this has succeeded?
-->

Add an option exclude the leader from any subgroup, while still being able to create subgroups on the other workers. Therefore, it should be possible to do the following split:
leader, (worker-1, worker-2), (worker-3, worker-4). If the desired effect is to have just one subgroup for the workers, subgroupSize should be set 
to the number of workers (so size - 1). 

### Non-Goals
This KEP assumes that the leader will not request accelerator resources, and thus, subGroupSize will always be assumed to be an odd number.

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

### User Stories (Optional)

<!--
Detail the things that people will be able to do if this KEP is implemented.
Include as much detail as possible so that people can understand the "how" of
the system. The goal here is to make this feel real for users without getting
bogged down.
-->

#### Story 1
As a user, I should be able to create a subGroup that only includes the leader in order to be able to schedule the leader with different resource requirements
still keeping exclusive topology on the workers.

### Notes/Constraints/Caveats (Optional)
As mentioned in Non-Goals, this KEP assumes that the leader does not request accelerator resources.

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

A new API field will be added under the SubGroupPolicy type. This API field will determine what type 
of subgroup is desired. The default is LeaderWorker, which is the existing implementation of subgroup. `SubGroupSize`
will continue to be a required field, while `Type` will be optional. 

```golang
type SubGroupPolicy struct {

  // +kubebuilder:validation:Enum={LeaderWorker,LeaderExcluded}
  // +kubebuilder:default=LeaderWorker
  Type SubGroupPolicyType `json:"subGroupPolicyType,omitempty"`

  SubGroupSize *int32 `json:"subGroupSize,omitempty"`
}

type SubGroupPolicyType string

const (
	SubGroupPolicyLeaderWorker SubGroupPolicyType = "LeaderWorker"

	SubGroupPolicyLeaderExcluded SubGroupPolicyType = "LeaderExcluded"
)
```

A new annotation will be created to determine whether or not the subgroup type is `LeaderExcluded`

* `leaderworkerset.sigs.k8s.io/subgroup-policy-type`

This annotation will only be injected in the leader pod.

In order to keep backwards compatibility, it will only be added if the type is `LeaderExcluded`.

### Subgroup Creation 
Implementation wise, the only change needed is to not add the SubGroup labels on the leader if the SubGroupType is `LeaderExcluded`. Effectively, this means 
that the leader is not part of a subgroup at all. The only point of a pod being in a subgroup is to guarantee exclusive placement with the other pods 
in the subgroup. However, since this is a one pod subgroup, there is no use case for injecting the subgroup labels on the leader. 


```golang
func (p *PodWebhook) Default() {
  if foundSubGroupSize && pod.Labels[leaderworkerset.SubGroupIndexLabelKey] == "" && !leaderOnlySubGroup {
    pod.Labels[leaderworkerset.SubGroupIndexLabelKey] = "0"
    subGroupUniqueKey := genGroupUniqueKey(pod.Name, "0")
    pod.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] = subGroupUniqueKey
    if subEpKey, foundSubEpKey := pod.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]; foundSubEpKey {
      SetExclusiveAffinities(pod, subGroupUniqueKey, subEpKey, leaderworkerset.SubGroupUniqueHashLabelKey)
    }
  }
}
```

### TPU Environment Injection
When injecting TPU environment variables, we will keep the same logic that exists for when the leader doesn't request TPU resources.

### Test Plan

<!--
**Note:** *Not required until targeted at a release.*
The goal is to ensure that we don't accept enhancements with inadequate testing.

All code is expected to have adequate tests (eventually with coverage
expectations). Please adhere to the [Kubernetes testing guidelines][testing-guidelines]
when drafting this test plan.

[testing-guidelines]: https://git.k8s.io/community/contributors/devel/sig-testing/testing.md
-->

[X] I/we understand the owners of the involved components may require updates to
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

- <test>: <link to test coverage>

##### e2e tests

<!--
This question should be filled when targeting a release.
For Alpha, describe what tests will be added to ensure proper quality of the enhancement.

For Beta and GA, add links to added tests together with links to k8s-triage for those tests:
https://storage.googleapis.com/k8s-triage/index.html

We expect no non-infra related flakes in the last month as a GA graduation criteria.
-->

- <test>: <link to test coverage>

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

## Drawbacks

<!--
Why should this KEP _not_ be implemented?
-->

## Alternatives
Original implementation was to have the leader have the subGroupIndex be 0, and shift all other subGroup indices are moved one to the right.

```golang
if leaderOnly {
    return fmt.Sprint(((workerIndex - 1) / subGroupSize) + 1)
}
```

This implementation also requires to shift the subGroupIndex back to the left when injecting TPU environment variables 

```golang
// Take worker 1 as an example with subGroupSize set to 2. It will now have subGroupIndex set to 1, which means that the start will be 3, and end will be 4. 
// Need to shift it down by 1 for the calculation to still work out. 
if leaderOnlyType && subGroupIndex != 0 {
    subGroupIndex -= 1
}
start := subGroupSize*subGroupIndex + 1 
end := subGroupSize * (subGroupIndex + 1)
```

Since there is no use case for having the leaderPod on its own subgroup, this implementation was scratched as it adds unnecessary complexity


<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->