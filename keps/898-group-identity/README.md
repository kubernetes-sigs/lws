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

With hash identity, scale down removes unscheduled and not-ready groups before healthy ones, because that is how the ReplicaSet ranks its victims. Everything else about a group stays the same: workers still run in a per-group StatefulSet, rolling updates still move group by group within the `maxSurge` and `maxUnavailable` budgets, and startup policies and exclusive placement work as they do today.

## Motivation

Group identity in LWS is currently an ordinal, so the set of live groups is always `0..replicas-1`. Scale down can only remove the highest ordinal. When some other group is unhealthy, for example because its node died, scaling down removes a healthy group and then rebuilds the unhealthy one. The user wanted one group gone and got two disruptions.

The same coupling makes it impossible to remove or relocate one specific group. Issue [#898](https://github.com/kubernetes-sigs/lws/issues/898) describes both problems from an inference serving platform running LWS behind an autoscaler.

Serving workloads generally do not need stable per-group identity. They need N interchangeable groups, and they want the unhealthy ones to be the first to go.

### Goals

1. Let users opt a LeaderWorkerSet into hash-based group identity where groups are interchangeable.
2. Prefer unscheduled and not-ready groups as scale-down victims.
3. Preserve group semantics: atomic group creation and restart, group-by-group rolling updates, startup policies, exclusive placement, and the scale subresource.
4. Let DisaggregatedSet roles opt into hash identity through their inline LeaderWorkerSet spec, including migrating an existing role from Ordinal to Hash as a rolling update.
5. Change nothing for existing LeaderWorkerSets and DisaggregatedSets. `Ordinal` remains the default.

### Non-Goals

1. Changing the default identity mode, now or later. Flipping the default to `Hash` would change the behavior of existing objects and manifests within the v1 API, so `Ordinal` remains the default permanently. Adoption is driven by recommending `Hash` for serving workloads in the documentation and using it in the project's guides and examples.
2. Migrating a live LeaderWorkerSet between modes. The field is immutable.
3. Supporting every ordinal-dependent feature in hash mode. Subgroups, volume claim templates, `subdomainPolicy: UniquePerReplica`, and rolling update partitions are rejected at admission (see [Unsupported Combinations](#unsupported-combinations)).
4. A user-facing API for naming specific scale-down victims.

## Proposal

Add `spec.groupIdentity` with values `Ordinal` and `Hash`, defaulted to `Ordinal` by the webhook and immutable after creation.

In hash mode the controller manages leader pods through a Deployment named after the LeaderWorkerSet. Each new leader pod is assigned a random 40 character group key at admission, stored in both the `group-index` and `group-key` labels. The per-group worker StatefulSet is named after its leader pod, and workers reach their leader by pod IP through the `LWS_LEADER_ADDRESS` environment variable rather than a stable DNS name.

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
3. There is no per-group stable hostname. Workers get the leader's pod IP, which changes when the leader is recreated.
4. Victim ranking runs on API server state. If a node fails and is replaced under the same name, its pods can report a stale `Running` phase for a short window and ranking sees that state until the kubelet reports in.

### Risks and Mitigations

1. **Two leader management paths.** The controller now reconciles leaders through either a StatefulSet or a Deployment. The group-level machinery (pod webhook, worker StatefulSet reconciliation, revisions) is shared and only leader ownership differs. Both modes run in the e2e suite.
2. **Consumers assuming numeric group indices.** The field is opt-in and immutable, and the label semantics are documented. Nothing changes unless a user asks for hash mode.
3. **Admission accepts templates the Deployment API rejects.** The LWS webhook does not fully validate pod templates, so an invalid template surfaces as reconcile errors rather than an admission failure. Ordinal mode has the same exposure through the StatefulSet API. A dry-run template validation at admission is a possible follow-up for both modes.

## Design Details

### API

```golang
type LeaderWorkerSetSpec struct {
    ...
    // GroupIdentity controls how group identities are assigned.
    // Ordinal (default) names groups by leader StatefulSet ordinals.
    // Hash names groups by random keys and manages leaders with a Deployment.
    // Immutable after creation.
    // +optional
    GroupIdentity GroupIdentityType `json:"groupIdentity,omitempty"`
}

type GroupIdentityType string

const (
    GroupIdentityOrdinal GroupIdentityType = "Ordinal"
    GroupIdentityHash    GroupIdentityType = "Hash"
)
```

The webhook defaults an empty value to `Ordinal` and rejects updates that change the field.

### Leader Deployment

In hash mode the controller owns a Deployment (named after the LeaderWorkerSet) instead of a leader StatefulSet. `maxSurge` and `maxUnavailable` from the LWS rolling update configuration map directly onto the Deployment strategy.

Leader pods carry a `leaderworkerset.sigs.k8s.io/group-ready` readiness gate. The pod controller sets the condition to true once the group's worker StatefulSet is ready, so a leader counts as ready only when its whole group is. This makes the Deployment's availability budget count groups rather than bare leader pods, which is what paces rollouts group by group and what makes the ReplicaSet prefer broken groups at scale down.

### Group Identity Assignment

The pod webhook assigns each new leader pod a group key, a SHA1 over the namespace and a random 16 character string. The key is stored in both the `group-index` and `group-key` labels on the leader and inherited by its workers. Exclusive placement continues to key off `group-key` exactly as in ordinal mode.

### Worker StatefulSets and Leader Address

Each group's worker StatefulSet is named after its leader pod, as today. Since hash-named leaders have no per-pod DNS record, the controller stamps the leader's pod IP onto worker pods through a `leaderworkerset.sigs.k8s.io/leader-address` annotation, surfaced to containers as the `LWS_LEADER_ADDRESS` environment variable. On the leader itself the variable resolves to its own pod IP through the downward API. Groups of size 1 get no readiness gate and no worker StatefulSet.

### Rollouts

Template changes produce a controller revision as in ordinal mode. The Deployment performs the rollout, replacing groups within the `maxSurge` and `maxUnavailable` budgets, and the readiness gate holds each step until the replacement group is fully ready. Startup policies behave as in ordinal mode, including `LeaderReady`, where the worker StatefulSet is not created until the leader's containers are ready.

### Scale Subresource and HPA

`kubectl scale` and the scale subresource work unchanged. `status.hpaPodSelector` selects pods by LWS name and `worker-index=0`, which matches exactly the leader pods in both modes.

### DisaggregatedSet Integration

DisaggregatedSet roles embed the full LeaderWorkerSet spec, so a role sets `groupIdentity: Hash` directly in its template and the controller passes it through to the LeaderWorkerSets it creates. Three pieces make this safe:

1. The DisaggregatedSet CRD schema is regenerated to include the field. Without this the API server prunes it from role templates silently.
2. The DisaggregatedSet webhook runs the same hash-mode validation per role, so an unsupported combination fails at DisaggregatedSet admission instead of surfacing later as LeaderWorkerSet creation failures in a reconcile loop.
3. The DisaggregatedSet revision hash includes the field, normalized so an empty value and the CRD default `Ordinal` hash identically. Objects persisted before the field existed keep their revision when the new CRD starts defaulting it, so upgrading the controller does not roll existing DisaggregatedSets.

Because the revision includes the field and DisaggregatedSet rolls template changes by replacing whole LeaderWorkerSets, changing a role from `Ordinal` to `Hash` is a normal rolling update rather than a forbidden in-place mutation. Note that the DisaggregatedSet revision covers all roles jointly, so changing one role's identity mode rolls the whole slice, the same as any other role template change. Role services select pods by DisaggregatedSet labels, which are identical in both modes.

### Unsupported Combinations

Validation rejects hash mode combined with features whose semantics depend on stable ordinals:

- `subGroupPolicy`
- volume claim templates
- `networkConfig.subdomainPolicy: UniquePerReplica`
- `rollingUpdateConfiguration.partition`

These can be revisited individually if there is demand.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- Shared test wrappers must set `groupIdentity` explicitly, since objects fetched from the API server now carry the defaulted field.

#### Unit tests

- Webhook defaulting and validation: default to `Ordinal`, immutability, rejected feature combinations.
- Leader Deployment construction: selector, strategy mapping, readiness gate injection.
- Group-ready condition sync in the pod controller.
- Leader address annotation and environment variable injection.
- DisaggregatedSet: webhook rejection of hash-mode combinations per role, revision stability between empty and `Ordinal`, revision change on `Hash`, and spec passthrough to created LeaderWorkerSets.

#### Integration tests

- Defaulting and validation through the running webhook.
- A hash mode LWS creates a leader Deployment and one worker StatefulSet per group.
- Readiness gate lifecycle: false while workers are pending, true when the group is ready.
- Scale up and scale down, including to zero and back.
- Rolling updates respect `maxSurge` and `maxUnavailable` in units of groups.
- Size 1 groups: no gate, no worker StatefulSet.
- A `groupIdentity: Hash` DisaggregatedSet role survives the CRD schema and is rejected when combined with `subGroupPolicy`.

#### e2e tests

- Hash mode lifecycle: create, scale, rolling update, group failure recovery.
- Scale down with an unhealthy group present removes the unhealthy group.
- `LeaderReady` startup policy in hash mode.
- Exclusive placement in hash mode.
- Upgrade from a release without the field to one with it, with ordinal workloads running across the upgrade.
- DisaggregatedSet with hash-mode roles: creation, scale down with an unhealthy group present, and migration of a role from `Ordinal` to `Hash`.

### Graduation Criteria

Alpha: field implemented behind the `Ordinal` default, hash mode covered by the tests above.

Beta: at least one release of user feedback, a decision on webhook template validation, and `Hash` recommended for serving workloads in the documentation and example manifests. The default does not change at any stage (see Non-Goals).

## Implementation History

- 2026-08-17: KEP drafted. A working prototype was built and validated first, including regression suites, node failure testing, and scale testing. Results are linked from issue #898.
- 2026-08-18: DisaggregatedSet integration added to the prototype and validated live, including upgrade stability for existing objects, victim selection through a DisaggregatedSet scale down, and a rolling Ordinal to Hash migration.

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
