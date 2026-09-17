# KEP-898: Hash Group Identity

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Scale down without collateral churn](#story-1-scale-down-without-collateral-churn)
    - [Story 2: Relocate one specific group](#story-2-relocate-one-specific-group)
    - [Story 3: Autoscale serving groups with HPA](#story-3-autoscale-serving-groups-with-hpa)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
  - [Leader Deployment](#leader-deployment)
  - [Group Identity Assignment](#group-identity-assignment)
  - [Worker StatefulSets and Leader Address](#worker-statefulsets-and-leader-address)
  - [Rollouts](#rollouts)
  - [Scale Subresource and HPA](#scale-subresource-and-hpa)
  - [DisaggregatedSet Integration](#disaggregatedset-integration)
  - [Unsupported Combinations](#unsupported-combinations)
  - [Test Plan](#test-plan)
    - [Prerequisite testing updates](#prerequisite-testing-updates)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Alternative 1: Smarter victim selection in the leader StatefulSet](#alternative-1-smarter-victim-selection-in-the-leader-statefulset)
  - [Alternative 2: Controller-managed bare leader pods](#alternative-2-controller-managed-bare-leader-pods)
  - [Alternative 3: Solve it in consumers](#alternative-3-solve-it-in-consumers)
<!-- /toc -->

## Summary

This KEP adds a `groupIdentity` field to the LeaderWorkerSet spec with two values. `Ordinal` is the default and keeps today's behavior, where groups are named by the leader StatefulSet's ordinals. `Hash` gives each group a random identity instead of an ordinal and manages leader pods with a Deployment instead of a StatefulSet.

With hash identity, scale down removes unscheduled and not-ready groups before healthy ones, because that is how the ReplicaSet ranks its victims. Everything else about a group stays the same: workers still run in a per-group StatefulSet, rolling updates still move group by group within the `maxSurge` and `maxUnavailable` budgets, and startup policies and exclusive placement are unaffected.

## Motivation

Group identity in LWS is currently an ordinal, so the set of live groups is always `0..replicas-1`. Scale down can only remove the highest ordinal. When some other group is unhealthy, for example because its node died, scaling down removes a healthy group when targeting the unhealthy group would save the rebuild churn.

The ordinal system makes it impossible to remove or relocate one specific group. Issue [#898](https://github.com/kubernetes-sigs/lws/issues/898) describes both problems from an inference serving platform running LWS behind an autoscaler. Serving workloads generally do not need stable per-group identity. They need N interchangeable groups, and this hashing system maintains that while allowing ReplicaSet to pick the unhealthy ones as the first to go.

### Goals

1. Let users opt a LeaderWorkerSet into hash-based group identity where groups are interchangeable.
2. Prefer unscheduled and not-ready groups as scale-down victims.
3. Preserve group semantics: atomic group creation and restart, group-by-group rolling updates, startup policies, exclusive placement, and the scale subresource.
4. Let DisaggregatedSet roles opt into hash identity through their inline LeaderWorkerSet spec, including migrating an existing role from Ordinal to Hash as a rolling update.
5. Change nothing for existing LeaderWorkerSets and DisaggregatedSets.

### Non-Goals

1. Changing the default identity mode, now or later. Flipping the default to `Hash` would change the behavior of existing objects and manifests within the v1 API, so `Ordinal` remains the default. Adoption is driven by recommending `Hash` for serving workloads in the documentation and using it in the project guides (work for future doc update PR).
2. Migrating a live LeaderWorkerSet between modes. The field is immutable.
3. Supporting every ordinal-dependent feature in hash mode. Volume claim templates and rolling update partitions are rejected at admission (see [Unsupported Combinations](#unsupported-combinations)).
4. A user-facing API for naming specific scale-down victims, for example via `pod-deletion-cost` (see [Alternatives](#alternatives) below).

## Proposal

Add `spec.groupIdentity` with values `Ordinal` and `Hash`, defaulted to `Ordinal` by the webhook and immutable after creation.

In hash mode the controller manages leader pods through a Deployment named after the LeaderWorkerSet. Each new leader pod is assigned a random 40 character group key at admission, stored in both the `group-index` and `group-key` labels. The webhook also sets the leader's hostname to the LeaderWorkerSet name plus an 8 character prefix of the group key, while the Deployment template sets its subdomain to the LeaderWorkerSet headless service, giving each leader a DNS name for its lifetime. The per-group worker StatefulSet is named after its leader pod, and workers reach their leader through that DNS name in the `LWS_LEADER_ADDRESS` environment variable, the same form as ordinal mode.

Scale down is delegated to the ReplicaSet, which deletes unscheduled and not-ready pods before healthy ones. Since a leader pod is only fully ready once its whole group is ready (via a readiness gate, described below), the ReplicaSet's ranking operates on group health.

### User Stories

#### Story 1: Scale down without collateral churn

An inference platform runs 4 groups and one dies with its node. The autoscaler scales to 3. In hash mode the dead group is the victim and the healthy groups are untouched. In ordinal mode the same sequence deletes a healthy group and rebuilds the dead one.

#### Story 2: Relocate one specific group

An operator wants to move one group off a node, for example ahead of maintenance. Deleting that group's leader pod recreates the group under a fresh identity wherever the scheduler places it, and no other group is affected.

#### Story 3: Autoscale serving groups with HPA

An HPA drives `spec.replicas` on a hash mode LeaderWorkerSet through the scale subresource. Scale down during partial outages removes the groups that are already broken.

### Notes/Constraints/Caveats

1. Group identity is not stable. A recreated group gets a new key, so anything keyed on group identity must treat it as ephemeral.
2. The `group-index` label carries a 40 character hash in hash mode. Tooling that parses it as an integer will not work on hash mode pods.
3. The leader's DNS name is derived from the group key, so a recreated group gets a new name. Consumers must not cache it across group replacement.
4. Victim ranking runs on API server state. If a node fails and is replaced under the same name, its pods can report a stale `Running` phase for a short window and ranking sees that state until the kubelet reports in.
5. `NoneRestartPolicy` keeps its meaning for worker failures only (the worker is recreated alone and rejoins its group). Losing the leader always replaces the whole group under a fresh identity, because the group's identity is the leader pod. Workers cannot survive their leader in hash mode.

### Risks and Mitigations

1. **Two leader management paths.** The controller now reconciles leaders through either a StatefulSet or a Deployment. The group-level machinery (pod webhook, worker StatefulSet reconciliation, revisions) is shared and only leader ownership differs. Both modes run in the e2e suite.
2. **Consumers assuming numeric group indices.** The field is opt-in and immutable, and the label semantics are documented.
3. **Controller downgrade with hash workloads present.** An older controller does not know the field, treats the LWS as Ordinal, and creates a leader StatefulSet alongside the existing Deployment. This cannot be fixed in already-shipped versions, so downgrading with hash mode workloads present is unsupported and documented as such: delete or migrate them first.

## Design Details

### API

```golang
type LeaderWorkerSetSpec struct {
    ...
    // GroupIdentity controls how group identities are assigned.
    // Ordinal (default) names groups by leader StatefulSet ordinals.
    // Hash names groups by random keys and manages leaders with a Deployment.
    // Immutable post creation.
    // +optional
    GroupIdentity GroupIdentityType `json:"groupIdentity,omitempty"`

    // GroupReplacementPolicy controls when a replacement group may start
    // scheduling after a group is deleted.
    // PostTermination (default) admits a replacement only once a previously
    // deleted group has been fully removed.
    // Immediate admits replacements as soon as they are created. Only
    // supported with groupIdentity Hash.
    // +optional
    GroupReplacementPolicy GroupReplacementPolicyType `json:"groupReplacementPolicy,omitempty"`
}

type GroupIdentityType string

const (
    GroupIdentityOrdinal GroupIdentityType = "Ordinal"
    GroupIdentityHash    GroupIdentityType = "Hash"
)

type GroupReplacementPolicyType string

const (
    GroupReplacementImmediate       GroupReplacementPolicyType = "Immediate"
    GroupReplacementPostTermination GroupReplacementPolicyType = "PostTermination"
)
```

The webhook defaults an empty `groupIdentity` to `Ordinal` and rejects updates to it. It defaults an empty `groupReplacementPolicy` to `PostTermination` and rejects `Immediate` with `groupIdentity: Ordinal`, where the StatefulSet always waits for the previous leader pod to be gone. The field is mutable and is not part of the revision, so changing it does not roll the groups.

### Leader Deployment

In hash mode, the controller owns a Deployment (named after the LeaderWorkerSet) instead of a leader StatefulSet. `maxSurge` and `maxUnavailable` from the LWS rolling update configuration map directly onto the Deployment strategy.

Leader pods carry a `leaderworkerset.sigs.k8s.io/group-ready` readiness gate. The pod controller sets the condition to true once the group's worker StatefulSet is ready, so a leader counts as ready only when its whole group is. This makes the Deployment's availability budget count groups rather than bare leader pods, which is what paces rollouts group by group and what makes the ReplicaSet prefer broken groups at scale down.

### Group Identity Assignment

The pod webhook assigns each new leader pod a group key, a SHA1 over the namespace and a random 16 character string. A random input is required because leader pods are created through `generateName`, so at mutating admission time the pod has no name or UID to derive a key from. The key is stored in both the `group-index` and `group-key` labels on the leader and inherited by its workers. Exclusive placement continues to key off `group-key` exactly as in ordinal mode.

The webhook also sets the leader's `hostname` to the LeaderWorkerSet name plus an 8 character prefix of the group key, while the Deployment template sets its `subdomain` to the LeaderWorkerSet headless service, which gives each leader a per-pod DNS record under the service. The key is truncated because the full 40 characters would consume most of the 63 character service name budget. Under `subdomainPolicy: UniquePerReplica` the per-replica headless service name matches the leader host name.

### Worker StatefulSets and Leader Address

Each group's worker StatefulSet is named after its leader pod, as today. The hostname and subdomain assigned at admission give each leader a DNS record under the LeaderWorkerSet headless service, which is created with `publishNotReadyAddresses` so the record resolves while the readiness gate holds the leader not-ready. Workers receive this DNS name through the `leaderworkerset.sigs.k8s.io/leader-address` annotation, surfaced to containers as the `LWS_LEADER_ADDRESS` environment variable, matching the form ordinal mode provides. Groups of size 1 get no readiness gate and no worker StatefulSet.

### Rollouts

Template changes produce a controller revision in both modes. The Deployment performs the rollout, replacing groups within the `maxSurge` and `maxUnavailable` budgets, and the readiness gate holds each step until the replacement group is fully ready. Startup policies behave as in ordinal mode, including `LeaderReady`, where the worker StatefulSet is not created until the leader's containers are ready.

### Group Replacement

In ordinal mode a replacement group cannot start before the group it replaces is gone: the leader pod is deleted with foreground propagation, so it outlives its worker StatefulSet and worker pods, and the leader StatefulSet cannot recreate a pod of the same name until the old object is removed. The replacement therefore lands on the capacity the old group released. A ReplicaSet has no such constraint. It creates the replacement leader as soon as the old one starts terminating, the pod controller creates its worker StatefulSet, and the new pods go through scheduling while the old group still holds its hardware. On clusters where pending pods trigger preemption, the replacement group evicts other workloads instead of waiting a few seconds for its predecessor's capacity.

`groupReplacementPolicy: PostTermination`, the default, restores the ordinal sequencing in hash mode with a scheduling gate:

1. The pod webhook adds the `leaderworkerset.sigs.k8s.io/group-replacement` scheduling gate to every hash mode leader pod at admission. The scheduler ignores gated pods entirely, so a gated leader triggers no preemption.
2. While the leader is gated, the pod controller creates nothing for its group: no worker StatefulSet, no per-replica service, no pod group.
3. The pod controller lifts the gate according to a per LeaderWorkerSet count. With `T` leader pods still terminating and `G` gated leaders ordered by creation time, the oldest `G - T` gated leaders are admitted. One fully removed group admits one replacement, and gated leaders that do not correspond to a terminating group, such as a scale up or a `maxSurge` pod, are admitted immediately.
4. A leader pod deletion event enqueues the gated leaders of the same LeaderWorkerSet, and gated leaders requeue every 10 seconds as a fallback.

The count uses the existence of the terminating leader pod object rather than its phase. Because the leader is foreground deleted, its object disappears only after the worker StatefulSet and every worker pod object are gone, and a worker pod object is removed only after the kubelet has reclaimed its resources, which is also when the scheduler counts that node capacity as free. Waiting on the leader's terminal phase instead, as the upstream Deployment `podReplacementPolicy: TerminationComplete` proposal (KEP-5882, not merged) would, is not sufficient: in a measured recreate, the leader reached `Succeeded` about one second after deletion while its workers held their capacity for another 29 seconds until their grace period expired.

Hash mode cannot tie a replacement to a specific predecessor, since ReplicaSet pod names are generated and the worker StatefulSet is named after its leader. The count based rule gives the same capacity accounting as ordinal mode without name matching. Two visible differences remain: the waiting replacement exists as a `SchedulingGated` pod instead of not existing, and `status.replicas` counts it as a not ready group. `Immediate` opts out of the wait and reproduces plain ReplicaSet behavior; leaders are still gated at admission and ungated on their first reconcile, so the policy can be switched without rolling the groups.

### Scale Subresource and HPA

`kubectl scale` and the scale subresource work unchanged. `status.hpaPodSelector` selects pods by LWS name and `worker-index=0`, which matches exactly the leader pods in both modes.

### DisaggregatedSet Integration

DisaggregatedSet roles embed the full LeaderWorkerSet spec, so a role sets `groupIdentity: Hash` directly in its template and the controller passes it through to the LeaderWorkerSets it creates:

1. The DisaggregatedSet CRD schema is regenerated to include the field. Without this the API server prunes it from role templates silently.
2. The DisaggregatedSet webhook runs the same hash-mode validation per role, so an unsupported combination fails at DisaggregatedSet admission instead of surfacing later as LeaderWorkerSet creation failures in a reconcile loop.
3. The DisaggregatedSet revision hash includes the field, normalized so an empty value and the CRD default `Ordinal` hash identically. Objects persisted before the field existed keep their revision when the new CRD starts defaulting it, so upgrading the controller does not roll existing DisaggregatedSets.

Because the revision includes the field and DisaggregatedSet rolls template changes by replacing whole LeaderWorkerSets, changing a role from `Ordinal` to `Hash` is a normal rolling update rather than a forbidden in-place mutation. NOTE: the DisaggregatedSet revision covers all roles jointly, so changing one role's identity mode rolls the whole slice, the same as any other role template change.

### Unsupported Combinations

Validation rejects hash mode combined with features whose semantics depend on stable StatefulSet identity:

- volume claim templates: persistent storage exists to be reattached to a successor with the same identity, and hash mode never reuses identities, so there is nothing to reattach to. Per-group scratch storage is already covered upstream by generic ephemeral volumes.
- `rollingUpdateConfiguration.partition`: partition is defined over ordinals and Deployments have no equivalent rollout control.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- Shared test wrappers must set `groupIdentity` explicitly, since objects fetched from the API server now carry the defaulted field.

#### Unit tests

- Webhook defaulting and validation: default to `Ordinal`, immutability, rejected feature combinations.
- Leader Deployment construction: selector, strategy mapping, readiness gate injection.
- Group-ready condition sync in the pod controller.
- Leader address annotation and environment variable injection.
- Hostname and subdomain assignment on hash mode leaders, and group key derivation for subgroup hashes and per-replica service names.
- DisaggregatedSet: webhook rejection of hash-mode combinations per role, revision stability between empty and `Ordinal`, revision change on `Hash`, and spec passthrough to created LeaderWorkerSets.
- Group replacement: gate injection on hash leaders only, `PostTermination` defaulting, `Immediate` rejected with `Ordinal`, and the admission count (no terminating groups, one terminating group holding one gated leader, oldest gated leader admitted first, `Immediate` ignoring terminating groups).

#### Integration tests

- Defaulting and validation through the running webhook.
- Group replacement: a gated leader stays gated while a terminating leader of the same LeaderWorkerSet exists and no worker StatefulSet is created for it; removing the terminating leader lifts the gate and creates the worker StatefulSet.
- A hash mode LWS creates a leader Deployment and one worker StatefulSet per group.
- Readiness gate lifecycle: false while workers are pending, true when the group is ready.
- Scale up and scale down, including to zero and back.
- Rolling updates respect `maxSurge` and `maxUnavailable` in units of groups.
- Size 1 groups: no gate, no worker StatefulSet.
- Leaders resolve by DNS name, and `subGroupPolicy` and `UniquePerReplica` work in hash mode with group key derived values.
- A `groupIdentity: Hash` DisaggregatedSet role survives the CRD schema and is rejected when combined with volume claim templates.

#### e2e tests

- Hash mode lifecycle: create, scale, rolling update, group failure recovery.
- Group failure recovery under `PostTermination`: the replacement leader stays `SchedulingGated` until the old leader pod is gone, then schedules and gets its worker StatefulSet.
- Scale down with an unhealthy group present removes the unhealthy group.
- `LeaderReady` startup policy in hash mode.
- Exclusive placement in hash mode.
- Upgrade from a release without the field to one with it, with ordinal workloads running across the upgrade.
- DisaggregatedSet with hash-mode roles: creation, scale down with an unhealthy group present, and migration of a role from `Ordinal` to `Hash`.

### Graduation Criteria

Alpha: field implemented behind the `Ordinal` default, hash mode covered by the tests above.

Beta: feedback, make `Hash` recommended for serving workloads in the documentation and example manifests.

## Implementation History

- 2026-08-17: KEP drafted after a working prototype was built and tested.
- 2026-08-18: DisaggregatedSet integration added to the prototype.
- 2026-08-24: Review updates. Leaders get DNS names derived from the group key, and `subGroupPolicy` and `UniquePerReplica` move from rejected to supported through group key derivation.
- 2026-09-17: `groupReplacementPolicy` added after review feedback that replacement groups preempted running workloads while the old group was still releasing capacity.

## Drawbacks

1. A second leader management path to maintain, even with the group machinery shared.
2. Hash labels are less readable than ordinals in `kubectl` output and logs.
3. Workloads that rely on stable identity or stable DNS names cannot use hash mode, which is why it is opt-in.

## Alternatives

### Alternative 1: Smarter victim selection in the leader StatefulSet

The StatefulSet API only removes the highest ordinal. There is no victim selection hook, and `pod-deletion-cost` is a ReplicaSet concept. Getting this upstream into StatefulSet would be a much larger change outside this project's control.

### Alternative 2: Controller-managed bare leader pods

LWS could create and delete leader pods itself and rank victims in its own code. This reimplements what ReplicaSets already do well: victim ranking, surge handling, and availability accounting during rollouts. Delegating to a Deployment keeps that logic out of LWS.

### Alternative 3: Solve it in consumers

Higher level controllers could delete unhealthy groups themselves before scaling down. Every consumer would need to rebuild the same logic, direct users of LWS would get nothing, and there is an unavoidable race between the consumer's cleanup and the StatefulSet's ordinal scale down.
