# KEP-666: Gang Scheduling in LWS

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [LWS-level Gang Scheduling](#lws-level-gang-scheduling)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Opt-in API](#opt-in-api)
  - [API Discovery and Prerequisites](#api-discovery-and-prerequisites)
  - [Lifecycle of Workload and PodGroup Objects](#lifecycle-of-workload-and-podgroup-objects)
  - [Escape Hatch: Pre-set pod.spec.schedulingGroup](#escape-hatch-pre-set-podspecschedulinggroup)
  - [Example](#example)
  - [Limitations of the alpha Workload and PodGroup APIs](#limitations-of-the-alpha-workload-and-podgroup-apis)
  - [Future Work: Hierarchical Gang via CompositePodGroup](#future-work-hierarchical-gang-via-compositepodgroup)
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

Integrate the upstream Kubernetes Workload and PodGroup APIs (alpha; [kubernetes/enhancements#5558][workload-kep], [kubernetes/enhancements#5832][podgroup-kep]) into LWS as a gang-scheduling provider. Opt-in is per-LWS via a typed alpha `spec.gangScheduling` field, gated at admission by [API discovery](#api-discovery-and-prerequisites) against the upstream `GenericWorkload` feature gate. LWS then treats each replica's pods (1 leader + (size − 1) workers) as one all-or-nothing scheduling unit via **one PodGroup per replica**.

[workload-kep]: https://github.com/kubernetes/enhancements/pull/5558
[podgroup-kep]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5832-decouple-podgroup-api

## Motivation

For most multi-host inference use cases, all pods in an LWS replica need to be running to serve requests (see [issue #167](https://github.com/kubernetes-sigs/lws/issues/167)). Because Kubernetes schedules pods independently, LWS deployments are susceptible to:

- **Partial scheduling.** Only a subset of a replica's pods is scheduled, wasting resources on a workload that cannot run.
- **Deadlocks.** Two LWS replicas are deployed on a cluster that can fit only one; both leader pods are scheduled first, neither replica's workers can find space, and neither replica can function.

[KEP-4671][kep4671] adds native gang scheduling to `kube-scheduler` via the Workload / PodGroup APIs, and brings group-level **TAS** (topology-aware scheduling) and **DRA** (dynamic resource allocation) along for free.

[KEP-407][kep407] already covers gang scheduling via third-party PodGroup CRDs (Volcano / coscheduling / YuniKorn); this KEP adds a parallel, upstream-native path for clusters with the `scheduling.k8s.io/v1alpha2` Workload and PodGroup APIs enabled.

[kep407]: https://github.com/kubernetes-sigs/lws/tree/main/keps/407-gang-scheduling
[kep4671]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/4671-gang-scheduling

### Goals

- Validate [KEP-4671][kep4671] (Workload / PodGroup APIs) for multi-host inference use cases.
- Support autoscaling at the replica level.

### Non-Goals

- Gang scheduling for [DisaggregatedSet][kep766] (needs hierarchical gangs — see [Limitations](#limitations-of-the-alpha-workload-and-podgroup-apis)).
- Other Workload-Aware-Scheduling features in alpha — [TAS][kep5732], [workload-aware preemption][kep5710], [PodGroup-shared ResourceClaims][kep5729]. LWS writes only `gang.minCount = Size`; users who need these go through the [Escape Hatch](#escape-hatch-pre-set-podspecschedulinggroup).
- `spec.gangScheduling` combined with `leaderworkerset.sigs.k8s.io/exclusive-topology` (rejected at admission).
- Mutating `LeaderWorkerTemplate.Size` while gang is enabled in LWS-managed mode (`gang.minCount` is immutable in alpha2; rejected at admission).
- `spec.gangScheduling && StartupPolicy: LeaderReady` (alpha cannot express leader-first gangs; rejected at admission — see [Limitations](#limitations-of-the-alpha-workload-and-podgroup-apis)).

[kep766]: https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet
[kep5710]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5710-workload-aware-preemption
[kep5729]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5729-resourceclaim-support-for-workloads
[kep5732]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5732-topology-aware-workload-scheduling

## Proposal

When `spec.gangScheduling` is set on the LWS, the validating webhook first runs [API discovery](#api-discovery-and-prerequisites) for the upstream `scheduling.k8s.io/v1alpha2` Workload and PodGroup GVKs (rejecting the request if either is missing); on success, LWS creates and owns one Workload (holding a gang PodGroup template) plus one standalone `PodGroup` per replica; each PodGroup's `MinCount` defaults to `LeaderWorkerTemplate.Size`, so all pods of a replica co-schedule by default. The pod webhook sets each pod's `spec.schedulingGroup.podGroupName` from its `leaderworkerset.sigs.k8s.io/group-index` label.

### User Stories

#### LWS-level Gang Scheduling

As a user running distributed inference with LWS, I want a replica's pods scheduled only if all of them fit; otherwise none should be scheduled, and other replicas of the same LWS should not be blocked because one replica is starved.

### Risks and Mitigations

**Prerequisites.** Requires the upstream `scheduling.k8s.io/v1alpha2` Workload / PodGroup APIs (gated by `GenericWorkload` on `kube-apiserver` and `kube-scheduler`) plus a scheduler build with the gang-scheduling plugin. The presence of the API resources is enforced by [API discovery at admission](#api-discovery-and-prerequisites); a disabled `GenericWorkload` gate cannot be discovered (without the gate `spec.schedulingGroup` is silently stripped), so it remains an install-time prerequisite.

**Upstream API churn.** Alpha may rename fields before beta. The LWS-side opt-in is an empty `spec.gangScheduling` struct in alpha (no upstream knobs surfaced), so churn is confined to controller / webhook code — track [#5558][workload-kep].

**Forward-looking: Workload immutability.** `Workload.spec.podGroupTemplates` is immutable upstream (harmless in alpha — no mutable knobs). If a future mutable Workload field forces delete-and-recreate, the migration must orphan-delete or drop the Workload ownerRef first to avoid cascading PodGroup GC.

**Forward-looking: deletion-protection finalizer.** [KEP-5832][podgroup-kep] §Deletion Protection makes the PodGroup finalizer mandatory at upstream beta. Once mandatory, recreating a leader pod will require its old PodGroup to fully delete before the new one can be created — see [Graduation Criteria](#graduation-criteria) for the migration path.

## Design Details

### Opt-in API

```go
// api/leaderworkerset/v1/leaderworkerset_types.go
type LeaderWorkerSetSpec struct {
    // ... existing fields ...

    // GangScheduling opts the LWS into LWS-managed gang scheduling via the
    // upstream Workload / PodGroup APIs (one PodGroup per replica,
    // MinCount = LeaderWorkerTemplate.Size). Alpha; subject to change.
    // +optional
    GangScheduling *GangSchedulingPolicy `json:"gangScheduling,omitempty"`
}

// Empty in alpha — presence is the only knob. TAS / DRA / ResourceClaims
// and hierarchical (KEP-6012) knobs are added additively as they stabilize.
type GangSchedulingPolicy struct{}
```

No LWS-side feature gate: the upstream `GenericWorkload` gate already controls whether `kube-apiserver` preserves `pod.spec.schedulingGroup`, and the webhook's [API discovery](#api-discovery-and-prerequisites) propagates that into LWS admission. Matches how LWS handles other typed alpha fields (`SubGroupPolicy`, `RolloutStrategy.MaxSurge`); a project-wide `pkg/features` scaffold, if ever needed, is tracked in [#850][lws-feature-gate-issue].

The validating webhook rejects:

1. `spec.gangScheduling` set when the v1alpha2 Workload / PodGroup APIs are not registered (see [API Discovery and Prerequisites](#api-discovery-and-prerequisites)).
2. Mutation of `spec.gangScheduling` on an existing LWS — `pod.spec.schedulingGroup` is immutable upstream, so a flip cannot retroactively gang-schedule already-running pods. Users delete-recreate the LWS to change mode.
3. A pre-set `pod.spec.schedulingGroup` without `spec.gangScheduling` set — set `spec.gangScheduling: {}` to opt into the [Escape Hatch](#escape-hatch-pre-set-podspecschedulinggroup) explicitly.

[lws-feature-gate-issue]: https://github.com/kubernetes-sigs/lws/issues/850

### API Discovery and Prerequisites

Setting `spec.gangScheduling` alone is not sufficient — LWS verifies the upstream API is actually available before accepting the opt-in:

- **Webhook discovery.** The validating webhook resolves the v1alpha2 Workload + PodGroup GVKs against a cached RESTMapper; if either is missing, admission rejects with an error naming the missing GVK so the operator knows to enable upstream `GenericWorkload`. The cache invalidates on `NoMatchError`, so installing the API takes effect on the next admission without an LWS restart.
- **Controller discovery.** `lws_controller` / `pod_controller` only start the Workload / PodGroup informers when discovery succeeds; admission already rejects new opt-ins, so this path only matters during transient `kube-apiserver` unavailability.
- **Gap: `GenericWorkload` gate.** Discovery cannot detect a disabled gate — the type can be served while the gate is off, in which case `kube-apiserver` silently strips `pod.spec.schedulingGroup`. Remains an install-time prerequisite (see [Risks](#risks-and-mitigations)); closes at upstream beta when the gate is on by default.

### Lifecycle of Workload and PodGroup Objects

When `spec.gangScheduling` is set (LWS-managed mode; for the escape-hatch sub-mode see [§Escape Hatch](#escape-hatch-pre-set-podspecschedulinggroup)):

- **Workload** — `<lws-name>`, with a single `podGroupTemplates[]` entry (`name: <lws-name>-pg-template`) shared by every replica. Created once by the **lws_controller** (same way it creates the headless service); owned by the LWS object.
- **PodGroups** — one per replica, named `<lws-name>-<group-index>` (the same name as the replica's leader pod). Created by the **pod_controller** when it reconciles a leader pod (only on leader pods, only if no PodGroup with that name exists). Steady-state count: `replicas` (transiently `replicas + maxSurge` during a rolling update).

Same-name across GVKs follows LWS's existing convention — `<lws-name>` is already the leader StatefulSet / shared Service; `<lws-name>-<group-index>` is already the leader pod, worker StatefulSet, and per-replica Service — and matches [KEP-407][kep407]'s PodGroup naming so the two gang-scheduling paths look identical to users.

The controllers maintain the following invariants:

- **Ownership.** The Workload is owned by the LWS. Each PodGroup's controller owner is **its leader pod**, with the Workload as an additional non-controller `ownerReferences` entry — the leader-pod ownerRef makes scale-down / surge reclaim trivially correct (PodGroup GC'd with its leader pod); the Workload ownerRef is a backstop for full-LWS deletion.
- **Ordering.** The pod webhook stamps `spec.schedulingGroup.podGroupName = <lws-name>-<group-index>` on every pod. A leader pod can briefly precede its PodGroup (between leader-STS creation and the pod_controller reconcile) — the scheduler reports it `UnschedulableAndUnresolvable` and re-enqueues once the PodGroup appears ([KEP-5832][podgroup-kep]); transient, not a deadlock. Worker pods never precede the PodGroup, because pod_controller writes `podGroupName` into the worker StatefulSet only after creating the PodGroup.
- **Workload as type, PodGroup as instance.** The Workload is created once and never mutated (`Workload.spec.podGroupTemplates` is immutable in alpha2). Each PodGroup carries an inline copy of the gang policy (Copy/Inline model from [KEP-5832][podgroup-kep]) and references the Workload via `podGroupTemplateRef.workload` for traceability.
- **Size mutability.** In LWS-managed mode, admission rejects Size mutation while gang is on (`gang.minCount` is immutable in alpha2); users delete-recreate the LWS to resize. In [escape-hatch](#escape-hatch-pre-set-podspecschedulinggroup) mode Size stays mutable — the user owns `MinCount` synchronization. The restriction lifts once upstream allows mutating `gang.minCount` (see [Graduation Criteria](#graduation-criteria)). [KEP-552][kep552] keeps Size mutable in general.
- **Stable naming across revisions.** PodGroups are keyed by `group-index` (not by revision), so a leader pod recreated during a template-only rolling update references the same PodGroup name; the underlying object is recreated whenever its leader pod is.

[kep552]: https://github.com/kubernetes-sigs/lws/tree/main/keps/552-worker-resizing

Per-PodGroup scheduling state is reported on `PodGroup.status.conditions[PodGroupScheduled]` ([KEP-5832][podgroup-kep]); alpha does not aggregate it into LWS status — users read it directly on the PodGroup. Beta will surface unschedulable PodGroups on the LWS object (condition and/or event).

### Escape Hatch: Pre-set pod.spec.schedulingGroup

**Trigger.** `spec.gangScheduling` set **and** `pod.spec.schedulingGroup` pre-set in `LeaderTemplate` or `WorkerTemplate`. Following the upstream Job controller integration pattern, LWS hands gang lifecycle to an external owner (e.g. Kueue, DisaggregatedSet):

- The **lws_controller** does not create the Workload.
- The **pod_controller** does not create per-replica PodGroups.
- The **pod webhook** does not override the user-provided `podGroupName`.
- LWS does not adopt Workload or PodGroup objects it did not create.

The user is responsible for creating the Workload and PodGroup objects (with whatever names the pod template references) before pods are scheduled. This is the knob-free handoff for external integrations (e.g. Kueue pre-creating these objects on LWS's behalf), with no extra LWS API surface.

### Example

The user opts in by setting `spec.gangScheduling` on the LWS:

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: leaderworkerset-sample
spec:
  replicas: 4
  gangScheduling: {}    # MinCount = size (= 2); no knobs in alpha
  leaderWorkerTemplate:
    size: 2
    leaderTemplate: { spec: {} }
    workerTemplate: { spec: {} }
```

The Workload that LWS creates:

```yaml
apiVersion: scheduling.k8s.io/v1alpha2
kind: Workload
metadata:
  name: leaderworkerset-sample
  ownerReferences:
    - apiVersion: leaderworkerset.x-k8s.io/v1
      kind: LeaderWorkerSet
      name: leaderworkerset-sample
      controller: true
spec:
  podGroupTemplates:
    - name: leaderworkerset-sample-pg-template
      schedulingPolicy:
        gang:
          minCount: 2
```

Each replica gets one PodGroup, named after its leader pod; replica `0` is shown below, replicas `1` … `3` follow the same shape with their `group-index` in the name:

```yaml
apiVersion: scheduling.k8s.io/v1alpha2
kind: PodGroup
metadata:
  name: leaderworkerset-sample-0   # same as the replica's leader pod
  ownerReferences:
    - apiVersion: v1
      kind: Pod
      name: leaderworkerset-sample-0   # leader pod for replica 0; controller owner
      controller: true
    - apiVersion: scheduling.k8s.io/v1alpha2
      kind: Workload
      name: leaderworkerset-sample   # backstop for full-LWS deletion
spec:
  podGroupTemplateRef:
    workload:
      workloadName: leaderworkerset-sample
      podGroupTemplateName: leaderworkerset-sample-pg-template
  schedulingPolicy:
    gang:
      minCount: 2
```

In general, `replicas: N, size: M` produces **N PodGroups, M pods each**.

### Limitations of the alpha Workload and PodGroup APIs

Alpha2's gang policy exposes a single `MinCount` scalar — no per-role or per-subgroup minimums. That is fine for the LWS-level case (one replica = one PodGroup, all M pods co-schedule), but not for:

- **[DisaggregatedSet][kep766]**: needs *"≥1 prefill AND ≥1 decode replica ready simultaneously"*, unexpressible by any single `MinCount`.
- **`SubGroupPolicy`**: the natural design is one PodGroup per SubGroup (each with its own `subgroup-exclusive-topology` constraint) plus a parent gang over all SubGroup PodGroups in the replica.
- **Per-role gang policy** (leader/worker split — one leader PodGroup + one worker PodGroup under a parent gang). A single `MinCount` conflates the two roles; splitting them unblocks:
  - **`minCount < size`** (leader-first gang): leader `MinCount = 1`, worker `MinCount` user-chosen.
  - **Leader preemption priority**: per-role `priorityClassName` lets workers be reclaimed before the leader under contention.
  - **Heterogeneous role minimums**: *"1 leader AND (size − 1) workers"* rather than *"any M pods"*, which matters when leader and worker resource shapes differ — e.g. a CPU-only leader (coordinator / RPC head) paired with workers that must all land on a single accelerator slice. Such a shape is already expressible at the **placement** layer via `SubGroupPolicy` + `subgroup-exclusive-topology`, but alpha gang admission can only enforce *"any M pods"*, not the *"1 CPU leader AND (size − 1) accelerator workers"* invariant the workload actually needs.
- **`StartupPolicy: LeaderReady`**: same root cause — no `MinResources`, so a single-`MinCount` PodGroup can't admit the leader before workers exist.

All of these resolve via hierarchical PodGroups upstream (tracked by [KEP-6012][kep6012]; also [KEP-5832][podgroup-kep] §Future Plans). Until that lands, alpha creates a single replica-level PodGroup with `MinCount = LeaderWorkerTemplate.Size`; SubGroup keeps driving placement (topology + TPU env), but per-subgroup gang minimums are not enforced. KEP-766 will track the LWS-side design for DisaggregatedSet once hierarchy lands.

[kep6012]: https://github.com/kubernetes/enhancements/issues/6012

### Future Work: Hierarchical Gang via CompositePodGroup

[KEP-6012][kep6012] (`CompositePodGroup`) provides the building blocks for lifting the limitations above. The alpha objects defined here are forward-compatible with it on two invariants:

- **Pod-level binding is invariant.** `pod.spec.schedulingGroup.podGroupName` always points at a **leaf** `PodGroup`; leaf names like `<lws-name>-<group-index>` stay stable across alpha → hierarchical.
- **Flat LWS case is untouched.** [KEP-6012 §Backward compatibility][kep6012-pr] preserves the standalone-`PodGroup` consumption pattern — LWS objects on the alpha `MinCount = Size` path never migrate.

**Single-LWS per-role split.** For the [Per-role gang policy](#limitations-of-the-alpha-workload-and-podgroup-apis) cases (leader-first, leader preemption priority, heterogeneous role minimums), each replica becomes a CompositePodGroup with one leaf per role; leader pods bind to the `-leader` leaf, worker pods to the `-workers` leaf:

```text
CompositePodGroup <lws-name>-<group-index>
├─ PodGroup       <lws-name>-<group-index>-leader    minCount = 1            (parentRef=<lws-name>-<group-index>)
└─ PodGroup       <lws-name>-<group-index>-workers   minCount = lws.size - 1 (parentRef=<lws-name>-<group-index>)
```

For the heterogeneous-role shape called out above, this lifts the *"1 CPU leader AND (size − 1) accelerator workers"* invariant from placement-only (`SubGroupPolicy` + `subgroup-exclusive-topology`) into gang admission itself, without changing pod-level binding (`pod.spec.schedulingGroup.podGroupName` still points at a leaf).

**Cross-LWS gangs.** Gangs that span multiple LWS objects — e.g. [DisaggregatedSet][kep766]'s *"≥ M prefill AND ≥ N decode replicas co-schedule"*, the multi-node case of the inter-PodGroup gang gap raised in upstream [kubernetes/kubernetes#136207][k8s136207] — are layered on top of the alpha [Escape Hatch](#escape-hatch-pre-set-podspecschedulinggroup) by an external `Workload` owner. With the escape hatch in place, DisaggregatedSet or any future external consumer can build whatever `CompositePodGroup` tree they need with **zero new LWS-side API surface**. Minimal shape — one prefill LWS + one decode LWS joined under a root CPG so the serving instance is all-or-nothing:

```yaml
# Workload owned by an external controller (e.g. DisaggregatedSet);
# both per-role LWS objects are in escape-hatch mode.
spec:
  compositePodGroupTemplates:
    - name: serving-root
      schedulingPolicy:
        gang: { minGroupCount: 2 }   # both leaves must co-schedule
      podGroupTemplates:
        - name: prefill-pg
          schedulingPolicy:
            gang: { minCount: 4 }    # = LWS(prefill).size
        - name: decode-pg
          schedulingPolicy:
            gang: { minCount: 2 }    # = LWS(decode).size
```

```text
CompositePodGroup serving-root
├─ PodGroup       prefill-pg       (parentRef=serving-root)
└─ PodGroup       decode-pg        (parentRef=serving-root)
```

The full DisaggregatedSet shape — per-revision tree, role × replica `compositePodGroupTemplates`, interaction with the N-dimensional rolling update — belongs to [KEP-766][kep766].

[kep6012-pr]: https://github.com/kubernetes/enhancements/pull/6017
[k8s136207]: https://github.com/kubernetes/kubernetes/issues/136207

### Test Plan

[x] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

##### Prerequisite testing updates

None.

##### Unit tests

- LWS validating webhook rejects:
  - mutating `spec.gangScheduling` on an existing LWS
  - Size mutation while gang is on (LWS-managed mode only — escape-hatch mode allows it)
  - `gang + StartupPolicy=LeaderReady`
  - `gang + exclusive-topology`
  - `gang` when the v1alpha2 Workload / PodGroup API resources are not registered (API discovery)
  - pre-set `pod.spec.schedulingGroup` in either template without `spec.gangScheduling` set (umbrella opt-in required)
- Pod mutating webhook stamps `spec.schedulingGroup.podGroupName = <lws-name>-<group-index>` only when gang is on and the pod template has no pre-set `schedulingGroup`.
- lws_controller builds Workload `<lws-name>` (LWS ownerRef; single `podGroupTemplates[]` entry named `<lws-name>-pg-template`).
- pod_controller builds PodGroup `<lws-name>-<group-index>` (leader-pod controller ownerRef + Workload non-controller ownerRef; `MinCount = Size`; `podGroupTemplateRef.workload` → the Workload).

##### Integration tests

Added under `test/integration/webhooks/` and `test/integration/controllers/`.

- Creating an LWS with gang on yields the Workload + `replicas` PodGroups; every pod's `podGroupName` matches its `group-index`.
- **Ordering**: on scale-up and `maxSurge` bursts, worker pods always observe their PodGroup; a leader pod may be transiently `UnschedulableAndUnresolvable` until pod_controller creates the PodGroup, then becomes schedulable.
- Scale-down / surge reclaim deletes the leader pod; PodGroup GC's via the leader-pod ownerRef (no explicit reclaim).
- Deleting the LWS cascades to the Workload and — via leader pods + Workload backstop — all PodGroups.
- Escape hatch: if `pod.spec.schedulingGroup` is pre-set in the template, the controller creates neither Workload nor PodGroup, and the webhook does not override `podGroupName`.

##### e2e tests

Deferred for alpha. End-to-end coverage requires a CI cluster with `GenericWorkload` enabled, the `scheduling.k8s.io/v1alpha2` Workload / PodGroup API resources served, and a `kube-scheduler` build shipping the gang-scheduling plugin. We will add e2e once those prerequisites land in CI (mirroring the e2e deferral in [KEP-407][kep407]).

### Graduation Criteria

Targets `alpha` while the upstream API is alpha. Promotion past alpha is gated on:

- Upstream `scheduling.k8s.io/v1alpha2` reaching beta with stable field names.
- Integration coverage for the lifecycle invariants above.
- Per-PodGroup unschedulable status surfaced on the LWS object (condition / event).
- A chosen path for re-enabling Size mutability under the mandatory deletion-protection finalizer, in priority order: (1) push upstream `gang.minCount` mutability so PodGroups can be patched in place; (2) switch to revision-keyed PodGroup names (`<lws-name>-<group-index>-<rev>`) that let old PodGroups GC naturally — contingent on the upstream finalizer treating an absent/deleted pod as terminal.
- Promote `spec.gangScheduling` to beta stability (drop the alpha disclaimer; field is already served by the v1 LWS API), aligned with upstream `GenericWorkload` reaching beta-on-by-default; no API rename — the same struct gains stabilized upstream knobs (e.g. `MinResources`, hierarchical / per-role minimums from KEP-6012) additively as they land.

## Implementation History

- 2025-10-13: Initial external draft by @Edwinhr716 ([Google Doc][gdoc])
- 2026-05-04: Imported as KEP-666 (PR [#844][pr844]); alpha2 Workload + per-replica PodGroups (`MinCount = Size`)
- 2026-05-06: Aligned with prototype and [KEP-407][kep407] naming; escape-hatch lifecycle replaces the rejected `podGroupNamePrefix` knob
- 2026-05-07: Added Future Work for hierarchical gangs via [KEP-6012][kep6012]; cross-LWS gangs layer on the escape hatch
- 2026-05-09: PR review pass: typed `spec.gangScheduling` replaces the annotation as the umbrella opt-in (admission rejects pre-set `pod.spec.schedulingGroup` without it); no LWS-side feature gate; per-role gang policy split; additional WAS Non-Goals

[gdoc]: https://docs.google.com/document/d/1QlcIBtR2KyOKYRUTGubhhxuy7NfjHs1fXMJlvdUCyhM
[pr844]: https://github.com/kubernetes-sigs/lws/pull/844

## Drawbacks

1. **Alpha API churn risk.** Upstream Workload + PodGroup are alpha; field renames before beta would require LWS controller / webhook updates and may force users to delete-recreate gang-enabled LWS objects. Mitigated by keeping the LWS-side opt-in to an empty `spec.gangScheduling` struct (no upstream knobs surfaced).

2. **PodGroup count scales with replicas.** One PodGroup per replica means a 1000-replica LWS produces 1000 PodGroup objects, each with its own controller bookkeeping and etcd footprint. Acceptable for typical multi-host inference (replica counts in tens to low hundreds); very-high-replica deployments can revisit once hierarchical PodGroups land ([KEP-6012][kep6012]).

3. **Two parallel gang-scheduling paths.** Users now choose between this KEP and [KEP-407][kep407] (third-party CRDs) based on which scheduler the cluster runs. A transient cost during alpha — beta of upstream `GenericWorkload` should make the upstream-native path the default, with KEP-407 narrowing to a third-party-only fallback.

## Alternatives

**Annotation-only opt-in in alpha**.
Rejected: can't be schema-validated, hard to deprecate, no place for future TAS / DRA / RC knobs. A typed alpha `spec.gangScheduling` struct gets all three for free.

**An LWS-side feature gate for alpha**.
Rejected: upstream `GenericWorkload` is already the kill switch (its `kube-apiserver` half decides whether `pod.spec.schedulingGroup` survives at all), and webhook API discovery propagates it to LWS admission. A parallel LWS gate would duplicate that signal and force LWS to stand up its first feature-gate scaffold ([#850][lws-feature-gate-issue]). Existing typed alpha fields (`SubGroupPolicy`, `RolloutStrategy.MaxSurge`) ship without one too.

**One shared PodGroup per LWS with `Replicas=N, MinCount=M`** (Edwin's original draft).
Rejected: `MinCount` only requires M co-scheduled pods, with no notion of which replica they belong to — the scheduler may legally pick M pods from different replicas, none complete, and the model still cannot start. Per-replica PodGroups make each replica an independent all-or-nothing unit.

**Rely on [KEP-407][kep407] only**.
KEP-407 targets third-party schedulers (Volcano / coscheduling / YuniKorn) via their own PodGroup CRDs; this KEP targets the upstream-native `scheduling.k8s.io/v1alpha2` Workload and PodGroup APIs. The two evolve independently — different prerequisites, different API surfaces, no shared data path — and a single LWS object opts into at most one.

**Explicit `podGroupNamePrefix` knob for user-managed lifecycle**.
Rejected: the implicit [Escape Hatch](#escape-hatch-pre-set-podspecschedulinggroup) (skip LWS-managed creation when `pod.spec.schedulingGroup` is already set in the pod template) gives the same opt-out semantics with no new API surface, matching the upstream Job controller integration pattern.
