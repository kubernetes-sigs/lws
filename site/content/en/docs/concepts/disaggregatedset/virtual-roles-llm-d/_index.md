---
title: "Virtual Roles (with llm-d)"
linkTitle: "Virtual Roles (with llm-d)"
weight: 75
description: >
  Partition a shared-template role into independently scaled routing pools and select them with llm-d.
---

Virtual roles divide a parent role, such as `decode`, into named routing pools.
Each child has its own replica target and rollout budgets. The children share
one physical LeaderWorkerSet per parent role, slice, and revision, including
the parent's pod templates, container images, and resource requirements.
Changing a group's subrole assignment changes its routing label; it does not
change the model server's configuration.

One llm-d Endpoint Picker (EPP) can serve these pools. Keep discovery broad
enough to include the parent's serving endpoints, then select a subrole when
scheduling each request. You do not need a separate router for every child.
If children need different images or GPU resources, use separate parent roles.

<figure class="td-max-width-on-larger-screens">
  <video controls playsinline preload="metadata" style="width: 100%; height: auto;" poster="/images/disaggregatedset-virtual-roles-poster.png">
    <source src="/videos/disaggregatedset-virtual-roles.mp4" type="video/mp4">
    <p><a href="/videos/disaggregatedset-virtual-roles.mp4">Watch the virtual roles walkthrough.</a></p>
  </video>
  <figcaption>One shared-template role, independently sized routing pools, and one router. An illustrated Ordinal walkthrough, not a live cluster recording.</figcaption>
</figure>

## Understand the rollout limitation

{{% alert title="Strict routing needs subrole-aware revision selection" color="warning" %}}
The parent-role revision gate can select revision B when its prefill and short
decode pools are Ready, even if its long decode pool has no Ready endpoints.
A long request then loses all candidates during subrole filtering and receives
HTTP 503, although revision A could serve it. Revision selection happens before
this filtering; the router does not retry another revision afterward.

Adding routers alone does not fix this. Strict subrole routing needs revision
selection that accounts for the request's required subrole. Requiring every
child for every request would block all traffic whenever one required child
has zero Ready endpoints. The
fragments below demonstrate routing configuration, not a complete deployment
with safe subrole routing during rollouts.
{{% /alert %}}

## Example: route by prompt length

The existing `context-length-aware` plugin understands labels containing
numeric ranges. Naming the children `0-8192` and `8193-131072` lets it read the
controller's subrole label directly, without changing the coordinator's
`decode` profile selection.

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

These targets total six groups per slice, as at the start of the video. Parent `spec.replicas` is ignored;
omit parent `scaling`. The controller assigns
`disaggregatedset.x-k8s.io/subrole` to each group's pods. Both pools still use
the same images and resources.

Merge these entries into your EPP configuration:

```yaml
plugins:
- type: label-selector-filter
  name: assigned-decode
  parameters:
    matchLabels:
      disaggregatedset.x-k8s.io/role: decode
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

`Exists` excludes new pods awaiting assignment: the context-length plugin
otherwise accepts unlabeled endpoints. Range boundaries are inclusive, so
8,192 prompt tokens use the first pool and 8,193 use the second. Out-of-range
endpoints are removed; an empty eligible pool fails the request.

The plugin reads prompt tokens, excluding the output budget. Its automatically
supplied tokenizer estimates token counts unless you configure real
tokenization. Missing token data is treated as zero. Treat these ranges as a
routing policy, not validation of the server's maximum supported context.
Friendly names such as `short-context` need additional routing logic because
this plugin expects numeric ranges.

Only include inference-serving endpoints in discovery (normally group leaders),
not workers that do not accept requests. Both Ordinal and Hash expose the same
subrole labels; the video uses Ordinal. Hash deletion is best effort and may
temporarily affect a different subrole's availability.

For deployment wiring and limitations, see the llm-d
[context-length filter](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/framework/plugins/scheduling/scorer/contextlengthaware/README.md)
and [revision gate](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/framework/plugins/requestcontrol/screener/disaggregatedsetrollout/README.md).
See [virtual-role configuration](../roles/#virtual-roles) for scaling and
rollout controls.
