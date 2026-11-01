---
title: "Virtual Roles (with llm-d)"
linkTitle: "Virtual Roles (with llm-d)"
weight: 75
description: >
  Partition a shared-template role into independently scaled routing pools and select them with llm-d.
---

Virtual roles divide a parent role, such as `decode`, into named routing pools.
Each child has its own replica target and rollout budgets. Children share one
physical LeaderWorkerSet per parent role, slice, and revision, with the same
pod templates, container images, and resources. Reassignment changes a group's
routing label, without changing its server configuration.

One llm-d Endpoint Picker (EPP) can discover the parent's serving endpoints and
select subroles per request. You do not need a separate router for every child.
If children need different images or GPU resources, use separate parent roles.

<figure class="td-max-width-on-larger-screens">
  <video controls playsinline preload="metadata" style="width: 100%; height: auto;" poster="/images/disaggregatedset-virtual-roles-poster.png">
    <source src="/videos/disaggregatedset-virtual-roles.mp4?v=fixed-pair" type="video/mp4">
    <p><a href="/videos/disaggregatedset-virtual-roles.mp4">Watch the virtual roles walkthrough.</a></p>
  </video>
  <figcaption>Shared-template pools, one or two routers, and request-dependent routing. An illustrated walkthrough, not a live cluster recording.</figcaption>
</figure>

## Example: a fixed Prefill/Decode pair

For requests that always use `prefill/batch` followed by `decode/standard`, the
existing rollout screener can require both subroles in the selected revision.
Define the children under their respective parents:

```yaml
spec:
  roles:
  - name: prefill
    subRoles: [{name: batch, replicas: 2}]
    # Retain this parent's spec.leaderWorkerTemplate.
  - name: decode
    subRoles: [{name: standard, replicas: 4}]
    # Retain this parent's spec.leaderWorkerTemplate.
```

Keep `batch` and `standard` unique within this set: the gate reads raw child
names. These fragments assume leaders serve inference. Retain existing
templates, discovery, phase selection, scorers, and pickers. With `subRoles`,
omit parent `scaling`; parent `spec.replicas` is ignored.

This example uses one slice. Replace `my-set` with your DisaggregatedSet name.
The required pair applies to every request using this screener; it does not
change with the selected profile or the request's prompt length.

Use these common EPP plugin definitions:

```yaml
plugins:
- type: disaggregatedset-rollout-screener
  name: rollout-screener
  parameters:
    scope:
      labelSelector: >-
        disaggregatedset.x-k8s.io/name=my-set,
        leaderworkerset.sigs.k8s.io/worker-index=0
    revisionGating:
      roleLabelKey: disaggregatedset.x-k8s.io/subrole
      requiredRoles: [batch, standard]
      mode: max-role
- type: label-selector-filter
  name: prefill-batch
  parameters:
    matchLabels:
      disaggregatedset.x-k8s.io/name: my-set
      disaggregatedset.x-k8s.io/role: prefill
      disaggregatedset.x-k8s.io/subrole: batch
      leaderworkerset.sigs.k8s.io/worker-index: "0"
- type: label-selector-filter
  name: decode-standard
  parameters:
    matchLabels:
      disaggregatedset.x-k8s.io/name: my-set
      disaggregatedset.x-k8s.io/role: decode
      disaggregatedset.x-k8s.io/subrole: standard
      leaderworkerset.sigs.k8s.io/worker-index: "0"
```

### One shared router

Install all three plugins and keep both phase profiles in the same EPP:

```yaml
schedulingProfiles:
- name: prefill
  plugins:
  - pluginRef: prefill-batch
  # Keep existing scorers and picker.
- name: decode
  plugins:
  - pluginRef: decode-standard
  # Keep existing scorers and picker.
```

The phase handler chooses the appropriate profile. The screener stays in the
top-level `plugins` list; never add it to a scheduling profile.

### Two routers

