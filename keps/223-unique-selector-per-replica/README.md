# KEP-223: Unique Node Selector and Toleration Per Replica

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
- [Design Details](#design-details)
  - [Implementation](#implementation)
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
This KEP aims to add two new API fields which allow the addition of a unique nodeSelector and toleration per replica. This allows
for Cluster Autoscaler to create a new node group per replica.

## Motivation
When running inference workloads across nodes, it can be advantageous to schedule an LWS replica across the same node group. For instance,
having the nodes be scheduled on the same rack can improve networking performance between the nodes. By adding a unique node selector and 
toleration per replica, we can force cluster autoscaler to create nodes on the same group to accomodate a new LWS replica.

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this KEP.  Describe why the change is important and the benefits to users. The
motivation section can optionally provide links to [experience reports] to
demonstrate the interest in a KEP within the wider Kubernetes community.

[experience reports]: https://github.com/golang/go/wiki/ExperienceReports
-->

### Goals
<!--
List the specific goals of the KEP. What is it trying to achieve? How will we
know that this has succeeded?
-->
Allow the user to define the label that will be targeted by the node selector and toleration for cluster autoscaler to create a new node group with 
said labels.

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

### User Stories (Optional)

<!--
Detail the things that people will be able to do if this KEP is implemented.
Include as much detail as possible so that people can understand the "how" of
the system. The goal here is to make this feel real for users without getting
bogged down.
-->

#### Story 1
As a user, I would like to be able to define the label that cluster autoscaler will use to create the new node group that will host an LWS replica
in order to ensure that I can maximize performance on inference workloads, creating a 1:1 mapping between the node group and replica.


<!--
What are the caveats to the proposal?
What are some important details that didn't come across above?
Go in to as much detail as necessary here.
This might be a good place to talk about core concepts and how they relate.
-->
<!--
What are the risks of this proposal, and how do we mitigate? Think broadly.
For example, consider both security and how this will impact the larger
Kubernetes ecosystem.

How will security be reviewed, and by whom?

How will UX be reviewed, and by whom?

Consider including folks who also work outside the SIG or subproject.
-->

## Design Details

We propose adding two new API fields to LWS:

```golang

type type LeaderWorkerTemplate struct {
  // Defines additional scheduling constraints for the group
  SchedulingPolicy *SchedulingPolicy `json:"schedulingPolicy,omitempty"`
}

type SchedulingPolicy struct {
    // Defines the label that will be targeted by the node selector
    // The key will be the name of the leader pod
	ReplicaUniqueNodeSelector string `json:"replicaUniqueNodeSelector,omitempty"`

    // Defines the toleration configuration that will be unique per replica
	ReplicaUniqueToleration *ReplicaUniqueToleration `json:"replicaUniqueToleration,omitempty"`
}

type ReplicaUniqueToleration struct {
    // Key that will be set in the toleration
	Key string `json:"key"`

	// +kubebuilder:validation:Enum={NoSchedule,PreferNoSchedule,NoExecute}
	Effect corev1.TaintEffect `json:"effect"`
}
```

### Implementation

When using exclusive placement, the workers are not created until the leader has been scheduled. This causes an issue 
if the worker pods have the unique node selector but the leader does not, and the same nodeGroup is used for exclusive placement. 
Suppose we have an LWS replica of size 2, and a nodeGroup of size 3. Then LWS scales up to two replicas. 
The leader will be scheduled on the existing nodeGroup, however, the workers won't be able to be scheduled on it because of the unique node selector.
Therefore, to ensure compatability with the exclusive placement feature, both the leader and worker pods will have the unique node selector and toleration.

We can only add the node selector and toleration after the leader pod has been created, so injecting them will be handled by the pod webhook. The pod webhook does
not have access to the LWS object, so three new annotations, corresponding to the new configurable fields, will be added:

* `leaderworkerset.sigs.k8s.io/replicaUniqueNodeSelector` 
* `leaderworkerset.sigs.k8s.io/replicaUniqueTolerationKey`
* `leaderworkerset.sigs.k8s.io/replicaUniqueTolerationEffect`


Then in the pod webhook:

```golang
func Default() {
    if pod.Annotations[leaderworkerset.ReplicaUniqueNodeSelectorAnnotationKey] != "" {
        if pod.Spec.NodeSelector == nil {
            pod.Spec.NodeSelector = make(map[string]string)
        }
        pod.Spec.NodeSelector[pod.Annotations[leaderworkerset.ReplicaUniqueNodeSelectorAnnotationKey]] = leaderName
    }
    if pod.Annotations[leaderworkerset.ReplicaUniqueTolerationKeyAnnotationKey] != "" {
                pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{
                    Key:      pod.Annotations[leaderworkerset.ReplicaUniqueTolerationKeyAnnotationKey],
                    Operator: corev1.TolerationOpEqual,
                    Value:    leaderName,
                    Effect:   getEffect(pod.Annotations[leaderworkerset.ReplicaUniqueTolerationEffectAnnotationKey]),
            })
    }
}
```
 
<!--
This section should contain enough information that the specifics of your
change are understandable. This may include API specs (though not always
required) or even code snippets. If there's any ambiguity about HOW your
proposal will be implemented, this is the place to discuss them.
-->

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

- Validate on unit tests for pod controller and lws controller that the right annotations are injected

- Validate that the right node Selector and tolerations are injected by their respective functions in the pod webhook

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

- Validate that using a non-supported effect is not allowed

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

<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->