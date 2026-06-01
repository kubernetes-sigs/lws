# KEP-173: Headless Service Per Replica

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
  - [Notes/Constraints/Caveats (Optional)](#notesconstraintscaveats-optional)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
  - [Implementation](#implementation)
    - [Creation and Deletion of Headless Services](#creation-and-deletion-of-headless-services)
    - [Transition from Different Subdomain Policies](#transition-from-different-subdomain-policies)
    - [Environment Variable Injection and Rolling Update](#environment-variable-injection-and-rolling-update)
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

LeaderWorkerSet creates a single headless service for the entire object.
This KEP is about allowing one to create a headless service for every replica of LWS.

## Motivation

The number of pods per service is finite. At large scale, it is possible to exhaust this limit.
See [DNS AT Scale](https://gist.github.com/aojea/32aeaa86aacebcdd93596ecb70fcba4f) for a more indepth explanation.

With large number of replicas and size, we should allow to create a headless service
for every replica of LWS.

### Goals

- Each replica gets its own headless service
<!--
List the specific goals of the KEP. What is it trying to achieve? How will we
know that this has succeeded?
-->

### Non-Goals

- Providing an API to configure service per replicas.
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

### API

We extend the LeaderWorkerSet API to introduce a new field: `NetworkConfig`. This field will have a subfield called `subDomainPolicy`. If set to `Shared`, LWS will create a single headless service for all replicas to share. If set to `UniquePerReplica`, it will create a headless service per replica. `subDomainPolicy` is a mutable field, and will trigger a rolling update when changed. 

In order to ensure backwards compatibility, the default value will be `Shared`.

```golang
type LeaderWorkerSetSpec struct {
	// NetworkConfig defines the network configuration of the group
	// +optional
 NetworkConfig *NetworkConfig`json:"networkConfig,omitempty"`
}

type NetworkConfig struct {
  SubdomainPolicy *SubdomainPolicy `json:"subdomainPolicy,omitempty"`
}
type SubdomainPolicy string
const (
	// SubdomainShared will create a single headless service that all replicas
	// will share. The host names look like:
	// Replica 0: my-lws-0.my-lws, my-lws-0-1.my-lws
	// Replica 1: my-lws-1.my-lws, my-lws-1-1.my-lws
	SubdomainShared SubdomainPolicy = "Shared"
	// UniquePerReplica will create a headless service per replica
	// The pod host names look like:
	// Replica 0: my-lws-0.my-lws-0,my-lws-0-1.my-lws-0, my-lws-0-2.my-lws-0
	// Replica 1: my-lws-1.my-lws-1,my-lws-1-1.my-lws-1, my-lws-1-2.my-lws-1
	SubdomainUniquePerReplica SubdomainPolicy = "UniquePerReplica"
)
```

### Implementation
The existing logic for creating the headless service will be used for the `Shared` option. 

#### Creation and Deletion of Headless Services
The pod controller will create the headless service if a leader pod is being reconciled. The leader pod is set as the owner of the service, so that when a leaderPod is restarted or deleted, the headless service is deleted as well. The name of each headless service will be the same as the leader pod's name. In order for the headless service only select the pods in each replica, `leaderworkerset.sigs.k8s.io/group-index` will be used as selector to differentiate between LWS replicas, and `leaderworkerset.sigs.k8s.io/name` to differentiate between other LWS deployments.


#### Transition from Different Subdomain Policies
When transitioning from `Shared` to `UniquePerReplica`, there will be one more headless service than number of replicas. This is because the shared headless service will not be deleted, allowing for a safer transition.

When transitioning from `UniquePerReplica` to `Shared`, the shared headless service is created as soon as the transition starts. Once each replica is updated to `Shared`, their respective headless service will be deleted. 


#### Environment Variable Injection and Rolling Update
LWS injects two environment variables that depend on the subdomain's (headless service's) name: 
* `LWS_LEADER_ADDRESS` 
* `TPU_WORKER_HOSTNAMES`. 


The subdomain's name can be set in two different places:
* When creating the StatefulSet
* Directly setting it at the pod level


Because the headless service's name depends on the name of the leader pod, the subdomain field cannot be set when creating the leader StatefulSet. Instead, it will be injected by the pod webhook. 

A new label `leaderworkerset.sigs.k8s.io/subdomainPolicy` will be set on the leader pods to determine whether or not to overwrite the subdomain field. 

In contrast, the subdomain field will be set when creating the worker StatefulSet, since it is created by the pod controller. 

When transitioning between subdomain policies, the containers must be restarted in order for the values of the environment variables to be updated. Thus, a change in subdomain policy will trigger a rolling update.


### Test Plan

<!--
**Note:** *Not required until targeted at a release.*
The goal is to ensure that we don't accept enhancements with inadequate testing.

All code is expected to have adequate tests (eventually with coverage
expectations). Please adhere to the [Kubernetes testing guidelines][testing-guidelines]
when drafting this test plan.

[testing-guidelines]: https://git.k8s.io/community/contributors/devel/sig-testing/testing.md
-->
##### Prerequisite testing updates

<!--
Based on reviewers feedback describe what additional tests need to be added prior
implementing this enhancement to ensure the enhancements have also solid foundations.
-->
None.

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

- Validate that the number of headless services matches the number of replicas when `subdomainPolicy` is set to `UniquePerReplica`

- Expected value of environment variables are injected on both policies

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

- Test that the environment variables are updated when transitioning between subdomain policies

- Test that the number of headless services scales up during MaxSurge


### Graduation Criteria

N/A.

I don't think this needs stages for graduation. 
It can go directly to stable for this project.
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

KEP drafted: August 6th, 2024

KEP updated: October 17th, 2024
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
The original implementation was to create a headless service per statefulset instead. This meant that we didn't have to update the environment variable `LWS_LEADER_ADDRESS`, as the leaders would have a headless service with name `lws.Name`. However, `TPU_WORKER_HOSTNAMES` would still need to be updated, so there was no clear advantage of using `LeadersSharedWorkersDedicated`. We opted to do `UniquePerReplica` because it is the more intuitive one. 

<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->

