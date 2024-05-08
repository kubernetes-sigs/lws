# KEP #115 Subgroup Support
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
- [KEP #115 Subgroup Support](#kep-115-subgroup-support)
  - [Motivation](#motivation)
  - [Proposal](#proposal)
    - [User Stories (Optional)](#user-stories-optional)
      - [Story 1](#story-1)
    - [Risks and Mitigations](#risks-and-mitigations)
  - [Design Details](#design-details)
    - [LeaderWorkerSet API](#leaderworkerset-api)
    - [Exclusive Topology Support](#exclusive-topology-support)
    - [Implementation](#implementation)
    - [TPU Environment Variable Injection](#tpu-environment-variable-injection)
    - [Test Plan](#test-plan)
      - [Unit Tests](#unit-tests)
      - [Integration tests](#integration-tests)
      - [End to End Tests](#end-to-end-tests)
  - [Alternatives](#alternatives)
    - [Only Set Pod-Affinity on workers with `subgroup-worker-index=0`](#only-set-pod-affinity-on-workers-with-subgroup-worker-index0)
<!-- /toc -->

## Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this KEP.  Describe why the change is important and the benefits to users. The
motivation section can optionally provide links to [experience reports] to
demonstrate the interest in a KEP within the wider Kubernetes community.

[experience reports]: https://github.com/golang/go/wiki/ExperienceReports
-->

Disaggregated serving is an optimization made for LLM inference workloads. It takes advantage of the fact that the two phases of inference have different characteristics and thus it can be beneficial to run them on different machines. State of the art LLM serving frameworks such as [vLLM](https://github.com/vllm-project/vllm/issues/2472) are already adding support for this optimization based on the paper released by [Microsoft](https://www.microsoft.com/en-us/research/publication/splitwise-efficient-generative-llm-inference-using-phase-splitting/). 


This KEP is to have LeaderWorkerSet to support running both prefill/decode servers in one pod group (aka one replica)


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
As a user, I should be able to split each LeaderWorkerSet replica into smaller subgroups, and be able to deploy each subgroup on a separate accelerator island. 

### Risks and Mitigations

<!--
What are the risks of this proposal, and how do we mitigate? Think broadly.
For example, consider both security and how this will impact the larger
Kubernetes ecosystem.

How will security be reviewed, and by whom?

How will UX be reviewed, and by whom?

Consider including folks who also work outside the SIG or subproject.
-->
To avoid the changes from affecting initial functionality and to not set 
unnecessary labels, there will be no default behavior if `SubGroupSize` is not 
set

## Design Details

<!--
This section should contain enough information that the specifics of your
change are understandable. This may include API specs (though not always
required) or even code snippets. If there's any ambiguity about HOW your
proposal will be implemented, this is the place to discuss them.
-->
### LeaderWorkerSet API
We extend the LeaderWorkerSet API to introduce a new field: subGroupSize to opt in and set the number of pods that each subgroup will contain. Current behavior is kept if not set. 

```
type LeaderWorkerSetSpec struct {
	 // Number of pods per subgroup. This value is immutable,
   // and must not be greater than LeaderWorkerSet.Spec.Size.
	SubGroupSize *int32 'json:"subGroupSize,omitempty'
} 
```

### Exclusive Topology Support
LeaderWorkerSet can guarantee that the leader and the workers are placed in the same topology if the `leaderworkerset.sigs.k8s.io/exclusive-topology` annotation is set. Similarly, we will support that the pods within the same subgroup will be placed in the same topology with a new annotation `leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology`, so that the new changes can support up to two levels of pod affinity. 

The overall workflow will look like this 
![kep-115](https://github.com/kubernetes-sigs/lws/assets/86417275/ff9fc93d-c738-4c09-abc8-50a7b16d49df)

Suppose we have an LWS deployment with 2 replicas, subGroupSize 2, size 4, and the following annotations: 
- `leaderworkerset.sigs.k8s.io/exclusive-topology: cloud.google.com/gke-placement-group` 
- `leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology: cloud.google.com/gke-nodepool`

The leader pod will first be scheduled on a placement group. Once a worker pod is created, it will follow the leader to the placement group. 
Afterward, it will be scheduled into a nodepool that has other pods with the same subgroup index. So, the placement will look something like this:

Placement will look like this
- Placement-group-1
  - Nodepool 1
    - LWS-0
    - LWS-0-1
  - Nodepool 2
    - LWS-0-2
    - LWS-0-3
- Placement-group-2
  - Nodepool 1 
    - LWS-1
    - LWS-1-1
  - Nodepool 2
    - LWS-1-2
    - LWS-1-3

### Implementation
- Two new annotations will be added
  - `leaderworkerset.sigs.k8s.io/ubgroup-exclusive-topology`
  - `leaderworkerset.sigs.k8s.io/subgroup-size` 
- Three new labels will be added,
  - `leaderworkerset.sigs.k8s.io/subgroup-index = worker-index/subGroupSize`
    - Tracks which subgroup the pod is part of 
  - `leaderworkerset.sigs.k8s.io/subgroup-worker-index = worker-index%subGroupSize`
    - index/identity of a pod inside the pod's subgroup
  - `leaderworkerset.sigs.k8s.io/subgroup-key` 
    - Pods that are part of the same subgroup will have an annotation that is a unique hash value will be generated from the name of the leader, and the subgroup-index

To support exclusive placement at the subgroup level, the pod webhook will inject the new labels, and set the pod affinity/anti-affinity on all the pods. If both levels of pod affinity are set, then the leader pod will contain two pod affinities, while the workers will have a node selector, and a single pod affinity set. 

### TPU Environment Variable Injection
Because the value of the TPU environmental variables will vary between subgroups, the way they are injected will be extended. `TPU_WORKER_ID` will be the value of `subgroup-worker-index`, while the value of `TPU_WORKER_HOSTNAMES` will only be a list of the pods in the same subgroup.

LeaderWorkerSet supports the leader not requesting TPU resources. This raises a problem when determining the values of `subgroup-index` and `subgroup-worker-index`. Suppose we have two TPU slices with two hosts each. Since the leader doesn’t request TPU resources, there will be four workers + the leader, meaning that one of the workers will have a worker index of four. Because of the way the new labels are calculated, this worker will have a subgroup-index of two, creating three subgroup indices (0,1,2) even though there are only two TPU slices.

If the leader does not request TPU resources, then the labels will have the following values
`leaderworkerset.sigs.k8s.io/subgroup-index = (workerIndex - 1) / subGroupSize`
`leaderworkerset.sigs.k8s.io/subgroup-worker-index = (workerIndex - 1) % subGroupSize`

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

Will be added to ensure that the annotations are injected into the leader and worker pods, and to verify that the new values of TPU hostnames and TPU worker ID are injected. 

#### Integration tests

<!--
Describe what tests will be added to ensure proper quality of the enhancement.

After the implementation PR is merged, add the names of the tests here.
-->

- The pod-webhook integration tests should test
  - Pod Affinity/Anti-Affinity is injected properly in leader and workers
  - The new labels are only added if `SubGroupSize` is set
  - The expected TPU environment variables are injected given different combinations of leader requests resources and subgroup sizes. 

#### End to End Tests

- Test that LWS deployment with subgrouping enabled will have correct pod labels, pod exclusive placement and work well with other features enabled, like failure handling and rolling update.


## Alternatives

<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->

### Only Set Pod-Affinity on workers with `subgroup-worker-index=0`
The original plan was to mimic how the leader worker group is scheduled. Only set the pod affinity/anti-affinity on pods with `subgroup-worker-index=0`, essentially treating them as a pseudo-leader. Once it has been scheduled, the pod webhook would set the node selector on all other pods so that they follow the pseudo-leader. 

This implementation is more complicated to implement, as it requires the pod webhook to have access to the client to be able to query which topology the pseudo-leader was scheduled to. Moreover, it can cause issues such as exponential backoff of the statefulset controller when too many pods fail to create because the subgroup index 0 is not yet scheduled.
