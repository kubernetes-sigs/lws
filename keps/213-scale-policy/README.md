# KEP-213: Scale policy API

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
  - [API](#api)
  - [Implementation](#implementation)
    - [lws webhook](#leaderWorkerSet-webhook)
    - [lws controller](#leaderWorkerSet-controller)
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
This KEP aims to add a scalePolicy field `spec.scalePolicy` to the LeaderWorkerSet's API to indicate the scaling policy for LeaderWorkerSet in the group.

## Motivation

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
- We add a new way to scale up and down. With this approach, we can dynamically adjust the number of LeaderWorkerSet replicas based on predefined metrics and thresholds to meet the application's needs and make efficient use of resources.

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

We propose a new field `spec.scalePolicy` for the LeaderWorkerSet API. 
This field can be used to specify the scaling policy for the LeaderWorkerSet in the group, utilizing the Horizontal Pod Autoscaler (HPA) configuration.

### User Stories (Optional)

<!--
Detail the things that people will be able to do if this KEP is implemented.
Include as much detail as possible so that people can understand the "how" of
the system. The goal here is to make this feel real for users without getting
bogged down.
-->

#### Story 1

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
```go
type LeaderWorkerSetSpec struct {
  ...

  // ScalePolicy if set, configures how to scale the LeaderWorkerSet.
  // +optional
  // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"
  ScalePolicy *ScalePolicy `json:"scalePolicy,omitempty"`
}
```

```go
type ScalePolicy struct {
  // MinReplicas is the lower limit for the number of replicas to which the autoscaler
  // can scale down.  It defaults to 1.
  // +optional
  MinReplicas *int32 `json:"minReplicas,omitempty"`
  // MaxReplicas is the upper limit for the number of replicas to which the autoscaler can scale up.
  // It cannot be less that minReplicas.
  // +optional
  MaxReplicas *int32 `json:"maxReplicas,omitempty"`
  // Metrics contains the specifications which are used to calculate the
  // desired replica count (the maximum replica count across all metrics will
  // be used).  The desired replica count is calculated with multiplying the
  // ratio between the target value and the current value by the current
  // number of pods. Ergo, metrics used must decrease as the pod count is
  // increased, and vice-versa.  See the individual metric source types for
  // more information about how each type of metric must respond.
  // If not set, the HPA will not be created.
  // +optional
  Metrics []autoscalingv2.MetricSpec `json:"metrics,omitempty"`
}
```

### Implementation
When setting `spec.scalePolicy`, automatically create an HPA associated with the LeaderWorkerSet, 
allowing the HPA functionality to apply to the LeaderWorkerSet.
#### LeaderWorkerSet webhook
The `spec.scalePolicy` is an optional field by default, and no processing is required in the webhook.

#### LeaderWorkerSet controller
When the LeaderWorkerSet enters the reconciliation loop, it goes into the `reconcileHPA` method.
```go
func (r *LeaderWorkerSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
  // Get leaderworkerset object
  lws := &leaderworkerset.LeaderWorkerSet{}
  if err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, lws); err != nil {
      return ctrl.Result{}, client.IgnoreNotFound(err)
  }
  log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
  ctx = ctrl.LoggerInto(ctx, log)

  // Reconcile the HPA
  err := r.reconcileHPA(ctx, lws)
  if err != nil {
      log.Error(err, "Reconcile LWS HPA error")
      return ctrl.Result{}, err
  }

  partition, replicas, err := r.rollingUpdateParameters(ctx, lws)
  if err != nil {
      log.Error(err, "Rolling partition error")
      return ctrl.Result{}, err
  }
  ...
}	
```

1. Check if `spec.ScalePolicy == nil || spec.ScalePolicy.Metrics == nil`. If it's empty, it means no scale policy operation is needed.
```go
if lws.Spec.ScalePolicy == nil || lws.Spec.ScalePolicy.Metrics == nil {
    log.V(1).Info("No ScalePolicy or Metric is specified, skipping HPA reconciling process")
    return nil
}
```
2. Set up HPA-related fields (name, namespace, ownerReferences...).
```go
func generateHPA(lws *leaderworkerset.LeaderWorkerSet, scheme *runtime.Scheme) (
  *autoscalingv2.HorizontalPodAutoscaler, error) {
  hpa := &autoscalingv2.HorizontalPodAutoscaler{
      ObjectMeta: metav1.ObjectMeta{
          Name:      lws.Name,
          Namespace: lws.Namespace,
      },
      Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
          ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
              Kind:       lws.Kind,
              Name:       lws.Name,
              APIVersion: lws.APIVersion,
          },
          MinReplicas: lws.Spec.ScalePolicy.MinReplicas,
          MaxReplicas: *lws.Spec.ScalePolicy.MaxReplicas,
          Metrics:     lws.Spec.ScalePolicy.Metrics,
      },
  }
  if err := controllerruntime.SetControllerReference(lws, hpa, scheme); err != nil {
      return nil, err
  }
  return hpa, nil
}
```
3. Check if the HPA exists; if not, create it.
4. Check if the HPA needs an update; if so, update the HPA.
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

All core changes must be covered by unit tests, in both API and pod controller.
- API: API validation.
- lws controller
- Pod controller

- `<package>`: `<date>` - `<test coverage>`

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

The following scenarios need to be covered in integration tests:

// TODO: add integration tests to cover this scenario

##### e2e tests

<!--
This question should be filled when targeting a release.
For Alpha, describe what tests will be added to ensure proper quality of the enhancement.

For Beta and GA, add links to added tests together with links to k8s-triage for those tests:
https://storage.googleapis.com/k8s-triage/index.html

We expect no non-infra related flakes in the last month as a GA graduation criteria.
-->
Deploy lws with `spec.scalePolicy` set.

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