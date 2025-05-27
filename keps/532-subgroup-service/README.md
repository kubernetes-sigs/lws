# KEP-532: Subgroup Service Auto-Creation 
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
Following KEP-115, subgroups provide the capability to differentiate roles among workers within the same LeaderWorkerSet (LWS) instance. However, we still need a mechanism to access different subgroups via Kubernetes service discovery.

Currently, LeaderWorkerSet supports auto-creating headless Services per replica when `subdomainPolicy: UniquePerReplica` is set. However, users may require similar functionality at the subgroup level: specifically, auto-creating a K8S cluster service per subgroup for finer-grained network control.

For example, in a Disaggregated Prefill scenario, we can split each LeaderWorkerSet replica into smaller subgroups: some running as Prefill roles and others as Decode roles. The frontend/router can then access different subgroups via Kubernetes service discovery, requiring distinct Service names for each subgroup to differentiate between Prefill and Decode roles.

**Example Architecture:**

```
├── LeaderWorkerSet Instance
    ├── Replica 0
        ├── Leader-0
        │   └── Proxy Router
        └── Workers-0
            ├── SubGroup 0          <---- svc:  lws-group-0-subgroup-0
            │   └── Prefill pods
            └── SubGroup 1          <---- svc:  lws-group-0-subgroup-1
                └── Decode pods
    ├── Replica 1
        ├── Leader-1
        │   └── Proxy Router
        └── Workers-1
            ├── SubGroup 0          <---- svc:  lws-group-1-subgroup-0
            │   └── Prefill pods
            └── SubGroup 1          <---- svc:  lws-group-1-subgroup-1
                └── Decode pods

```

This KEP proposes to automatically create a cluster Service(NOT Headless service) for each subgroup in a LeaderWorkerSet replica,  when the `spec.leaderWorkerTemplate.subGroupPolicy.autoCreateService` flag is enabled.


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
As a user, when I have a LeaderWorkerSet with subgroups, I want Kubernetes services automatically created for each subgroup so that I can access different subgroups via Kubernetes service discovery. 

For example, in a Disaggregated Prefill scenario, I can split LWS workers in one group into smaller subgroups—some running as Prefill roles and others as Decode roles. I can then use different Kubernetes services to access Prefill and Decode roles separately.



**Example Configuration:**
LeaderWorkerSet with `replicas=2`, `size=4`, and `subgroupSize=2` results in the following pod and group structure:

| Pod Name | Group ID | Subgroup ID |
|----------|----------|-------------|
| lws-0    | 0        | 0           |
| lws-0-1  | 0        | 0           |
| lws-0-2  | 0        | 1           |
| lws-0-3  | 0        | 1           |
| lws-1    | 1        | 0           |
| lws-1-1  | 1        | 0           |
| lws-1-2  | 1        | 1           |
| lws-1-3  | 1        | 1           |

**Current LWS Implementation:**
Headless services (named `${LwsInstanceName}-${GroupID}`) are created for each group/replica:

| Service Name | Selected Group Index | Selected Pods                        |
|--------------|---------------------|--------------------------------------|
| svc-lws-0    | 0                   | lws-0, lws-0-1, lws-0-2, lws-0-3    |
| svc-lws-1    | 1                   | lws-1, lws-1-1, lws-1-2, lws-1-3    |

**Proposed Behavior:**
In addition to the existing group-level headless services, we will auto-create cluster-ip services for each subgroup, named `${LwsInstanceName}-group-${GroupID}-subgroup-${subGroupID}`:

| Service Name | Selected Group Index | Selected Subgroup Index | Selected Pods       |
|--------------|---------------------|-------------------------|---------------------|
| svc-lws-0-0  | 0                   | 0                       | lws-0, lws-0-1     |
| svc-lws-0-1  | 0                   | 1                       | lws-0-2, lws-0-3   |
| svc-lws-1-0  | 1                   | 0                       | lws-1, lws-1-1     |
| svc-lws-1-1  | 1                   | 1                       | lws-1-2, lws-1-3   |





**Additional Capability:**
Whenever if the leader is included in a subgroup(`SubGroupPolicyType` is either `LeaderWorker` or `LeaderExcluded`), the corresponding service will be created. This allows the leader to be scheduled with different resource requirements.


### Risks and Mitigations

<!--
What are the risks of this proposal, and how do we mitigate? Think broadly.
For example, consider both security and how this will impact the larger
Kubernetes ecosystem.

