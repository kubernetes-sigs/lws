# KEP-893: DisaggregatedSet Integration with CompositePodGroup

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Admit a slice as a unit](#story-1-admit-a-slice-as-a-unit)
    - [Story 2: Per-role placement](#story-2-per-role-placement)
    - [Story 3: Roll out without deadlocking](#story-3-roll-out-without-deadlocking)
    - [Story 4: Per-role gang without CompositePodGroup](#story-4-per-role-gang-without-compositepodgroup)
  - [Scheduling Hierarchy](#scheduling-hierarchy)
  - [One Root per Slice and Revision](#one-root-per-slice-and-revision)
  - [Workload and PodGroup Without CompositePodGroup](#workload-and-podgroup-without-compositepodgroup)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
  - [Validation](#validation)
  - [Generated Objects](#generated-objects)
  - [Delegation to the Child LWS](#delegation-to-the-child-lws)
  - [Role and Slice Minimums](#role-and-slice-minimums)
  - [Rollouts](#rollouts)
  - [Interaction with Placement Policy](#interaction-with-placement-policy)
  - [Lifecycle and Garbage Collection](#lifecycle-and-garbage-collection)
  - [Observability](#observability)
  - [Feature Gate, Upgrade and Downgrade](#feature-gate-upgrade-and-downgrade)
  - [Example](#example)
  - [Open Questions](#open-questions)
  - [Test Plan](#test-plan)
    - [Prerequisite testing updates](#prerequisite-testing-updates)
    - [Unit Tests](#unit-tests)
    - [Integration Tests](#integration-tests)
    - [End-to-End Tests](#end-to-end-tests)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

This KEP describes how [DisaggregatedSet][kep766] uses the upstream `CompositePodGroup` (CPG) API ([KEP-6012][kep6012]) so that each slice is gang-scheduled as one hierarchy: the slice, its roles, each replica of a role, and the leader and worker pods of that replica.

A DisaggregatedSet already owns one LeaderWorkerSet (LWS) per role, slice, and revision. [KEP-666][kep666] compiles a single LWS into a Workload and PodGroups, and leaves the role and slice levels to a future DisaggregatedSet root controller. This KEP is that follow-up. It adds a `spec.scheduling` field to DisaggregatedSet, defines the Workload and CPG objects the controller creates from it, and describes how each child LWS attaches its replicas underneath.

The design is provisional. `CompositePodGroup` is alpha and off by default in Kubernetes 1.37 and targets Beta in 1.38. Several parts of this KEP (changing a role minimum, scaling a role to zero) depend on the Beta behavior where `minGroupCount` becomes mutable, so the KEP targets the Beta API and describes what is and is not supported on 1.37.

## Motivation

Today a disaggregated deployment gets per-role gang scheduling at best: each child LWS can set `spec.scheduling` and get one PodGroup per replica. Nothing ties the roles of a slice together, so the scheduler can admit prefill while decode waits for capacity, or the reverse. The GPUs held by the admitted role cannot serve anything until the other role lands.

That per-role path is still useful on its own and stays supported (see [Story 4](#story-4-per-role-gang-without-compositepodgroup)). What it cannot do is gang roles together, which is the gap this KEP fills.

A flat PodGroup cannot fix this. A flat gang across a slice would need prefill and decode pods in one group, which two separate LWS objects cannot share, and it would lose the per-replica gang and per-role topology that disaggregated serving needs. `CompositePodGroup` was built for this case. KEP-6012 lists "disaggregated serving under LeaderWorkerSet" as one of its user stories.

### Goals

1. Define the CPG hierarchy for one slice: slice root, one CPG per role, one CPG per replica, and the leader and worker PodGroups.
2. Add an optional `spec.scheduling` field to DisaggregatedSet that compiles to that hierarchy. Users never write Workload or CPG objects by hand.
3. Keep DisaggregatedSet as the root controller that owns the Workload, and delegate each role's replicas to the child LWS through the KEP-6089 linkage annotations.
4. Keep the KEP-846 rule that changing `slices` never triggers a rollout, and make sure rollouts and External scaling cannot deadlock on a gang.

### Non-Goals

1. Weighted prefill-to-decode ratios. `minGroupCount` counts child groups and has no weights, so it cannot say "two prefill per decode".
2. Changing the slice or rollout model from [KEP-766][kep766], [KEP-846][kep846], or [KEP-1022][kep1022].
3. Bring-your-own Workload or CPG objects.
4. Defining the single-LWS `spec.scheduling` API. That belongs to KEP-666 and is reused here.
5. A DisaggregatedSet-owned Workload with flat PodGroups only. See [Workload and PodGroup Without CompositePodGroup](#workload-and-podgroup-without-compositepodgroup).
6. Scheduler providers other than the upstream `kubernetes` provider.

## Proposal

When `spec.scheduling` is set, the DisaggregatedSet controller creates one Workload per revision and one CPG tree per slice and revision. It then sets three annotations on each child LWS so the LWS creates its replica CPGs and leader/worker PodGroups under the right role CPG. When `spec.scheduling` is not set, nothing changes.

### User Stories

#### Story 1: Admit a slice as a unit

An operator runs vLLM with two prefill replicas and one decode replica per slice. They want the scheduler to admit a slice only when both roles fit in one zone, so a slice never holds prefill GPUs while decode is stuck Pending.

#### Story 2: Per-role placement

Prefill replicas need their workers inside one NVLink domain, and all prefill replicas of a slice should sit in the same block. Decode has looser needs. The operator wants to write those constraints once per role and have every slice pick them up.

#### Story 3: Roll out without deadlocking

The operator changes the pod template. The rollout planner from KEP-766 grows the new revision one replica at a time. The gang settings must let those first new replicas schedule, instead of waiting for a minimum the new revision has not reached yet.

#### Story 4: Per-role gang without CompositePodGroup

An operator runs on a cluster where `CompositePodGroup` is not enabled, or does not need prefill and decode admitted together. They want each multi-node replica gang-scheduled so a replica never holds a partial set of GPUs. They set `roles[].spec.scheduling`, and each child LWS creates its own Workload and PodGroups. This works today with no change from this KEP. See [Workload and PodGroup Without CompositePodGroup](#workload-and-podgroup-without-compositepodgroup).

### Scheduling Hierarchy

One slice of one revision compiles to a four-level tree. Composite levels use `minGroupCount`, which counts child groups. Leaf PodGroups use `minCount`, which counts pods.

```text
CompositePodGroup  ds-0-rev1                     gang(minGroupCount=2)  topology=zone   # slice root
├── CompositePodGroup  ds-0-rev1-prefill         gang(minGroupCount=2)  topology=block  # role
│   ├── CompositePodGroup  <replica 0>           gang(minGroupCount=2)  topology=rack   # replica
│   │   ├── PodGroup  <replica 0 leader>         gang(minCount=1)
│   │   └── PodGroup  <replica 0 workers>        gang(minCount=size-1)  topology=nvlink
│   └── CompositePodGroup  <replica 1>           ...
└── CompositePodGroup  ds-0-rev1-decode          gang(minGroupCount=1)  topology=block
    └── CompositePodGroup  <replica 0>           ...
```

- The slice root has one child per role. Its `minGroupCount` is the number of roles that have at least one replica in this revision, so every running role must be admitted together.
- A role CPG has one child per replica of that role. Its `minGroupCount` is the role minimum (see [Role and Slice Minimums](#role-and-slice-minimums)).
- A replica CPG and its leaves are the KEP-666 Phase 2 shape for one replica. The child LWS creates them. When `size` is 1 there are no workers and the worker leaf is omitted, as in KEP-666.

Four levels is the most the API allows (`WorkloadMaxTreeDepth = 4`), so there is no room for an extra LWS root between the role CPG and its replicas. The role CPG takes the place of the LWS root CPG from the KEP-666 sketch.

[Sub-roles][kep960] do not add a level. Sub-roles are routing pools inside one physical LWS per role, slice, and revision, so all of a role's replicas, across every sub-role, sit under the same role CPG.

### One Root per Slice and Revision

Each slice and revision gets its own root CPG. Slices share one configuration, not one object. This follows the direction agreed in [#894][api894] and keeps two DisaggregatedSet rules intact:

1. Changing `slices` adds or removes roots. It never edits an existing root, so it never triggers a rollout (KEP-846).
2. Each slice stays its own rollout and disruption domain. During a rollout the old and new revisions of a slice have separate roots, so the new revision's gang never depends on pods from the old one.

### Workload and PodGroup Without CompositePodGroup

DisaggregatedSet already gets the KEP-666 Phase 1 path with no new API. `roles[].spec` embeds `LeaderWorkerSetSpec`, so `roles[].spec.scheduling` is copied to each child LWS. The child gets no `group-template-name` annotation, so it acts as its own root: it creates one `v1beta1` Workload and its per-replica (or whole-LWS) PodGroups. Each role, slice, and revision therefore gets per-replica gang scheduling today, on the beta `GenericWorkload` path.

A DisaggregatedSet-owned Workload with only flat PodGroups would not add the one thing this KEP is for, which is admitting a slice's roles together:

- A cross-role gang needs prefill and decode pods in one group. In KEP-666 delegated flat mode each child LWS creates its own groups from one template, so two LWS cannot join a shared PodGroup. Supporting that would need a new linkage that the CPG path then replaces.
- One flat group for the whole slice would drop the per-replica gang and the per-role topology that the per-role path already gives.

So there is no flat DisaggregatedSet phase. Per-role pass-through stays the supported path without CPG. Once `spec.scheduling` is set, the two must not be mixed: the webhook rejects `roles[].spec.scheduling` on the same object (see [Validation](#validation)).

### Notes/Constraints/Caveats

- The upstream API caps each template list at 8 entries. The slice root has one child template per role, so a DisaggregatedSet that sets `spec.scheduling` can have at most 8 roles, even though `spec.roles` allows 10.
- kube-scheduler rejects a hierarchy whose groups have different priorities or preemption policies. Every role in a slice must therefore resolve to the same priority.
- kube-scheduler also rejects a `gang` parent with a `basic` child, and an `all` disruption parent with a `single` child. The webhook checks these combinations up front so users do not find out from a Pending pod.
- Upstream schedules sibling groups greedily. When a role minimum is lower than the role's replica count, an early replica can take capacity a later one needs. KEP-6012 accepts this because the common case requires every child. The default role minimum here follows that and requires every replica.
- `startupPolicy: LeaderReady` cannot be combined with a gang that holds both leader and worker pods (KEP-666 non-goal), so roles using it are rejected when `spec.scheduling` is set.
- LWS-managed templates must not pre-set `pod.spec.schedulingGroup`, same as KEP-666.

### Risks and Mitigations

**Risk**: The upstream API changes before Beta, or Beta slips past 1.38.

**Mitigation**: The DisaggregatedSet fields embed the same upstream building blocks KEP-666 uses, not raw CPG objects, and the whole feature is gated. On 1.37 the controller only uses the parts that work with an immutable `minGroupCount`.

**Risk**: A gang that can never be satisfied leaves a slice Pending forever, and the rollout planner waits on it.

**Mitigation**: The minimums are derived from the replicas that actually exist in the revision (see [Role and Slice Minimums](#role-and-slice-minimums)), and the planner treats a slice root that reports `Unschedulable` the same way it treats scheduler-rejected pods today (see [Rollouts](#rollouts)).

**Risk**: An earlier attempt at this KEP (#920) was closed as premature.

**Mitigation**: The two conditions from that discussion, KEP-666 and the KEP-6012 Beta update, are now merged. This KEP stays provisional and gated.

## Design Details

### API

DisaggregatedSet gets one optional field. The role and replica blocks reuse the KEP-666 types, so a user who knows LWS scheduling already knows most of this.

```go
type DisaggregatedSetSpec struct {
    // ... existing fields ...

    // Scheduling configures workload-aware scheduling for every slice.
    // When set, roles[].spec.scheduling must not be set.
    // +optional
    Scheduling *DisaggregatedSetScheduling `json:"scheduling,omitempty"`
}

type DisaggregatedSetScheduling struct {
    // Slice root. minGroupCount is derived by the controller and must not be set.
    SchedulingPolicy      *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy      `json:"schedulingPolicy,omitempty"`
    SchedulingConstraints *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints `json:"schedulingConstraints,omitempty"`
    DisruptionMode        *schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode        `json:"disruptionMode,omitempty"`

    // Roles holds per-role settings. Each name must match an entry in spec.roles.
    // Roles without an entry get the defaults.
    // +listType=map
    // +listMapKey=name
    // +kubebuilder:validation:MaxItems=8
    // +optional
    Roles []DisaggregatedRoleScheduling `json:"roles,omitempty"`
}

type DisaggregatedRoleScheduling struct {
    // Name of the role in spec.roles.
    Name string `json:"name"`

    // Role level. gang.minGroupCount is the minimum number of complete replicas.
    SchedulingPolicy      *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy      `json:"schedulingPolicy,omitempty"`
    SchedulingConstraints *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints `json:"schedulingConstraints,omitempty"`
    DisruptionMode        *schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode        `json:"disruptionMode,omitempty"`

    // Replica, leader and worker levels. Same type and defaults as KEP-666.
    Replica *leaderworkersetv1.LeaderWorkerSetReplicaScheduling `json:"replica,omitempty"`
}
```

All settings live under `spec.scheduling`. This matches the root-only model that [KEP-6089][kep6089] recommends and that JobSet adopted, and avoids having two places to configure the same role. Role entries are keyed by name rather than selected with a `targetRoles` list, because each role maps to exactly one role CPG. A keyed list cannot target two roles from one entry or give one role two entries.

`spec.scheduling` is part of the revision hash, except for role `minGroupCount` values. Changing topology, disruption, or policy therefore rolls out a new revision with a new Workload, which is how the controller deals with Workload templates being immutable. Changing only a role minimum is applied in place (see [Role and Slice Minimums](#role-and-slice-minimums)).

### Validation

When `spec.scheduling` is set, the webhook rejects:

- `roles[].spec.scheduling` on any role.
- More than 8 roles.
- A `spec.scheduling.roles[].name` that does not match a role.
- An explicit `minGroupCount` on the slice root.
- A `gang` level with a `basic` level below it, or an `all` disruption level with a `single` level below it.
- Roles whose pod templates resolve to different `priorityClassName` values.
- Roles with `startupPolicy: LeaderReady`.
- A scheduler provider other than `kubernetes`.
- A slice root topology key that differs from `spec.placementPolicy.topology` (see [Interaction with Placement Policy](#interaction-with-placement-policy)).
- An explicit role `minGroupCount` when the served CPG API does not allow `minGroupCount` updates (1.37).

### Generated Objects

For each revision the controller creates one Workload, named `<ds>-<revision>` and owned by the DisaggregatedSet. Its template tree is fixed for the life of the revision:

```text
compositePodGroupTemplates:
  slice                       # slice root
    <role>                    # one per role, at most 8
      <role>-replica          # KEP-666 replica template
        <role>-leader         # leaf
        <role>-worker         # leaf, omitted when size is 1
```

Template names are unique within the Workload, as the API requires. All slices of a revision share this Workload, because slices of a revision are identical (KEP-846). Every group in one slice's tree references the same Workload, which kube-scheduler also requires.

For each slice and revision the controller creates the slice root CPG `<ds>-<slice>-<revision>` and one role CPG per role, `<ds>-<slice>-<revision>-<role>`. The role CPG has the same name as the child LWS. They are different kinds, so the names do not clash, and it makes the pairing easy to follow with `kubectl`. Replica CPGs and leaves are created and named by the child LWS.

### Delegation to the Child LWS

KEP-6089 hands work to a child controller through three annotations. The DisaggregatedSet controller sets all three on each child LWS:

- `scheduling.k8s.io/workload-name`: `<ds>-<revision>`.
- `scheduling.k8s.io/group-template-name`: `<role>-replica`.
- `scheduling.k8s.io/parent-compositepodgroup`: the role CPG.

The child LWS creates one replica CPG per replica from the `<role>-replica` template, sets its parent to the role CPG, and creates the leader and worker leaves below it. It does not create a root CPG of its own.

This needs two changes to KEP-666, which should land with or before this KEP's alpha:

1. KEP-666 Phase 2 says `group-template-name` selects the LWS root composite template. It also needs to accept a replica composite template, meaning "create replica CPGs under the given parent, without a root". Without this the tree would be five levels deep and kube-scheduler would reject it.
2. KEP-666 and `pkg/schedulerprovider` only know the template and parent annotations and find the Workload by walking controller owners. KEP-6089 now also defines `scheduling.k8s.io/workload-name`, required whenever `group-template-name` is set. LWS should read it, and fall back to the owner walk only when it is missing.

The annotations are controller-to-controller linkage, not user settings, and the existing KEP-666 rules apply: they are immutable once set, and pods stay blocked until the Workload and the parent CPG exist.

### Role and Slice Minimums

In 1.37 `minGroupCount` is immutable on a running CPG. KEP-6012 makes it mutable in Beta. The controller uses the mutable form when the served API allows it.

**With the Beta API:**

- Slice root: `minGroupCount` is the number of roles with at least one replica in this revision. The API does not allow 0, so a role that scales to 0 has its role CPG removed and the root count lowered, and a role that comes back from 0 gets its role CPG recreated and the count raised. If every role is at 0 there is nothing to schedule and the root is left as is.
- Role: `minGroupCount` defaults to the role's current replica count in this revision. A user can set a lower value. Either way the controller stores `min(configured, current replicas)`, and never less than 1.
- When raising a minimum, the controller creates the new child first and then raises the count. When lowering, it lowers the count first and then removes the child. A minimum is never above the number of children that exist.

These are in-place updates. They do not recreate the tree or trigger a rollout. The Workload template keeps the value it was created with, because `compositePodGroupTemplates` is immutable in 1.37, so the running CPG is the source of truth for the current minimum. If the Beta API also allows updating the template value, the controller updates both. This covers External scaling through [`DisaggregatedSetRoleScaler`][kep849] or HPA and sub-role pools that scale to 0, since all of them change the replica count the minimum follows.

**On 1.37:**

- Role `minGroupCount` is 1 and cannot be set by the user.
- Slice root `minGroupCount` is the number of roles when the root is created.
- A role that scales to 0 leaves its slice unadmittable until it comes back. The controller reports this (see [Observability](#observability)) but cannot fix it without the Beta API.

Leaf `minCount` is already mutable in 1.37, so a change to a role's `size` updates the leader and worker leaves in place as described in KEP-666.

### Rollouts

The KEP-766 planner grows the new revision a replica at a time, can create a single bootstrap replica for a role, and already detects target pods that the scheduler keeps marking Unschedulable (`targetUnschedulableRoles`). Because each revision has its own root and its minimums follow the replicas that exist, the first new replicas can always be admitted.

A 2P/1D template change with the default `maxSurge: 0` and `maxUnavailable: 1` in one slice, with the Beta API:

1. The planner creates one new prefill replica. The controller creates the new revision's root with one role child (prefill) and `minGroupCount: 1`, and the prefill role CPG with `minGroupCount: 1`. The replica is admitted on its own.
2. The planner adds the decode replica. The controller creates the decode role CPG and then raises the root to 2.
3. The planner adds the second prefill replica. The controller raises the prefill role minimum to 2, or to the configured value if it is lower.
4. The old revision drains, and its tree is removed as described in [Lifecycle and Garbage Collection](#lifecycle-and-garbage-collection).

KEP-6012 says new members of an already scheduled hierarchy are scheduled without re-checking the gang, and that `CompositePodGroupInitiallyScheduled` never goes back to False. We expect a raised minimum to leave admitted groups alone and only affect groups still waiting. That needs confirming with WG Workload-Aware Scheduling and is listed under [Open Questions](#open-questions).

On 1.37 the role minimum is 1, but the root starts at the full role count, so step 1 waits until every role has a replica in the new revision. That only works if the planner creates a replica of every role before it waits on readiness, which the bootstrap replica does for a missing role. This still needs checking against the KEP-1022 planner and is part of open question 3.

If a new revision's root reports `Unschedulable`, the planner handles it the way it handles scheduler-rejected target pods today, including the existing availability fallback.

### Interaction with Placement Policy

[KEP-848][kep848] already keeps a slice inside one topology domain, by injecting pod affinity on `spec.placementPolicy.topology`. A slice root topology constraint does the same through the scheduler. Using two different keys for the same intent could conflict, so when both are set the slice root topology key must equal `spec.placementPolicy.topology`. The pod affinity and anti-affinity from KEP-848, including `ExclusiveTopology`, keeps working as before. The CPG constraint does not replace it.

### Lifecycle and Garbage Collection

The Workload and the slice and role CPGs have the DisaggregatedSet as their controller owner. Replica CPGs and leaves are owned by the child LWS, as in KEP-666.

From Beta, CPGs carry a deletion-protection finalizer that is only removed once no child group refers to them, so the hierarchy always goes away from the leaves up. The controller follows the same order when it removes a slice (`slices` scaled down) or a drained revision: delete the child LWS, which removes its replica CPGs and leaves, then the role CPGs, then the root. The Workload for a revision is deleted once no root of that revision is left. Deleting the DisaggregatedSet relies on ownerReferences and the same finalizers.

### Observability

The Workload and every CPG the controller creates carry the existing `disaggregatedset.x-k8s.io/{name,slice,role,revision}` labels, so `kubectl get compositepodgroups -l disaggregatedset.x-k8s.io/name=<ds>` shows the whole tree.

The DisaggregatedSet gets a `WorkloadSchedulingCreated` condition, the same name KEP-666 uses for LWS. It is True once the Workload and every slice root and role CPG exist. The controller emits a Warning event when a slice root's `CompositePodGroupInitiallyScheduled` condition is False with reason `Unschedulable`, `SchedulerError`, or `Invalid`, and copies the message, so a stuck slice shows up on the DisaggregatedSet itself. The 1.37 case where a role at 0 blocks its slice uses the same event.

`CompositePodGroupInitiallyScheduled` only reports initial placement. Once True it stays True, even after a replica is recreated, so it is not a health signal. DisaggregatedSet readiness keeps coming from pod readiness, as today.

### Feature Gate, Upgrade and Downgrade

The feature needs the LWS `WorkloadAwareScheduling` gate, the separate default-off LWS gate KEP-666 plans for nested scheduling, and the Kubernetes `CompositePodGroup` gate. It follows the KEP-666 discovery rules: if the CPG API is not served, new objects that set `spec.scheduling` are rejected.

- Turning on `spec.scheduling` for an existing DisaggregatedSet rolls out a new revision. Old-revision LWS objects keep their own per-role Workloads from the pass-through path until they drain. KEP-666 forbids changing an LWS from flat to composite in place, and this never does, because the new revision uses new LWS objects.
- Removing `spec.scheduling` also rolls out a new revision, without CPGs.
- If the LWS gate is turned off, the controller stops creating CPGs for new revisions and leaves existing ones in place, so running pods are not disturbed.
- If the Kubernetes `CompositePodGroup` gate is turned off, existing CPGs stay in etcd and the control plane ignores them (KEP-6012).
- Downgrading LWS to a version without this field prunes `spec.scheduling` from the stored object on its next write. Existing CPGs are left behind until the DisaggregatedSet is deleted or a cleanup is run.
- Version skew: the controller checks the served CPG API to decide whether role minimums can be updated in place, and uses the 1.37 behavior when they cannot.

### Example

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: serving
spec:
  slices: 2
  roles:
    - name: prefill
      spec:
        replicas: 2
        leaderWorkerTemplate:
          size: 4           # 1 leader + 3 workers
    - name: decode
      spec:
        replicas: 1
        leaderWorkerTemplate:
          size: 2           # 1 leader + 1 worker
  scheduling:
    schedulingPolicy: { gang: {} }
    schedulingConstraints: { topology: [{ key: topology.kubernetes.io/zone }] }
    disruptionMode: { single: {} }
    roles:
      - name: prefill
        schedulingPolicy: { gang: { minGroupCount: 2 } }
        schedulingConstraints: { topology: [{ key: topology.k8s.io/block }] }
        disruptionMode: { single: {} }
        replica:
          schedulingPolicy: { gang: {} }
          schedulingConstraints: { topology: [{ key: topology.k8s.io/rack }] }
          disruptionMode: { all: {} }
          worker:
            schedulingPolicy: { gang: {} }
            schedulingConstraints: { topology: [{ key: nvidia.com/nvlink-domain }] }
      - name: decode
        schedulingConstraints: { topology: [{ key: topology.k8s.io/block }] }
        # gang over every decode replica, KEP-666 replica defaults below
```

The controller compiles this to one Workload per revision. With revision hash `abc12345`:

```yaml
apiVersion: scheduling.k8s.io/v1alpha3
kind: Workload
metadata:
  name: serving-abc12345
spec:
  controllerRef: { apiGroup: disaggregatedset.x-k8s.io, kind: DisaggregatedSet, name: serving }
  compositePodGroupTemplates:
    - name: slice
      schedulingPolicy: { gang: { minGroupCount: 2 } }
      schedulingConstraints: { topology: [{ key: topology.kubernetes.io/zone }] }
      disruptionMode: { single: {} }
      compositePodGroupTemplates:
        - name: prefill
          schedulingPolicy: { gang: { minGroupCount: 2 } }
          schedulingConstraints: { topology: [{ key: topology.k8s.io/block }] }
          disruptionMode: { single: {} }
          compositePodGroupTemplates:
            - name: prefill-replica
              schedulingPolicy: { gang: { minGroupCount: 2 } }
              schedulingConstraints: { topology: [{ key: topology.k8s.io/rack }] }
              disruptionMode: { all: {} }
              podGroupTemplates:
                - name: prefill-leader
                  schedulingPolicy: { gang: { minCount: 1 } }
                  disruptionMode: { all: {} }
                - name: prefill-worker
                  schedulingPolicy: { gang: { minCount: 3 } }
                  schedulingConstraints: { topology: [{ key: nvidia.com/nvlink-domain }] }
                  disruptionMode: { all: {} }
        - name: decode
          schedulingPolicy: { gang: { minGroupCount: 1 } }
          schedulingConstraints: { topology: [{ key: topology.k8s.io/block }] }
          compositePodGroupTemplates:
            - name: decode-replica
              # KEP-666 replica defaults, leaves decode-leader and decode-worker
```

At runtime, slice 0 has root `serving-0-abc12345` with role CPGs `serving-0-abc12345-prefill` and `serving-0-abc12345-decode`. Slice 1 has the same set with `1` in place of `0`. The child LWS `serving-0-abc12345-prefill` carries:

```yaml
metadata:
  annotations:
    scheduling.k8s.io/workload-name: serving-abc12345
    scheduling.k8s.io/group-template-name: prefill-replica
    scheduling.k8s.io/parent-compositepodgroup: serving-0-abc12345-prefill
```

### Open Questions

These need a maintainer and working-group decision before the KEP moves from provisional to implementable.

1. **Raising `minGroupCount` on a scheduled hierarchy.** The rollout steps assume a raised minimum does not touch groups that are already admitted. KEP-6012 only describes the effect of lowering it. To confirm with WG Workload-Aware Scheduling.
2. **Slice root disruption mode.** #894 proposed `single`, so a preemption can take one role of a slice and leave the other running and useless. `all` matches the "a slice serves only as a unit" goal, but the API then forces `all` on every level below, so losing one replica takes down the whole slice. This KEP keeps `single` as the default.
3. **1.37 support.** Is the restricted 1.37 behavior worth carrying, or should alpha require the Beta CPG API?
4. **Feature gate.** Ride the nested-scheduling gate KEP-666 plans, or add a DisaggregatedSet-specific one.
5. **Weighted ratios.** Out of scope here. Keep them for a DisaggregatedSet follow-up, as KEP-666 suggests.
6. **Placement of this KEP.** Standalone, or folded into KEP-766.

Topology authoring for DisaggregatedSet ([#892][issue892]) is no longer open here: this KEP defines where constraints are written at each level, and #892 can build on these fields instead of adding a second place to write them.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None. The KEP-666 Phase 2 changes listed in [Delegation to the Child LWS](#delegation-to-the-child-lws) need their own tests in LWS.

#### Unit Tests

- Compiling `spec.scheduling` into the Workload template tree, including defaults, derived minimums, and omitting the worker leaf when `size` is 1.
- Every webhook rule in [Validation](#validation).
- Minimum calculation for scale up, scale down, a role at 0, and the 1.37 fallback.
- Revision hash: changing a role `minGroupCount` keeps the revision, changing anything else in `spec.scheduling` creates a new one.

#### Integration Tests

- A DisaggregatedSet with `spec.scheduling` produces one Workload per revision, the slice and role CPGs, and the three annotations on each child LWS.
- Changing `slices` adds or removes roots without touching existing ones.
- External scaling and sub-role changes update role and root minimums in place, in the order described in [Role and Slice Minimums](#role-and-slice-minimums).
- Deleting a slice or draining a revision removes the tree from the leaves up.
- No CPGs are created when the gate is off or the CPG API is not served.

#### End-to-End Tests

With the CPG API enabled and kube-scheduler:

- A slice is admitted only when every role meets its minimum. A partial slice stays Pending and the DisaggregatedSet shows the event.
- A template rollout with `maxSurge: 0`, `maxUnavailable: 1` completes.
- Scaling `slices` up and down.
- An External scaler taking a role to 0 and back.

### Graduation Criteria

**Provisional to Implementable**

- The open questions are resolved.
- The KEP-666 changes in [Delegation to the Child LWS](#delegation-to-the-child-lws) are agreed.

**Alpha**

- Behind the nested-scheduling LWS gate, default off.
- Slice and role CPGs, delegation to child LWS, and the validation rules are implemented.
- Unit and integration tests above.

**Beta**

- Built against the `CompositePodGroup` Beta API.
- End-to-end tests above.
- At least one release of alpha feedback with no open issues on rollout or scaling deadlocks.

## Implementation History

- 2026-10-23: Provisional KEP drafted from [#893][issue893], the Future DisaggregatedSet Integration section of [KEP-666][kep666], and the API proposal in [#894][api894].
- 2026-10-31: Revised after review. Targets the KEP-6012 Beta API, moves all settings under `spec.scheduling`, adds per-revision Workloads, the three KEP-6089 annotations, and the validation, lifecycle, and observability sections.

## Drawbacks

- It adds API surface on top of an upstream API that is still alpha in 1.37.
- The useful version needs Kubernetes 1.38. On 1.37 role minimums are fixed at 1 and a role at 0 replicas blocks its slice.
- The feature also needs changes to KEP-666 before it can be implemented.

## Alternatives

- **Flat Workload and PodGroups owned by DisaggregatedSet.** Rejected. Per-role gang scheduling already works through `roles[].spec.scheduling`, and flat groups cannot gang roles together. See [Workload and PodGroup Without CompositePodGroup](#workload-and-podgroup-without-compositepodgroup).
- **Settings inside each role's LWS template (`roles[].spec.scheduling`) plus a slice block.** Rejected for the same reasons JobSet rejected KEP-6089 Option B: settings are split across two places, and the parent's API gets tied to the child's schema.
- **`rolePolicies` with a `targetRoles` selector**, as sketched in #894. A selector can match two roles or none, while the tree has exactly one role CPG per role. A list keyed by role name avoids both.
- **One root CPG shared by all slices.** Changing `slices` would change the root's child count and break the KEP-846 rule that it never triggers a rollout. It would also make all slices one disruption domain.
- **One Workload per DisaggregatedSet.** Workload templates cannot be added, removed, or changed after creation, so adding a role or changing topology would be impossible without replacing it. One Workload per revision fits the existing rollout model.

[kep666]: https://github.com/kubernetes-sigs/lws/tree/main/keps/666-gang-scheduling-in-lws
[kep766]: https://github.com/kubernetes-sigs/lws/tree/main/keps/766-DisaggregatedSet
[kep846]: https://github.com/kubernetes-sigs/lws/tree/main/keps/846-disaggregatedset-slices
[kep848]: https://github.com/kubernetes-sigs/lws/tree/main/keps/848-disaggregatedset-placement-policy
[kep849]: https://github.com/kubernetes-sigs/lws/tree/main/keps/849-DisaggregatedSet-HPA
[kep960]: https://github.com/kubernetes-sigs/lws/tree/main/keps/960-DisaggregatedSet-subroles
[kep1022]: https://github.com/kubernetes-sigs/lws/tree/main/keps/1022-DisaggregatedSet-Scaling-During-Rollout
[kep6012]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/6012-composite-podgroup-api
[kep6089]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/6089-was-controller-apis
[issue893]: https://github.com/kubernetes-sigs/lws/issues/893
[issue892]: https://github.com/kubernetes-sigs/lws/issues/892
[api894]: https://github.com/kubernetes-sigs/lws/issues/894#issuecomment-5244163695