The Prefill EPP installs the same screener plus `prefill-batch`, with only the
`prefill` profile above. The Decode EPP installs the same screener plus
`decode-standard`, with only the `decode` profile.

Both EPPs' Pod notification watchers must observe **both child pools and all
revisions**, while endpoint selection stays within each phase. The coordinator
forwards Prefill's `x-llm-d-disagg-revision` response header into Decode's request,
pinning its revision. Preserve this default forwarding, including for separate
phase calls to one shared router. See the published
[revision-gating guide](https://lws.sigs.k8s.io/docs/concepts/disaggregatedset/llm-d-integration/)
for deployment wiring.

## Request-dependent routing needs more care

{{% alert title="Short/Long routing with a parent-role gate" color="warning" %}}
For request-dependent Short/Long routing, a parent-role gate requiring
`[prefill, decode]` can select revision B with Prefill and Short Ready but no
Long endpoints. Subrole filtering then leaves a long request with no candidates:
HTTP 503, although A could serve it. The router chooses the revision before
this filtering and does not retry another revision.

The fixed-pair gate covers its two required children. Short/Long revision
selection must account for the request's required child. Adding routers alone
does not provide that policy; globally requiring both children blocks all
traffic whenever either has zero Ready endpoints. The next example demonstrates
filtering, without solving that rollout policy.
{{% /alert %}}

## Example: route by prompt length

The `context-length-aware` plugin reads numeric ranges. Children named
`0-8192` and `8193-131072` work directly with the controller's subrole label,
retaining the coordinator's `decode` profile selection.

Add this fragment to the DisaggregatedSet's roles list, retaining your decode
role's existing `spec.leaderWorkerTemplate`:

```yaml
spec:
  roles:
  - name: decode
    subRoles:
    - name: "0-8192"
      replicas: 4
    - name: "8193-131072"
      replicas: 2
    # Retain spec.leaderWorkerTemplate here.
```

These targets total six groups per slice. The controller assigns
`disaggregatedset.x-k8s.io/subrole` to each group's pods. Both pools use the same
images and resources.

Merge these entries into your EPP configuration:

```yaml
plugins:
- type: label-selector-filter
  name: assigned-decode
  parameters:
    matchLabels:
      disaggregatedset.x-k8s.io/name: my-set
      disaggregatedset.x-k8s.io/role: decode
      leaderworkerset.sigs.k8s.io/worker-index: "0"
    matchExpressions:
    - key: disaggregatedset.x-k8s.io/subrole
      operator: Exists
- type: context-length-aware
  name: decode-subrole-by-length
  parameters:
    label: disaggregatedset.x-k8s.io/subrole
    enableFiltering: true
schedulingProfiles:
- name: decode
  plugins:
  - pluginRef: assigned-decode
  - pluginRef: decode-subrole-by-length
    weight: 1
  # Keep your existing scorers and picker.
```

`decode-subrole-by-length` is an arbitrary plugin instance name; keep its
`pluginRef` identical if you rename it.

`Exists` excludes pods awaiting assignment; the context-length plugin otherwise
accepts unlabeled endpoints. Inclusive boundaries send 8,192 prompt tokens to
the first pool and 8,193 to the second. An empty eligible pool fails the request.

The plugin counts prompt tokens, excluding output. Configure real tokenization
if estimates are insufficient; missing token data becomes zero. These ranges
express routing policy, without validating server context limits. Friendly names
such as `short-context` need additional routing logic.

Discover only serving endpoints. Ordinal and Hash expose the same subrole
labels; Hash deletion may temporarily affect a different child's availability.

For deployment wiring and limitations, see the llm-d
[context-length filter](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/framework/plugins/scheduling/scorer/contextlengthaware/README.md)
and [revision gate](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/framework/plugins/requestcontrol/screener/disaggregatedsetrollout/README.md).
See [virtual-role configuration](../roles/#virtual-roles) for scaling and
rollout controls.
