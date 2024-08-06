# KEP-173: Headless Service Per Group

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
- [KEP-173: Headless Service Per Group](#kep-173-headless-service-per-group)
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
    - [API](#api)
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

LeaderWorkerSet creates a single headless service for the entire object.
This KEP is about allowing one to create a headless service per each replica.

## Motivation

The number of pods per service is finite. At large scale, it is possible to exhaust this limit.
See [DNS AT Scale](https://gist.github.com/aojea/32aeaa86aacebcdd93596ecb70fcba4f) for a more indepth explanation.

With large number of replicas and size, we should allow one to create a service per replica.

### Goals

- Each group of leader-worker replicas can have a dedicated service
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

#### Story 1

As a user running a large scale LWS job, I want to have each leader-worker group with its own service.

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

```golang
type LeaderWorkerSetSpec struct {
 // SubdomainPolicy determines the policy that will be used when creating
 // the headless service
 SubdomainPolicy SubdomainPolicy `json:"subdomainPolicy,omitempty"`
}

type NetworkConfig struct {
  SubdomainPolicy SubdomainPolicy
}
type SubdomainPolicy string
const (
  // SubdomainShared will create a single headless service that all replicas 
  // will share
  SubdomainShared SubdomainPolicy = "Shared"
  // SubdomainUniquePerReplica will create a headless service for each
  // leader-worker group
  SubdomainUniquePerReplica SubdomainPolicy = "UniquePerReplica"
)
```

### Implementation

With HeadlessServicePerReplica true, we will create a headless service per replica.
The name of the service will be `{lws.Name}-{replicaIndex}`.
Creation of the services will be in a loop of replicas. The selector of each

With SubdomainPolicy set to SubdomainUniquePerReplica, we will create a headless service
per replica. The name of the service will be `{lws.Name}-{replicaIndex}`. The selector of
the headless service will be the group index.

```golang
for i := 0; i < lws.Spec.Replicas; i++ {
  headlessService := corev1.Service{
    ObjectMeta: metav1.ObjectMeta{
      Name:      fmt.Sprintf("%s-%s", lws.Name, i),
      Namespace: lws.Namespace,
    },
    Spec: corev1.ServiceSpec{
      ClusterIP: "None", // defines service as headless
      Selector: map[string]string{
        leaderworkerset.GroupIndexLabelKey: i,
      },
      PublishNotReadyAddresses: true,
    },
  }
}
```

Currently, LWS will add an environment variable to all pods that
points to the dns record.

The pods get the following environment variable:

```golang
 leaderAddressEnvVar := corev1.EnvVar{
  Name:  leaderworkerset.LwsLeaderAddress,
  Value: fmt.Sprintf("%s-%s.%s.%s", lwsName, groupIndex, lwsName, pod.ObjectMeta.Namespace),
 }
```

With this feature enabled, we will need to set the headless service of the replica the pod belongs to.
```golang
 leaderAddressEnvVar := corev1.EnvVar{
  Name:  leaderworkerset.LwsLeaderAddress,
  Value: fmt.Sprintf("%s-%s.%s-%s.%s", lwsName, groupIndex, lwsName, groupIndex, pod.ObjectMeta.Namespace),
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

When the field is true, we will verify that there exists a headless service per replica.

When the field is false, we will verify that only one headless service exists.

##### e2e tests

<!--
This question should be filled when targeting a release.
For Alpha, describe what tests will be added to ensure proper quality of the enhancement.

For Beta and GA, add links to added tests together with links to k8s-triage for those tests:
https://storage.googleapis.com/k8s-triage/index.html

We expect no non-infra related flakes in the last month as a GA graduation criteria.
-->

None. This feature is not complicated enough to warrant a e2e test.
Integration tests will confirm that the service is created and that should be sufficient.

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