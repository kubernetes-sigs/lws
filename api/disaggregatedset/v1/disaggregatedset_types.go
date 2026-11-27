/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const (
	// RevisionHashVersion identifies the current revision hash algorithm, which
	// covers all generated LeaderWorkerSet fields that require a rollout.
	RevisionHashVersion = "v2"
	// RevisionHashVersionAnnotationKey records which revision hash algorithm a
	// DisaggregatedSet uses. Its absence identifies a legacy set.
	RevisionHashVersionAnnotationKey = "disaggregatedset.x-k8s.io/revision-hash-version"

	// SetNameLabelKey records the DisaggregatedSet name that resources belong to.
	// Applied to LWS and Service objects in the same namespace as the DisaggregatedSet.
	SetNameLabelKey string = "disaggregatedset.x-k8s.io/name"

	// RoleLabelKey records which role the resource belongs to (e.g. "prefill", "decode").
	// Applied to LWS and Service objects in the same namespace as the DisaggregatedSet.
	RoleLabelKey string = "disaggregatedset.x-k8s.io/role"
	// SubRoleLabelKey is controller-owned and applied to every Pod in an
	// assigned replica group.
	SubRoleLabelKey string = "disaggregatedset.x-k8s.io/subrole"

	// SliceLabelKey records which slice the resource belongs to.
	SliceLabelKey string = "disaggregatedset.x-k8s.io/slice"

	// RevisionLabelKey records the revision hash for the resource.
	// Applied to LWS and Service objects in the same namespace as the DisaggregatedSet.
	RevisionLabelKey string = "disaggregatedset.x-k8s.io/revision"

	// SetNameEnv is injected into every container of Pods managed by a DisaggregatedSet.
	SetNameEnv string = "DISAGGREGATEDSET_NAME"

	// RoleEnv is injected into every container of Pods managed by a DisaggregatedSet.
	RoleEnv string = "DISAGGREGATEDSET_ROLE"

	// SliceEnv is injected into every container of Pods managed by a DisaggregatedSet.
	SliceEnv string = "DISAGGREGATEDSET_SLICE"

	// RevisionEnv is injected into every container of Pods managed by a DisaggregatedSet.
	RevisionEnv string = "DISAGGREGATEDSET_REVISION"

	// InitialReplicasAnnotationKey records this LWS revision's intended replica
	// count. While the revision is current, the controller keeps it aligned with
	// the target. When a newer revision makes it old, the value freezes and is
	// used as its drain baseline. It can therefore differ from the current Spec
	// after an interrupted rollout. The annotation is deleted with the LWS after
	// the old revision finishes draining. A missing, non-integer, or negative
	// value is treated as unset.
	InitialReplicasAnnotationKey string = "disaggregatedset.x-k8s.io/initial-replicas"

	// MaxUnavailableAnnotationKey is a controller-owned DisaggregatedSet annotation
	// mapping role names to their last observed, resolved maxUnavailable counts.
	// Removed roles retain these counts until their LWS objects have been deleted.
	MaxUnavailableAnnotationKey string = "disaggregatedset.x-k8s.io/max-unavailable"

	// InitialSubRoleReplicasAnnotationKey records the revision's intended
	// sub-role counts as a JSON object, frozen while the revision is old.
	InitialSubRoleReplicasAnnotationKey string = "disaggregatedset.x-k8s.io/initial-subrole-replicas"

	// AutoComputeDomainAnnotationKey opts a DisaggregatedSet into per-(slice, revision)
	// NVIDIA DRA ComputeDomain management when set to "enabled", and allows
	// individual roles to opt out when set to "disabled" on role metadata.
	AutoComputeDomainAnnotationKey string = "disaggregatedset.x-k8s.io/auto-compute-domain"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RoleScalingMode controls the source of the replica count for a role.
// +kubebuilder:validation:Enum=Static;External
type RoleScalingMode string

const (
	// RoleScalingStatic uses the inline .spec.replicas value on the role.
	RoleScalingStatic RoleScalingMode = "Static"

	// RoleScalingExternal delegates replicas to an external autoscaler via an
	// auto-created DisaggregatedSetRoleScaler named "<disaggregatedset>-<role>".
	// .spec.replicas on the role is ignored.
	RoleScalingExternal RoleScalingMode = "External"
)

// RoleScaling configures how replicas are determined for a role. Sub-struct
// (not a bare enum) so future per-role scaling policies can be added without
// a v2 API bump.
type RoleScaling struct {
	// Mode controls the source of the replica count. Static (default) uses
	// inline spec.replicas; External uses the auto-created scaler CR.
	// +kubebuilder:default=Static
	// +optional
	Mode RoleScalingMode `json:"mode,omitempty"`
}

// RolloutScalingPolicy controls when a replica target change is applied during
// a rolling update. The scale-up and scale-down fields select it independently.
// +kubebuilder:validation:Enum=Deferred;Immediate
type RolloutScalingPolicy string

const (
	// RolloutScalingDeferred keeps scaling paced by the rolling update.
	RolloutScalingDeferred RolloutScalingPolicy = "Deferred"

	// RolloutScalingImmediate considers the replica target change immediately,
	// subject to the direction's availability and startup safeguards.
	RolloutScalingImmediate RolloutScalingPolicy = "Immediate"
)

// RolloutScaleDownPolicy is an alias for RolloutScalingPolicy.
//
// Deprecated: Use RolloutScalingPolicy.
type RolloutScaleDownPolicy = RolloutScalingPolicy

const (
	// RolloutScaleDownDeferred is an alias for RolloutScalingDeferred.
	//
	// Deprecated: Use RolloutScalingDeferred.
	RolloutScaleDownDeferred = RolloutScalingDeferred

	// RolloutScaleDownImmediate is an alias for RolloutScalingImmediate.
	//
	// Deprecated: Use RolloutScalingImmediate.
	RolloutScaleDownImmediate = RolloutScalingImmediate
)

// DisaggregatedSetScalingPolicy configures how replica target changes interact
// with DisaggregatedSet reconciliation.
type DisaggregatedSetScalingPolicy struct {
	// ScaleUpDuringRollout controls target growth while old revisions remain.
	// Deferred preserves readiness-paced growth, without waiting for the entire
	// rollout to finish. Immediate also tops up target replicas to desired minus
	// replicas requested from all old revisions of the same role. Loading groups
	// count, so readiness loss alone does not trigger extra growth.
	// Old-revision retirement checks are unchanged.
	// Renamed-role replacement limits still apply.
	// During a partial canary, Deferred instead queues demand increases until
	// finalization or a switch to Immediate. Percentage promotion still proceeds.
	// +optional
	// +kubebuilder:default=Deferred
	ScaleUpDuringRollout RolloutScalingPolicy `json:"scaleUpDuringRollout,omitempty"`

	// ScaleDownDuringRollout controls demand-driven target replica reductions.
	// Deferred waits until old revisions finish draining. Immediate considers
	// reductions during the rollout, preferring safe old-revision drains.
	// Both policies allow scale-up and old-revision drains within rollout bounds.
	// Deferred uses max(initial old replicas, effective desired) + maxSurge as
	// the surge ceiling. External roles retain at least the target's current Spec
	// as effective desired while old replicas of that role remain.
	// Immediate uses desired + maxSurge as the surge ceiling and may shrink
	// target replicas when old revisions must remain. Under both policies,
	// reductions of roles still present preserve max(0, effective desired -
	// maxUnavailable) Ready replicas, or avoid worsening an existing shortfall.
	// Renamed-role surplus recovery uses the same availability and scheduled-startup
	// protection as other target reductions. These safeguards may delay
	// reductions: Immediate does not mean instant deletion.
	// During a partial canary, Deferred queues demand decreases (including zero)
	// until finalization or a switch to Immediate. The latest request wins.
	// +optional
	// +kubebuilder:default=Deferred
	ScaleDownDuringRollout RolloutScalingPolicy `json:"scaleDownDuringRollout,omitempty"`

	// SubRoleScaleDelaySeconds batches scale updates within a fixed window for LWS
	// pools with multiple sub-roles and at least one External scaler. Defaults to 2
	// seconds (range 0 to 60); zero disables it. Scaling from zero and rollouts bypass
	// the delay. Controller restarts reset the window.
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=60
	SubRoleScaleDelaySeconds *int32 `json:"subRoleScaleDelaySeconds,omitempty"`
}

// DisaggregatedSubRoleSpec defines a scaling and routing pool sharing its parent's templates.
// +kubebuilder:validation:XValidation:rule="!has(self.scaling) || self.scaling.mode != 'External' || !has(self.replicas)",message="replicas must be omitted when scaling.mode is External"
type DisaggregatedSubRoleSpec struct {
	// Name is unique within the parent role.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`
	// Replicas counts groups (Static default: 1); omit it for External scaling.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`
	// Scaling selects this pool's replica source.
	// +optional
	Scaling *RoleScaling `json:"scaling,omitempty"`

	// MaxSurge overrides the parent's rollout surge budget for this sub-role.
	// Accepts a non-negative integer or a percentage up to 100%, rounded up
	// against this sub-role's replica target. Omitted values inherit the parent;
	// with no parent rollout configuration, the default is 0.
	// +optional
	// +kubebuilder:validation:XIntOrString
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

	// MaxUnavailable overrides the parent's rollout unavailability budget for
	// this sub-role. Accepts a non-negative integer or a percentage up to 100%,
	// rounded down against this sub-role's replica target. Omitted values inherit
	// the parent; with no parent rollout configuration, the default is 1. Zero is explicit.
	// +optional
	// +kubebuilder:validation:XIntOrString
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// DisaggregatedRoleSpec defines the configuration for a disaggregated role.
// This structure embeds LeaderWorkerSetTemplateSpec from sigs.k8s.io/lws, with validation
// to reject unsupported fields (RolloutStrategy.Type must be RollingUpdate,
// RolloutStrategy.RollingUpdateConfiguration.Partition must not be set).
type DisaggregatedRoleSpec struct {
	// Name is the unique identifier for this role.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// SubRoles partitions one LWS into pools sharing its templates.
	// Child targets sum to parent replicas; omit parent scaling. Parent spec.replicas is ignored.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	SubRoles []DisaggregatedSubRoleSpec `json:"subRoles,omitempty"`

	// Scaling configures how replicas are determined. Omit for inline Static
	// scaling (default). When set to External, the DisaggregatedSet controller
	// auto-creates a DisaggregatedSetRoleScaler and reads its spec.replicas.
	// +optional
	Scaling *RoleScaling `json:"scaling,omitempty"`

	// LeaderWorkerSetTemplateSpec defines the LWS template for this role.
	// Note: Spec.RolloutStrategy.Type must be RollingUpdate (or empty) and
	// Spec.RolloutStrategy.RollingUpdateConfiguration.Partition must not be set.
	// DisaggregatedSet handles rollouts across roles.
	leaderworkerset.LeaderWorkerSetTemplateSpec `json:",inline"`
}

// DisaggregatedSetSpec defines the desired state of DisaggregatedSet.
//
// The all-or-nothing replicas rule (either every role has replicas > 0, or
// every role has replicas == 0) applies only to non-External, unpartitioned
// roles. External targets come from scalers; sub-roles may independently scale
// to zero without pausing sibling routing pools.
// +kubebuilder:validation:XValidation:rule="self.roles.filter(r, (!has(r.subRoles) || size(r.subRoles) == 0) && (!has(r.scaling) || r.scaling.mode != 'External')).all(r, !has(r.spec.replicas) || r.spec.replicas == 0) || self.roles.filter(r, (!has(r.subRoles) || size(r.subRoles) == 0) && (!has(r.scaling) || r.scaling.mode != 'External')).all(r, has(r.spec.replicas) && r.spec.replicas > 0)",message="replicas must be zero for all non-External unpartitioned roles or non-zero for all non-External unpartitioned roles"
type DisaggregatedSetSpec struct {
	// Roles defines the list of roles (at least 1 required).
	// Each role has a unique name and its own configuration.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	// +required
	Roles []DisaggregatedRoleSpec `json:"roles"`

	// Slices is the number of independent copies of the whole role topology.
	// Each slice is a complete set of all roles that rolls out independently.
	// Changing Slices scales copies up or down and does not trigger a rollout.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Slices *int32 `json:"slices,omitempty"`

	// ScalingPolicy controls scale-up and scale-down during rolling updates.
	// Omit it to use Deferred for both directions: readiness-paced growth and
	// target reductions after old revisions drain.
	// +optional
	ScalingPolicy *DisaggregatedSetScalingPolicy `json:"scalingPolicy,omitempty"`

	// RolloutStrategy controls progressive delivery across all roles and sub-roles.
	// Changing it does not create a new revision.
	// +optional
	RolloutStrategy *DisaggregatedSetRolloutStrategy `json:"rolloutStrategy,omitempty"`

	// PlacementPolicy controls how a slice's roles are co-located and how the
	// DisaggregatedSet's slices are spread across topology domains. When set, the
	// controller injects pod affinity and anti-affinity into the managed
	// LeaderWorkerSet pod templates. Placement is applied when a LeaderWorkerSet is
	// created, so changing it takes effect on the next rollout.
	// +optional
	PlacementPolicy *PlacementPolicy `json:"placementPolicy,omitempty"`
}

// CanaryDimension selects the unit used to divide a canary.
// +kubebuilder:validation:Enum=Groups;Slices
type CanaryDimension string

const (
	CanaryDimensionGroups CanaryDimension = "Groups"
	CanaryDimensionSlices CanaryDimension = "Slices"
)

// DisaggregatedSetRolloutStrategy configures a set-wide canary hold.
type DisaggregatedSetRolloutStrategy struct {
	// Canary configures the new revision's requested share during a rollout.
	// +optional
	Canary *DisaggregatedSetCanary `json:"canary,omitempty"`
}

// DisaggregatedSetCanary configures a percentage-based canary rollout.
type DisaggregatedSetCanary struct {
	// Percent is the new revision's requested share, using Dimension
	// as the unit. By default, split each role/sub-role's groups independently in
	// every slice. Omitted or 100 finishes the rollout, allows old history to be
	// cleaned up, and accepts the new revision as the next canary's baseline.
	// Lowering the percentage after completion does not restore retired history;
	// use a Git revert for rollback after finalization.
	// The oldest revision with surviving groups is recorded as the baseline;
	// that choice survives controller restarts. A later template update replaces
	// the canary within its share while retaining the recorded baseline's share.
	// During a partial canary Percent may increase, but decreases are rejected.
	// Starting a partial canary from omitted/100 requires the current generation
	// to be observed, no pending canary cleanup, and every role's existing replicas
	// to be on the current revision. Unready groups or single-revision scaling
	// do not prevent starting a canary. Requesting finalization does not complete it.
	// Start at 0 to hold the baseline before creating any canary groups.
	// For a durable rollback, restore the stable templates in Git. If they match
	// the surviving baseline, the rollout converges fully to it without a hold.
	// Remove Percent in the same Git change to leave no canary hold.
	// It does not limit initial creation when there is no old revision.
	// For 1..99, the canary count rounds up and keeps at least one unit on each
	// side. In groups mode, a role starting with one admitted group keeps the
	// whole slice on the baseline. If both sides already have groups, reducing
	// demand to one retains one group on each side instead of restarting the
	// canary. This one extra group consumes surge allowance; with zero surge,
	// retention is a one-group exception. Admitted zero demand or finalization
	// ends retention.
	// A peer resuming at one waits on the baseline without discarding existing
	// canary groups in other roles; further canary growth waits until it can split.
	// In slices mode the units are whole slices instead of groups.
	// This is not a traffic percentage.
	// While held, Immediate admits demand changes in its configured direction;
	// Deferred keeps the last admitted count for that direction. Accepted demand
	// resizes both shares within readiness and availability budgets. Promotion
	// and replacement of the canary templates continue even with Deferred.
	// Percentage changes alone never recreate retired history.
	// The active role/sub-role layout must match the recorded baseline; an
	// incompatible baseline blocks even if a younger revision is compatible.
	// An ambiguous initial choice or conflicting recorded baselines also blocks.
	// Existing roles can scale to and from zero. Scaling the whole set to zero
	// retains the marked baseline's LWS templates; scale-up restores the split.
	// Finalizing at 100 or by removing the field retires that baseline and its
	// rollback history. Keeping 100 does not automatically hold the next template
	// update below 100%; set its starting percentage with the new templates.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percent *int32 `json:"percent,omitempty"`

	// Dimension defaults to Groups: divide each role/sub-role within each
	// slice. Slices instead promotes whole slices in ascending slice-index order.
	// Dimensions cannot change while Percent is below 100.
	// Slice-count changes are not supported during a partial Slices canary;
	// finish at 100 or remove Percent first.
	// This field has no effect when Percent is omitted or 100.
	// +optional
	Dimension CanaryDimension `json:"dimension,omitempty"`
}

// PlacementType selects the DisaggregatedSet placement guarantee.
type PlacementType string

const (
	// PlacementNone injects no affinity. This is the default.
	PlacementNone PlacementType = "None"
	// PlacementExclusiveSlice co-locates a slice's roles in one topology domain and
	// spreads this DisaggregatedSet's slices across domains. Other DisaggregatedSets
	// may share a domain.
	PlacementExclusiveSlice PlacementType = "ExclusiveSlice"
	// PlacementExclusiveTopology is ExclusiveSlice plus domain exclusivity: a domain
	// holds at most one slice across all DisaggregatedSets (a 1:1 domain-to-slice mapping).
	PlacementExclusiveTopology PlacementType = "ExclusiveTopology"
)

// PlacementPolicy controls topology placement of a DisaggregatedSet's slices.
type PlacementPolicy struct {
	// Type selects the placement guarantee. Defaults to None.
	// +optional
	// +kubebuilder:default=None
	// +kubebuilder:validation:Enum=None;ExclusiveSlice;ExclusiveTopology
	Type PlacementType `json:"type,omitempty"`

	// Topology is the node-label key that defines a domain, used as the affinity
	// topologyKey. Required when Type is not None.
	// +optional
	Topology string `json:"topology,omitempty"`
}

// SubRoleStatus defines the observed state of a sub-role within its parent.
type SubRoleStatus struct {
	// Name is the sub-role name, without the parent prefix.
	// +required
	Name string `json:"name"`

	// DesiredReplicas is the number of replica groups the controller is driving
	// this sub-role toward, totalled across slices: its replicas, or the target of
	// its DisaggregatedSetRoleScaler when its scaling mode is External. It is unset
	// while that target is unknown.
	// +optional
	DesiredReplicas *int32 `json:"desiredReplicas,omitempty"`

	// MinAvailableReplicas is the availability floor of this sub-role, totalled
	// across slices: in each slice, desiredReplicas minus its resolved
	// maxUnavailable, and never below zero. It is unset while desiredReplicas is
	// unknown. The MinimumReplicasAvailable condition is judged against it.
	// +optional
	MinAvailableReplicas *int32 `json:"minAvailableReplicas,omitempty"`

	// Replicas is the number of replica groups assigned to this sub-role.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas counts Ready groups with a coherent whole-group assignment.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas counts the Ready groups that can serve requests: those
	// in a revision of a slice in which every role that revision requires has
	// at least one Ready group. It never exceeds ReadyReplicas.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// UpdatedReplicas counts groups belonging to the latest revision.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`
}

// RoleStatus defines the observed state of a single parent role.
type RoleStatus struct {
	// Name is the parent role name.
	// +required
	Name string `json:"name"`

	// DesiredReplicas is the number of replicas the controller is driving this role
	// toward, totalled across slices: its replicas, or the target of its
	// DisaggregatedSetRoleScaler when its scaling mode is External. A partitioned
	// role reports the sum of its sub-role targets. It is unset while any of those
	// targets is unknown, for example before the scaler exists, so it is never
	// mistaken for a requested zero. The Available condition is judged against it.
	// +optional
	DesiredReplicas *int32 `json:"desiredReplicas,omitempty"`

	// MinAvailableReplicas is the availability floor of this role, totalled
	// across slices: in each slice, desiredReplicas minus its resolved
	// maxUnavailable, and never below zero. A partitioned role reports the sum
	// of its sub-role floors. It is unset while any of those targets is unknown.
	// The MinimumReplicasAvailable condition is judged against it. During a
	// Deferred rollout the controller may protect more Ready replicas than this.
	// +optional
	MinAvailableReplicas *int32 `json:"minAvailableReplicas,omitempty"`

	// Replicas is the total number of replicas for this role.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of ready replicas for this role.
	// Sub-role readiness requires a coherent assignment across the whole group.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas counts the Ready replicas that can serve requests: those
	// in a revision of a slice in which every role that revision requires has
	// at least one Ready replica. A Ready Prefill cannot serve without a Ready
	// Decode of the same revision and slice. It never exceeds ReadyReplicas.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// UpdatedReplicas is the number of replicas updated to the latest revision.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// SubRoleStatuses lists the current sub-roles in spec order.
	// Removed sub-roles are omitted, but their groups remain in the parent counts
	// while draining. Do not add child counts to their parent aggregate.
	// +listType=map
	// +listMapKey=name
	// +optional
	SubRoleStatuses []SubRoleStatus `json:"subRoleStatuses,omitempty"`
}

// DisaggregatedSetCanaryStatus describes an observed canary or its pending
// cleanup. Replica counts remain in RoleStatuses; this section does not drive
// rollout planning.
type DisaggregatedSetCanaryStatus struct {
	// Percent is the requested canary percentage observed by the controller,
	// not a measured traffic weight or a guarantee of the current replica split.
	// +required
	Percent int32 `json:"percent"`

	// Dimension is the unit of the requested percentage: Groups or Slices.
	// +required
	Dimension CanaryDimension `json:"dimension"`

	// TargetRevision identifies the revision computed from the desired templates.
	// +required
	TargetRevision string `json:"targetRevision"`

	// BaselineRevisions lists the recorded baseline revision identifiers across
	// slices. It is empty until the baseline markers have been observed. Different
	// slices can retain different baselines; conflicting markers do not imply a
	// successful hold. Consult Settled and Conditions before treating it as ready.
	// +optional
	// +listType=set
	BaselineRevisions []string `json:"baselineRevisions,omitempty"`

	// Settled is true only when the controller reports CanaryHeld for the
	// observed generation and replica demand. It confirms replica readiness,
	// not application health. External scaler changes do not advance the parent
	// generation; compare RoleStatuses.DesiredReplicas with the scaler targets.
	// +required
	Settled bool `json:"settled"`
}

// DisaggregatedSetStatus defines the observed state of DisaggregatedSet.
type DisaggregatedSetStatus struct {
	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// observedGeneration is the most recent generation observed for this DisaggregatedSet.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RoleStatuses lists current parent roles in spec order, with nested sub-role statuses.
	// Parent counts include historical layouts and may exceed the visible child sum.
	// Removed roles are omitted even while their groups drain.
	// +listType=map
	// +listMapKey=name
	// +optional
	RoleStatuses []RoleStatus `json:"roleStatuses,omitempty"`

	// Canary describes a partial canary with surviving history, or its pending
	// cleanup at omitted/100. Finalization clears it only after old LWS objects
	// disappear. It is absent during initial creation without a baseline.
	// +optional
	Canary *DisaggregatedSetCanaryStatus `json:"canary,omitempty"`

	// conditions represent the current state of the DisaggregatedSet resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the rollout is complete and every desired group is Ready
	// - "Progressing": the resource is being created or updated
	// Available is False during a successful partial canary hold. Use
	// MinimumReplicasAvailable for the serving-capacity floor and application
	// health checks for service health, not Available alone.
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DisaggregatedSetConditionType is a valid value for DisaggregatedSetStatus.Conditions[].Type.
type DisaggregatedSetConditionType string

// These are built-in conditions of a DisaggregatedSet.
const (
	// DisaggregatedSetAvailable means every role has reached its desired replica
	// count (across all slices), with all of those replicas ready and updated to
	// the current revision. A role with a desired replica count of 0 (the
	// all-roles-paused state) is satisfied once it has fully drained to 0.
	DisaggregatedSetAvailable DisaggregatedSetConditionType = "Available"

	// DisaggregatedSetProgressing means at least one role has not yet reached its
	// desired replica count, or has replicas that are not ready or not updated to
	// the current revision.
	DisaggregatedSetProgressing DisaggregatedSetConditionType = "Progressing"

	// DisaggregatedSetMinimumReplicasAvailable means every role and sub-role has
	// at least minAvailableReplicas available replicas. Unlike Available, it
	// stays True during scale-ups and rollouts that remain within each role's
	// maxUnavailable budget. It is False while any role is below a known floor,
	// and otherwise Unknown while any replica target is unknown.
	DisaggregatedSetMinimumReplicasAvailable DisaggregatedSetConditionType = "MinimumReplicasAvailable"

	// DisaggregatedSetSubRolesAssigned means every live group of a partitioned
	// role has a consistent assignment on all of its observed members.
	DisaggregatedSetSubRolesAssigned DisaggregatedSetConditionType = "SubRolesAssigned"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName={dset,disaggset}
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// DisaggregatedSet is the Schema for the disaggregatedsets API
type DisaggregatedSet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of DisaggregatedSet
	// +required
	Spec DisaggregatedSetSpec `json:"spec"`

	// status defines the observed state of DisaggregatedSet
	// +optional
	Status DisaggregatedSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DisaggregatedSetList contains a list of DisaggregatedSet
type DisaggregatedSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DisaggregatedSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DisaggregatedSet{}, &DisaggregatedSetList{})
}
