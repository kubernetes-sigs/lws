---
title: "Revision Gating (with llm-d)"
linkTitle: "Revision Gating (with llm-d)"
weight: 70
description: >
  Keep requests within one revision and shift traffic as a DisaggregatedSet rolls out.
---

During a DisaggregatedSet rollout, old and new Pods run together. LWS creates and retires those Pods; the **llm-d rollout screener** decides which revision receives each request.

Prefill and Decode from different revisions may use incompatible KV-cache formats or transfer protocols. The screener keeps the two phases on the same revision.

<figure class="td-max-width-on-larger-screens">
<!-- Keep a compact area for native playback controls below the picture. -->
<div style="position: relative; padding-top: calc(75% + 2rem);">
<video controls autoplay muted playsinline preload="metadata" poster="/images/disaggregatedset-llm-d-routing-poster.png" width="1400" height="1050" class="img-fluid d-block" style="position: absolute; inset: 0; width: 100%; height: 100%; object-fit: contain; object-position: top; background: #fff;" aria-label="Revision gating with llm-d during a DisaggregatedSet rollout">
  <source src="/videos/disaggregatedset-llm-d-routing.mp4" type="video/mp4">
  <a href="/videos/disaggregatedset-llm-d-routing.mp4">Download the revision gating animation.</a>
</video>
</div>
<figcaption class="small text-muted mt-2">The same gating applies to either router deployment option. Each square is a Pod; percentages are expected shares of new requests, not exact quotas. Selected snapshots and timing are illustrative.</figcaption>
</figure>

## How traffic moves

First, a revision needs at least one **Ready Pod in every required role**. B receives no traffic while its Prefill Pod is missing or still starting, even if its Decode Pods are Ready.

With `max-role`, the screener finds the role with the most Ready Pods **across all eligible revisions**. It uses that same role to calculate each revision's traffic share. Here, Decode is largest: six Ready Decode Pods in A and four in B give B a **40% probability** of receiving a new request. More B Pods becoming Ready changes that split as the rollout progresses.

The weights count Ready **Pods**, not replica groups, GPUs, or measured serving capacity. This approximation works best when the role ratio and per-Pod capacity remain similar across revisions. It does not replace LWS's rollout availability checks.

## One shared router or one per role

**One shared llm-d router** is the simpler starting point. One scheduling call can run both profiles. The screener chooses a revision before either profile runs, so Prefill and Decode use the same revision.

**One router per role** separates Prefill and Decode into two router deployments. The **llm-d coordinator** carries the revision choice between them:

- A Prefill llm-d router, which chooses the revision.
- The llm-d coordinator, which copies Prefill's `x-llm-d-disagg-revision` response header into the Decode request.
- A Decode llm-d router, which reuses that revision instead of making another weighted choice.

The split is per **role**, not per revision: each router can serve A and B during the rollout. Either layout can use multiple replicas of each router deployment.

The header must be forwarded between separate Prefill and Decode scheduling calls, even if those calls use the same router deployment.

## Configure llm-d

Add this entry to the top-level `plugins` list in your llm-d `EndpointPickerConfig`. With separate routers, configure it in both. Installing LWS alone does not configure the router.

```yaml
plugins:
- type: disaggregatedset-rollout-screener
  name: rollout-screener
  parameters:
    scope:
      labelSelector: "disaggregatedset.x-k8s.io/name=my-set"
    revisionGating:
      mode: max-role
      requiredRoles: [prefill, decode]
      # All fields below are optional; their default values are shown.
      revisionHeaderName: x-llm-d-disagg-revision
      revisionLabelKey: disaggregatedset.x-k8s.io/revision
      roleLabelKey: disaggregatedset.x-k8s.io/role
```

Replace `my-set` and the role names with yours. Each router must be able to see both roles and all revisions of the set.

See the [llm-d rollout screener documentation](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/framework/plugins/requestcontrol/screener/disaggregatedsetrollout/README.md) for the full setup, including coordinator configuration and advanced options.
