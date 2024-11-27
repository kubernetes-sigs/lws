# KEP-238: Controller-Revision

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
  - [LWS controller](#lws-controller)
  - [Pod Controller](#pod-controller)
  - [Controller Revision Implementation](#controller-revision-implementation)
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

This KEP aims to add controller revision to store previous states of the LWS object.
It fixes bug #238, and opens the road for future rollback support.

## Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this KEP.  Describe why the change is important and the benefits to users. The
motivation section can optionally provide links to [experience reports] to
demonstrate the interest in a KEP within the wider Kubernetes community.

[experience reports]: https://github.com/golang/go/wiki/ExperienceReports
-->
If a replica is restarted during rolling update, and the replica hasn't been updated
yet, the worker pod spec that is used is the updated one, while the leader pod spec 
is the original one. We can fix this by storing the worker pod spec using controller revision.

### Goals

<!--
List the specific goals of the KEP. What is it trying to achieve? How will we
know that this has succeeded?
-->

Stores the state of the LWS object in order to use the correct version when recreating a replica.

### Non-Goals

<!--
What is out of scope for this KEP? Listing non-goals helps to focus discussion
and make progress.
-->

This KEP does not add rollback support, though the design takes into account that it will be 
added in the future.

## Proposal
We propose adding controller revision to the LWS controller. This allows us to store previous
iterations of the LWS object, in order to make it possible to recreate pods with the right pod 
spec when restarted during rolling update. This requires adding three new fields to the lws status:
`lws.status.CurrentRevision` `lws.Status.UpdateRevision` and `lws.Status.CollisionCount`.

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

#### Story 2

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
The following LWS API fields need to be added to support controller revision.

```golang
// LeaderWorkerSetStatus defines the observed state of LeaderWorkerSet
type LeaderWorkerSetStatus struct {
	// currentRevision, if not empty, indicates the version of lws
	// used to generate the worker pods in sequence [0,currentReplicas)
	CurrentRevision string `json:"currentRevision,omitempty"`

	// updateRevision, if not empty, indicates the version of lws
	// used to generate the worker pods in sequence 
  // [replicas-updatedReplicas,replicas)
	UpdateRevision string `json:"updateRevision,omitempty"`

	// collisionCount is the count of hash collisions for the controller 
	// revision uses this field as a collision avoidance mechanism 
	// when it needs to create the name for the newest ControllerRevision.   
	// +optional
	CollisionCount *int32 `json:"collisionCount,omitempty"`
}
```

### LWS controller

The status of the revisions will be updated before rolling update starts.

```golang
func Reconcile() {
	currentRevision, updateRevision, collisionCount, err := GetLeaderWorkerSetRevisions(ctx, r.Client, lws)
	if err != nil {
		log.Error(err, "Getting StatefulSet revisions")
		return ctrl.Result{}, err
	}
	lws.Status.CurrentRevision = currentRevision.Name
	lws.Status.UpdateRevision = updateRevision.Name
	lws.Status.CollisionCount = new(int32)
	lws.Status.CollisionCount = &collisionCount

	partition, replicas, err := r.rollingUpdateParameters(ctx, lws)
}
```

Once the update has been determined to be done, `currentRevision` will be set to be the value of `updateRevision`

```golang
func updateConditions() {
	if updatedAndReadyCount == int(*lws.Spec.Replicas) {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetAvailable))
		lws.Status.CurrentRevision = lws.Status.UpdateRevision
	}
}
```

### Pod Controller

In order to determine what worker pod spec to use to create the worker pods, we can compare 
the value of the template hash generated by the LWS object, with the template hash that the leader pod 
hash. Because the leader pod spec is determined by the statefulset controller, there is a guarantee that 
it will always have the right pod spec. So, if the template hashes don't match, it means that the leader pod was created 
using the old pod spec, meaning the old worker pod spec needs to be used.

```golang

func Reconcile() {
	currentRevision, _, _, err := GetLeaderWorkerSetRevisions(ctx, r.Client, lws)
	constructWorkerStatefulSetApplyConfiguration(currentRevision)
}

func constructWorkerStatefulSetApplyConfiguration(currentRevision) {
	updatedTemplateHash := LeaderWorkerTemplateHash(&lws)
	podTemplateSpec := *WorkerTemplate.DeepCopy()
	if updatedTemplateHash != leaderPod.Labels[templateHash] {
		originalLws, err := ApplyRevision(&lws, currentRevision)
		if err != nil {
			return nil, err
		}
		podTemplateSpec = *originalLws.WorkerTemplate.DeepCopy()
	}
}
```

### Controller Revision Implementation
A new package will be created for the functions that interact with controllerRevision, similar to how it is done in [upstream](https://github.com/kubernetes/kubernetes/blob/cb31e42b85d0a1e2be6639ecd4f7b9414887552a/pkg/controller/history/controller_history.go).

Functions for creating patches, applying revisions, and truncating history will be added to `controller_utils` since both the pod and lws controllers need to access these functions.

For now, the history will only contain the current and updatedRevisions. Once rollback is implemented, this can be updated.

Only the `LeaderWorkerTemplate` will be included in the controller revision patch. This can be modified later while still being backwards compatible.

```golang
func createPatch() {
	template := spec["leaderWorkerTemplate"].(map[string]interface{})
	specCopy["leaderWorkerTemplate"] = template
	template["$patch"] = "replace"
	objCopy["spec"] = specCopy
	patch, err := json.Marshal(objCopy)
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
- Test controller revision implemenation functions
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

Test that currentRevision, updatedRevision, and collisionCount work as intended.

Test that worker pod is recreated with the correct pod template if restarted during rolling update.

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


##### e2e tests

<!--
This question should be filled when targeting a release.
For Alpha, describe what tests will be added to ensure proper quality of the enhancement.

For Beta and GA, add links to added tests together with links to k8s-triage for those tests:
https://storage.googleapis.com/k8s-triage/index.html

We expect no non-infra related flakes in the last month as a GA graduation criteria.
-->


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
An implementation similar to the one done for StatefulSet was considered. In this implementation, we would create
a label to store what controller revision was used to generate the pods, and replacing the label `leaderworkerset.sigs.k8s.io/template-revision-hash`.
However, this requires updating the value of those labels (or adding a new label alltogether). Doing this, triggers a rolling update when upgrading 
from an LWS version that does not have controller revision. 

Moreover, there are features that need to trigger a rolling update (e.g NetworkConfig). In order to replicate this behavior when using 
a label to store controller revision, NetworkConfig must be part of the controller revision patch. However, this would mean that NetworkConfig would 
also be rolled back.

<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->