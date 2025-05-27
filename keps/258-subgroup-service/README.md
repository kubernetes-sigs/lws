# KEP-258 Subgroup Service Auto-Creation 
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
- [Motivation](#motivation)
- [Proposal](#proposal)
  - [User Stories (Optional)](#user-stories-optional)
    - [Story 1](#story-1)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [LeaderWorkerSet API](#leaderworkerset-api)
  - [Implementation](#implementation)
  - [Test Plan](#test-plan)
    - [Unit Tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [End to End Tests](#end-to-end-tests)
- [Alternatives](#alternatives)
  - [Only Set Pod-Affinity on workers with <code>(workerIndex) % subGroupSize == 0</code>](#only-set-pod-affinity-on-workers-with-)
<!-- /toc -->

## Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this KEP.  Describe why the change is important and the benefits to users. The
motivation section can optionally provide links to [experience reports] to
demonstrate the interest in a KEP within the wider Kubernetes community.

[experience reports]: https://github.com/golang/go/wiki/ExperienceReports
-->
Follow up the KEP 115, subGroup provides capability of differenate roles among workers in the same LWS instance.
But we still need to have a way to access the different subgroups via kubernetes service discovery.
Currently, LeaderWorkerSet supports auto-creating headless Services per replica when subdomainPolicy: UniquePerReplica is set. However, users may need similar functionality at the subgroup level, i.e., to auto-create a Service per subgroup for finer-grained network control.
For example, for Disaggregated Prefill senario, we can split each LeaderWorkerSet replica into smaller subgroups, some of them are running as Prefill role, and the other are running as Decode role. Then the fronted/router can access the different subgroups via kubernetes service discovery, so we need distintic Service names for each subgroup, to tell between Prefill role and Decode role.

As below example:

|- LeaderWorkerSet Instance
   |- Leader
      |- Proxy Router
   |- Worker
      |- SubGroup 0
        |- Prefill pods
      |- SubGroup 1
        |- Decode pods


This KEP is to automiatically create a headless Service for each subgroup in LeaderWorkerSet, when a flag(`spec.leaderWorkerTemplate.subGroupPolicy.autoCreateService`) is set.


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
As a user, when I have a LeaderWorkerSet with subGroups, I want the kubernetes services in place with the subGroups created. So that I can access the different subgroups via kubernetes service discovery. For example in Disaggregated Prefill scenario, we can split LWS workers in one group into smaller subgroups, some of them are running as Prefill role, and the other are running as Decode role. The I can use different K8S services to access Prefill role and Decode role separately.



For example, if I have a LeaderWorkerSet with replicas=2, and size=4, subgroupSize set to 2, then there will be pods and groups as below:

Pod Name | Group ID | subGroup ID|
-- | -- | -- 
lws-0    | 0 | 0 |
lws-0-1 | 0 | 0 |
lws-0-2 | 0 | 1 |
lws-0-3 | 0 | 1 |
lws-1    | 1 | 0 |
lws-1-1 | 1 | 0 |
lws-1-2 | 1 | 1 |
lws-1-3 | 1 | 1 |

With current LWS implementation, the headless services(named ${LwsInstanceName}-${GroupID} ) will be created for each group/replica as below:


Service Name | Selected group Index | Selected Pods
-- | -- | --
svc-lws-0 | 0 | lws-0, lws-0-1, lws-0-2,lws-0-3
svc-lws-1 | 1 | lws-1, lws-1-1, lws-1-2, lws-1-3




The Proposed Behavior: beside the two headless services for each group/replica, we can also auto create headless services for each subgroup, named ${LwsInstanceName}-${GroupID}-${subGroupID}.

Service Name | Selected group Index | Selects subGroup Index| Selected Pods
-- | -- | -- | --
svc-lws-0-0 |  0 | 0|  lws-0, lws-0-1
svc-lws-0-1 |  0 | 1 |  lws-0-2, lws-0-3
svc-lws-1-0 |  1 | 0|  lws-1, lws-1-1
svc-lws-1-1 |  1 | 1 |  lws-1-2, lws-1-3





+I should be able to create a subGroup that only includes the leader in order to 
be able to schedule the leader with different resource requirements

### Risks and Mitigations

<!--
What are the risks of this proposal, and how do we mitigate? Think broadly.
For example, consider both security and how this will impact the larger
Kubernetes ecosystem.

How will security be reviewed, and by whom?

How will UX be reviewed, and by whom?

Consider including folks who also work outside the SIG or subproject.
-->
To avoid the changes from affecting initial functionality, the default behavior 
is to not create addtional services or adding any new labels/annotations, if the desired flag is not set.

## Design Details

The overall goal of subgroup scheduling

<!--
This section should contain enough information that the specifics of your
change are understandable. This may include API specs (though not always
required) or even code snippets. If there's any ambiguity about HOW your
proposal will be implemented, this is the place to discuss them.
-->
### LeaderWorkerSet API
We extend the LeaderWorkerSet API to introduce a new field: subGroupSize to opt in and set the number of pods that each subgroup will contain. Current behavior is kept if not set. 

```
type LeaderWorkerTemplate struct {

  // SubGroupPolicy describes the policy that will be applied when creating subgroups.
	SubGroupPolicy *SubGroupPolicy `json:"subGroupPolicy,omitempty"`
} 

type SubGroupPolicy struct {
   //.....
   // The flag to ideintify if the subgroup service should be auto-created.
   // Defaults to false.
   // If set to true, the service will be created for each subgroup.
   // The service name will be ${LwsInstanceName}-${GroupID}-${subGroupID}
   // The service will be a headless service with selector:
   //  leaderworkerset.sigs.k8s.io/subgroup-key: <subgroup-key> and leaderworkerset.sigs.k8s.io/leaderworkerset-name: <lws-name>
   AutoCreateService bool `json:"autoCreateService,omitempty"`	
   //.....
}
```

### Implementation
Add  a new function `reconcileSubGroupServices()` into `LeaderWorkerSetReconciler.Reconcile()` logic.




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


#### Unit Tests

<!--
In principle every added code should have complete unit test coverage, so providing
the exact set of tests will not bring additional value.
However, if complete unit test coverage is not possible, explain the reason of it
together with explanation why this is acceptable.
-->

<!--
Additionally, try to enumerate the core package you will be touching
to implement this enhancement and provide the current unit coverage for those
in the form of:
- <package>: <date> - <current test coverage>

This can inform certain test coverage improvements that we want to do before
extending the production code to implement this enhancement.
-->

Unit testes will be added to ensure the K8S services created to expose subGroup pods.


#### Integration tests

<!--
Describe what tests will be added to ensure proper quality of the enhancement.

After the implementation PR is merged, add the names of the tests here.
-->



#### End to End Tests

- Test that LWS deployment with subgrouping and autoCreateService flag set, will have correct services in places.


## Alternatives

<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->

