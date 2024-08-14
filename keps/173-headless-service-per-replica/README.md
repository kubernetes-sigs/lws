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
This KEP is about allowing one to create a headless service for all of the leaders, and a headless service per replica for the workers.

## Motivation

The number of pods per service is finite. At large scale, it is possible to exhaust this limit.
See [DNS AT Scale](https://gist.github.com/aojea/32aeaa86aacebcdd93596ecb70fcba4f) for a more indepth explanation.

With large number of replicas and size, we should allow to create a headless service for the leaders, and a headless service per replica for the workers.

### Goals

- Leaders can have a service
- Each replica can have a service for the workers 
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

```golang
type LeaderWorkerSetSpec struct {
 // SubdomainPolicy determines the policy that will be used when creating
 // the headless service
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
  // SubdomainLeadersSharedWorkersDedicated will create a headless service for each
  // leader-worker group. 
  // The leader host names will look like:
  // Replica 0: my-lws-0.my-lws
  // Replica 1: my-lws-1.my-lws
  // The worker host names will look like:
  // Replica 0: my-lws-0-1.my-lws-0, my-lws-0-2.my-lws-0
  // Replica 1: my-lws-1-1.my-lws-1, my-lws-1-2.my-lws-1
  SubdomainLeadersSharedWorkersDedicated SubdomainPolicy = "LeadersSharedWorkersDedicated"
)
```

### Implementation

With SubdomainPolicy set to SubdomainLeadersSharedWorkersDedicated, we will create a headless
service for all of the leaders, and a headless service per replica for the workers. In order 
to ensure backwards compatability, the default value if non is set will be Shared.

A label will be added to determine whether a pod is a leader or a worker.
`leaderworkerset.sigs.k8s.io/role`


The creation and update logic is the following:
```golang
// Existing creation logic
if lws.Spec.SubdomainPolicy == leaderworkerset.SubdomainShared {
  createHeadlessServiceIfNotExists(
    name=lws.Name, 
    selector= {
      leaderworkerset.SetNameLabelKey: lws.Name
      }
    )
  for i := 0; i < int(*lws.Spec.Replicas); i++ {
    // If transitioning from LeadersSharedWorkersDedicated to shared, need to delete 
    // the worker headless services that were created
    r.deleteHeadlessServiceIfExists(
      name=fmt.Sprintf("%s-%s", lws.Name, strconv.Itoa(i))
      )
	}
  return
}
// Create the headless service for the leaders
createHeadlessServiceIfNotExists(
  name=lws.Name, 
  selector={
    leaderworkerset.PodRoleLabelKey: "leader"
    })

// Create the headless service for the workers
for i := 0; i < int(*lws.Spec.Replicas); i++ {
  createHeadlessServiceIfNotExists(
    name=fmt.Sprintf("%s-%s", lws.Name, strconv.Itoa(i)), 
    selector={
      leaderworkerset.PodRoleLabelKey: "worker", 
      leaderworkerset.GroupIndexLabelKey: strconv.Itoa(i)
      }
    )
}
```

The headless service created for the leaders will have the same name as the 
headless service that is created when subdomainPolicy is set to `Shared`. This 
guarantees that the `LWS_LEADER_ADDRESS` environment variable does not change.


When transitioning between the two policies, the leader headless service won't be
deleted, but instead will be updated with a new selector.

```golang
func createHeadlessServiceIfNotExists(serviceName string, serviceSelector map[string]string) {
  var headlessService corev1.Service
  if err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &headlessService); err != nil {
    // creation logic
    return
  }

  // if the selectors are different, it means that the subdomainPolicy has changed, so 
  // need to update the headlessService with the new selector.
  if eq := reflectDeepEqual(headlessService.Spec.Selector, serviceSelector); !eq {
    headlessService.Spec.Selector = serviceSelector
    r.Update(&headlessService)
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

- When field is set to Shared, verify that there is only one headless service

- When field is set to LeadersSharedWorkersDedicated, verify that the number of headless services is lws.Spec.Replicas + 1

- When updating the field, verify that the correct number of headless
services exist after update
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

Instead of doing a headless service exclusively for the leaders, and a headless service per replica for the workers, we 
considered having a headless service per replica. Having the leader and its respective workers use the same headless 
service made more sense at first, but it would require having to update the environment variable `LWS_LEADER_ADDRESS`, 
significantly complicating the process of updating the field.