How will security be reviewed, and by whom?

How will UX be reviewed, and by whom?

Consider including folks who also work outside the SIG or subproject.
-->
To ensure backward compatibility and avoid affecting existing functionality, the default behavior remains unchanged. Additional services and labels/annotations will only be created when the `autoCreateService` flag is explicitly enabled.

## Design Details

<!--
This section should contain enough information that the specifics of your
change are understandable. This may include API specs (though not always
required) or even code snippets. If there's any ambiguity about HOW your
proposal will be implemented, this is the place to discuss them.
-->

### LeaderWorkerSet API

We extend the LeaderWorkerSet API to introduce the `autoCreateService` field within the existing `SubGroupPolicy`. This field enables automatic service creation for subgroups while maintaining backward compatibility. 


```go
type LeaderWorkerTemplate struct {
    // SubGroupPolicy describes the policy that will be applied when creating subgroups.
    SubGroupPolicy *SubGroupPolicy `json:"subGroupPolicy,omitempty"`
} 

type SubGroupPolicy struct {
    // ... existing fields ...
    
    // AutoCreateService determines whether subgroup services should be auto-created.
    // Defaults to false.
    // When set to true, a cluster service will be created for each subgroup.
    // Service naming convention: ${LwsInstanceName}-group-${GroupID}-subgroup-${subGroupID}
    // The service will be a cluster service with selectors:
    //   - leaderworkerset.sigs.k8s.io/subgroup-key: <subgroup-key>
    //   - leaderworkerset.sigs.k8s.io/leaderworkerset-name: <lws-name>
    // +optional
    AutoCreateService bool `json:"autoCreateService,omitempty"`
    
    // ... other fields ...
}
```

### Implementation

The implementation involves adding a new function `reconcileSubGroupServices()` to the `LeaderWorkerSetReconciler.Reconcile()` logic. This function will:

1. **Service Creation Logic**: Create cluster services for each subgroup when `autoCreateService` is enabled
2. **Service Naming**: Follow the naming convention `${LwsInstanceName}-group-${GroupID}-subgroup-${subGroupID}`
3. **Label Management**: Apply appropriate labels and selectors to ensure correct pod targeting
4. **Lifecycle Management**: Handle service creation, updates, and cleanup in alignment with subgroup lifecycle(`ownerReferences` as the group leader pod)
5. **Port**: the service port should cover the ports of Pod's ports in workerTemplate.


The service looks like :
```
apiVersion: v1
kind: Service
metadata:
  labels:
    leaderworkerset.sigs.k8s.io/name: my-lws-name
  name: my-lws-name-group-0-subgroup-1
  namespace: my-test 
  ownerReferences:
  - apiVersion: leaderworkerset.x-k8s.io/v1
    blockOwnerDeletion: true
    controller: true
    kind: LeaderWorkerSet
    name: my-lws-name
    uid: f1e0309a-aebc-4163-bcdc-fdd2d6a20527
spec:
  type: ClusterIP
  ports:
  - port: 80
    protocol: TCP
    targetPort: 80
  selector:
    leaderworkerset.sigs.k8s.io/group-index: "0"
    leaderworkerset.sigs.k8s.io/name: my-lws-name
    leaderworkerset.sigs.k8s.io/subgroup-index: "1"
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

Unit tests will be added to ensure:
- Kubernetes cluster services are correctly created for subgroup pods
- Service naming follows the specified convention
- Service selectors properly target subgroup pods
- Services are created only when the `autoCreateService` flag is enabled
- Proper cleanup occurs when LWS Set are deleted


#### Integration Tests

Integration tests will verify:
- End-to-end service creation workflow for subgroups
- Service discovery functionality across different subgroups
- Proper service lifecycle management during LeaderWorkerSet updates
- No side effect to existing replica-level headless services



#### End to End Tests

End-to-end tests will validate:
- LeaderWorkerSet deployment with subgrouping and `autoCreateService` flag creates correct services
- Services are accessible and properly route traffic to intended subgroup pods
- Service cleanup occurs when LeaderWorkerSet is deleted
- Backward compatibility with existing LeaderWorkerSet deployments without the flag


## Alternatives

<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->

### Manual Service Creation
**Approach**: Require users to manually create services for each subgroup.
**Rejected because**: This approach increases operational overhead and is error-prone, especially in dynamic environments where subgroups may change frequently.

