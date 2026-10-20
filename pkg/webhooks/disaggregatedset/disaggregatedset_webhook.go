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

package disaggregatedset

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/pkg/webhooks"
)

// DisaggregatedSetWebhook handles validation for DisaggregatedSet resources.
type DisaggregatedSetWebhook struct{}

// SetupDisaggregatedSetWebhook registers the webhook with the manager.
func SetupDisaggregatedSetWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &disaggv1.DisaggregatedSet{}).
		WithValidator(&DisaggregatedSetWebhook{}).
		Complete()
}

//+kubebuilder:webhook:path=/validate-disaggregatedset-x-k8s-io-v1-disaggregatedset,mutating=false,failurePolicy=fail,sideEffects=None,groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets,verbs=create;update,versions=v1,name=vdisaggregatedset.kb.io,admissionReviewVersions=v1

var _ admission.Validator[*disaggv1.DisaggregatedSet] = &DisaggregatedSetWebhook{}

// ValidateCreate implements admission.Validator for create operations.
func (w *DisaggregatedSetWebhook) ValidateCreate(ctx context.Context, disagg *disaggv1.DisaggregatedSet) (admission.Warnings, error) {
	warnings, allErrs := w.validate(disagg, nil)
	allErrs = append(allErrs, w.validatePlacement(disagg)...)
	return warnings, allErrs.ToAggregate()
}

// ValidateUpdate implements admission.Validator for update operations.
func (w *DisaggregatedSetWebhook) ValidateUpdate(ctx context.Context, oldDisagg, newDisagg *disaggv1.DisaggregatedSet) (admission.Warnings, error) {
	warnings, allErrs := w.validate(newDisagg, oldDisagg)
	allErrs = append(allErrs, w.validatePlacement(newDisagg)...)
	return warnings, allErrs.ToAggregate()
}

// ValidateDelete implements admission.Validator for delete operations.
func (w *DisaggregatedSetWebhook) ValidateDelete(ctx context.Context, disagg *disaggv1.DisaggregatedSet) (admission.Warnings, error) {
	return nil, nil
}

func (w *DisaggregatedSetWebhook) validate(obj, oldObj *disaggv1.DisaggregatedSet) (admission.Warnings, field.ErrorList) {
	var allErrs field.ErrorList
	var warnings admission.Warnings
	rolesPath := field.NewPath("spec", "roles")

	// Roles are a map-style list; reordering must preserve legacy templates.
	oldRoles := make(map[string]*disaggv1.DisaggregatedRoleSpec)
	if oldObj != nil {
		for i := range oldObj.Spec.Roles {
			role := &oldObj.Spec.Roles[i]
			oldRoles[role.Name] = role
		}
	}

	hasExternal := false
	generatedScalerNames := make(map[string]*field.Path)
	for i, role := range obj.Spec.Roles {
		rolePath := rolesPath.Index(i)
		metadataPath := rolePath.Child("metadata")
		oldRole := oldRoles[role.Name]
		unchanged := unchangedRoleTemplate(&role, oldRole)
		if oldRole == nil || !apiequality.Semantic.DeepEqual(role.Labels, oldRole.Labels) {
			allErrs = append(allErrs, metav1validation.ValidateLabels(role.Labels, metadataPath.Child("labels"))...)
		}
		if oldRole == nil || !apiequality.Semantic.DeepEqual(role.Annotations, oldRole.Annotations) {
			allErrs = append(allErrs, apivalidation.ValidateAnnotations(role.Annotations, metadataPath.Child("annotations"))...)
		}
		if !unchanged || !apiequality.Semantic.DeepEqual(role.Labels, oldRole.Labels) {
			allErrs = append(allErrs, validateReservedSubRoleLabel(role, rolePath)...)
		}
		if !unchanged {
			allErrs = append(allErrs, w.validateRoleRolloutStrategy(role, rolePath)...)
			lws := &leaderworkerset.LeaderWorkerSet{ObjectMeta: role.ObjectMeta, Spec: role.Spec}
			allErrs = append(allErrs, webhooks.ValidateLeaderWorkerSetTemplate(rolePath.Child("spec"), metadataPath, lws)...)
		}
		if len(role.SubRoles) > 0 {
			if role.Scaling != nil && !unchanged {
				allErrs = append(allErrs, field.Forbidden(rolePath.Child("scaling"), "parent scaling must be omitted when subRoles is present"))
			}
			if role.Spec.Replicas != nil && *role.Spec.Replicas > 1 {
				warnings = append(warnings, fmt.Sprintf("role %q defines subRoles: parent spec.replicas is ignored; replicas are the sum of sub-role targets", role.Name))
			}
			var staticReplicas int64
			for j, child := range role.SubRoles {
				childPath := rolePath.Child("subRoles").Index(j)
				if !unchanged {
					allErrs = append(allErrs, validateSubRoleRolloutStrategy(&role, &child, childPath)...)
				}
				if child.Scaling == nil || child.Scaling.Mode != disaggv1.RoleScalingExternal {
					staticReplicas += int64(ptr.Deref(child.Replicas, 1))
					continue
				}
				hasExternal = true
				allErrs = append(allErrs, validateScalerName(obj.Name+"-"+role.Name+"-"+child.Name, child.Name, childPath.Child("name"), generatedScalerNames)...)
			}
			if !unchanged {
				allErrs = append(allErrs, webhooks.ValidateReplicaCount(rolePath.Child("subRoles"), staticReplicas, ptr.Deref(role.Spec.LeaderWorkerTemplate.Size, 1))...)
			}
			continue
		}

		if role.Scaling == nil || role.Scaling.Mode != disaggv1.RoleScalingExternal {
			if !unchanged {
				allErrs = append(allErrs, webhooks.ValidateReplicaCount(rolePath.Child("spec", "replicas"), int64(ptr.Deref(role.Spec.Replicas, 1)), ptr.Deref(role.Spec.LeaderWorkerTemplate.Size, 1))...)
			}
			continue
		}
		hasExternal = true

		// Scaler name is "<ds>-<role>" and must fit within the Kubernetes 253-character limit.
		allErrs = append(allErrs, validateScalerName(obj.Name+"-"+role.Name, role.Name, rolePath.Child("name"), generatedScalerNames)...)

		// spec.replicas is ignored for External roles; explicit values > 1
		// almost certainly indicate confusion about which knob takes effect.
		if role.Spec.Replicas != nil && *role.Spec.Replicas > 1 {
			warnings = append(warnings, fmt.Sprintf(
				"role %q sets scaling.mode: External and spec.replicas: %d — spec.replicas is ignored; drive replicas via DisaggregatedSetRoleScaler %q instead",
				role.Name, *role.Spec.Replicas, obj.Name+"-"+role.Name))
		}
	}

	// Alpha: External scaling and slices > 1 are incompatible. The scaler
	// design for multi-slice is deferred to a follow-up KEP.
	if hasExternal && obj.Spec.Slices != nil && *obj.Spec.Slices > 1 {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "slices"),
			"spec.slices > 1 is not supported while any role has scaling.mode: External (alpha restriction)"))
	}

	// Names are immutable. Rechecking a legacy name would prevent cleanup.
	if obj.Name != "" && (oldObj == nil || obj.Name != oldObj.Name) {
		if errs := utilvalidation.IsDNS1035Label(obj.Name); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(field.NewPath("metadata", "name"), obj.Name,
				"name is used as a prefix for generated LeaderWorkerSets: "+strings.Join(errs, "; ")))
		}
	}
	allErrs = append(allErrs, w.validateGeneratedNames(obj, oldRoles)...)

	return warnings, allErrs
}

func isExternal(scaling *disaggv1.RoleScaling) bool {
	return scaling != nil && scaling.Mode == disaggv1.RoleScalingExternal
}

// externalLeaves returns the role's External leaves: "" for an ordinary
// role, or the names of its External sub-roles.
func externalLeaves(role *disaggv1.DisaggregatedRoleSpec) map[string]bool {
	leaves := map[string]bool{}
	if role == nil {
		return leaves
	}
	if len(role.SubRoles) == 0 {
		if isExternal(role.Scaling) {
			leaves[""] = true
		}
		return leaves
	}
	for _, child := range role.SubRoles {
		if isExternal(child.Scaling) {
			leaves[child.Name] = true
		}
	}
	return leaves
}

// hasNewExternalLeaf reports whether the role has an External leaf that was
// not External before. Its replica count comes from a scaler, so admission
// cannot bound it. Existing External leaves keep their earlier budget so
// unrelated updates of stored DisaggregatedSets are not rejected.
func hasNewExternalLeaf(role, oldRole *disaggv1.DisaggregatedRoleSpec) bool {
	old := externalLeaves(oldRole)
	for leaf := range externalLeaves(role) {
		if !old[leaf] {
			return true
		}
	}
	return false
}

// unchangedRoleTemplate preserves legacy templates during unrelated updates and
// scale-to-zero. Positive replica changes, new roles, and changed cross-field
// inputs still require validation. Comparisons never default the stored template.
func unchangedRoleTemplate(role, oldRole *disaggv1.DisaggregatedRoleSpec) bool {
	if oldRole == nil {
		return false
	}
	key := leaderworkerset.SubGroupExclusiveKeyAnnotationKey
	value, found := role.Annotations[key]
	oldValue, oldFound := oldRole.Annotations[key]
	if found != oldFound || value != oldValue {
		return false
	}
	external := role.Scaling != nil && role.Scaling.Mode == disaggv1.RoleScalingExternal
	oldExternal := oldRole.Scaling != nil && oldRole.Scaling.Mode == disaggv1.RoleScalingExternal
	if external != oldExternal || len(role.SubRoles) != len(oldRole.SubRoles) {
		return false
	}
	spec, oldSpec := role.Spec, oldRole.Spec
	if len(role.SubRoles) == 0 {
		replicas, oldReplicas := ptr.Deref(spec.Replicas, 1), ptr.Deref(oldSpec.Replicas, 1)
		if !external && replicas != oldReplicas && replicas != 0 {
			return false
		}
	} else {
		if !apiequality.Semantic.DeepEqual(role.Scaling, oldRole.Scaling) {
			return false
		}
		totalReplicas := *validationReplicasForRole(*role)
		oldTotalReplicas := *validationReplicasForRole(*oldRole)
		if totalReplicas != oldTotalReplicas && totalReplicas != 0 {
			return false
		}
		// Parent replicas are ignored for partitioned roles. Match children by
		// name so reordering and stopping existing pools can preserve legacy data.
		oldChildren := make(map[string]disaggv1.DisaggregatedSubRoleSpec, len(oldRole.SubRoles))
		for _, child := range oldRole.SubRoles {
			oldChildren[child.Name] = child
		}
		for _, child := range role.SubRoles {
			oldChild, found := oldChildren[child.Name]
			if !found {
				return false
			}
			if replicas := ptr.Deref(child.Replicas, 1); replicas != ptr.Deref(oldChild.Replicas, 1) && replicas != 0 {
				return false
			}
			if child.Scaling != nil && (child.Scaling.Mode == "" || child.Scaling.Mode == disaggv1.RoleScalingStatic) {
				child.Scaling = nil
			}
			if oldChild.Scaling != nil && (oldChild.Scaling.Mode == "" || oldChild.Scaling.Mode == disaggv1.RoleScalingStatic) {
				oldChild.Scaling = nil
			}
			child.Replicas, oldChild.Replicas = nil, nil
			if !apiequality.Semantic.DeepEqual(child, oldChild) {
				return false
			}
		}
	}
	spec.Replicas, oldSpec.Replicas = nil, nil
	return apiequality.Semantic.DeepEqual(spec, oldSpec)
}

func validateScalerName(name, value string, path *field.Path, generated map[string]*field.Path) field.ErrorList {
	var errs field.ErrorList
	if len(name) > 253 {
		errs = append(errs, field.Invalid(path, value, fmt.Sprintf("would produce scaler name %q exceeding 253 characters", name)))
	}
	if previous := generated[name]; previous != nil {
		errs = append(errs, field.Invalid(path, value, fmt.Sprintf("generated scaler name collides with %s", previous)))
	}
	generated[name] = path
	return errs
}

func validateReservedSubRoleLabel(role disaggv1.DisaggregatedRoleSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	check := func(labels map[string]string, path *field.Path) {
		if _, exists := labels[disaggv1.SubRoleLabelKey]; exists {
			errs = append(errs, field.Forbidden(path.Key(disaggv1.SubRoleLabelKey), "label is reserved for controller-managed sub-role assignment"))
		}
	}
	check(role.Labels, path.Child("metadata", "labels"))
	templates := path.Child("spec", "leaderWorkerTemplate")
	check(role.Spec.LeaderWorkerTemplate.WorkerTemplate.Labels, templates.Child("workerTemplate", "metadata", "labels"))
	if leader := role.Spec.LeaderWorkerTemplate.LeaderTemplate; leader != nil {
		check(leader.Labels, templates.Child("leaderTemplate", "metadata", "labels"))
	}
	return errs
}

// Validate the physical sum rather than the ignored parent target. The schema
// forbids replicas for External children, so they use one until resolved.
func validationReplicasForRole(role disaggv1.DisaggregatedRoleSpec) *int32 {
	if len(role.SubRoles) == 0 {
		return role.Spec.Replicas
	}
	var total int64
	for _, child := range role.SubRoles {
		total += int64(ptr.Deref(child.Replicas, 1))
	}
	return ptr.To(int32(min(total, int64(math.MaxInt32))))
}

// validateGeneratedNames checks the length of generated LWS names and their
// derived names. The generated name format is:
//
//	<dsName>-<sliceIndex>-<revision 8 chars>-<roleName>
//
// A new External role or sub-role reserves room for the largest group index
// any scaler target can produce, since its replicas are not known at admission.
func (w *DisaggregatedSetWebhook) validateGeneratedNames(obj *disaggv1.DisaggregatedSet, oldRoles map[string]*disaggv1.DisaggregatedRoleSpec) field.ErrorList {
	var allErrs field.ErrorList
	// Worst-case slice index string length: slices can be 1-100 → index 0-99 → up to 2 digits.
	slices := int32(1)
	if obj.Spec.Slices != nil {
		slices = *obj.Spec.Slices
	}
	maxSliceIndex := slices - 1
	sliceDigits := len(strconv.Itoa(int(maxSliceIndex)))

	const (
		dns1035MaxLen                     = 63
		revisionLen                       = 8  // hex characters in the revision hash
		separators                        = 3  // three "-" between dsName, slice, revision, roleName
		statefulSetRevisionLabelSuffixLen = 11 // len("-<10-char-hash>")
	)

	rolesPath := field.NewPath("spec", "roles")
	for i, role := range obj.Spec.Roles {
		lwsNameLen := len(obj.Name) + separators + sliceDigits + revisionLen + len(role.Name)

		groupIndexDigits := 1
		if replicas := validationReplicasForRole(role); replicas != nil && *replicas > 0 {
			groupIndexDigits = len(strconv.Itoa(int(*replicas - 1)))
		}
		unbounded := hasNewExternalLeaf(&role, oldRoles[role.Name])
		if unbounded {
			groupIndexDigits = len(strconv.Itoa(math.MaxInt32 - 1))
		}

		// The worker StatefulSet pods get a label "controller-revision-hash" with value:
		// <lwsName>-<groupIndex>-<hash>
		// Which translates to: lwsNameLen + 1 (dash) + groupIndexDigits + statefulSetRevisionLabelSuffixLen
		maxSuffixLen := 1 + groupIndexDigits + statefulSetRevisionLabelSuffixLen
		if role.Spec.GroupIdentity == leaderworkerset.GroupIdentityHash {
			// Hash roles derive names from the leader host name instead.
			maxSuffixLen = webhooks.HashNameSuffixLen(&role.Spec)
		}

		maxNameLen := lwsNameLen + maxSuffixLen

		if maxNameLen > dns1035MaxLen {
			combinedLimit := dns1035MaxLen - separators - sliceDigits - revisionLen - maxSuffixLen
			msg := fmt.Sprintf(
				"the generated names (%d chars) would exceed the DNS-1035 limit of %d characters (accounting for StatefulSet revision hash labels); "+
					"reduce the DisaggregatedSet name and/or role name (combined limit: %d characters)",
				maxNameLen, dns1035MaxLen, combinedLimit,
			)
			if unbounded {
				msg += "; scaling.mode External reserves room for the largest group index its scaler can request"
			}
			allErrs = append(allErrs, field.Invalid(rolesPath.Index(i).Child("name"), role.Name, msg))
		}
	}
	return allErrs
}

// validatePlacement validates the DisaggregatedSet PlacementPolicy. A non-None policy
// needs a topology key, and conflicts with the LWS group-level exclusive-topology
// annotation on a role: both co-locate/exclude at overlapping levels, so the slice
// would never schedule.
func (w *DisaggregatedSetWebhook) validatePlacement(obj *disaggv1.DisaggregatedSet) field.ErrorList {
	var allErrs field.ErrorList

	policy := obj.Spec.PlacementPolicy
	if policy == nil || policy.Type == disaggv1.PlacementNone || policy.Type == "" {
		return allErrs
	}
	policyPath := field.NewPath("spec", "placementPolicy")

	if policy.Topology == "" {
		allErrs = append(allErrs, field.Required(policyPath.Child("topology"),
			"topology is required when type is not None"))
	}

	rolesPath := field.NewPath("spec", "roles")
	for i, role := range obj.Spec.Roles {
		if key, found := roleExclusiveTopologyAnnotation(role); found {
			allErrs = append(allErrs, field.Forbidden(
				rolesPath.Index(i),
				fmt.Sprintf("the %q annotation must not be combined with a non-None spec.placementPolicy.type (%s)",
					key, policy.Type)))
		}
	}

	return allErrs
}

// exclusiveTopologyAnnotationKeys are the LWS annotations that make the LWS pod
// webhook inject its own exclusive-placement affinity, at the group and subgroup
// level respectively. Either one conflicts with a DisaggregatedSet placement policy.
var exclusiveTopologyAnnotationKeys = []string{
	leaderworkerset.ExclusiveKeyAnnotationKey,
	leaderworkerset.SubGroupExclusiveKeyAnnotationKey,
}

// roleExclusiveTopologyAnnotation returns the first LWS exclusive-topology annotation
// (group or subgroup level) a role carries anywhere it takes effect: the LWS metadata,
// or the leader/worker pod templates (the LWS pod webhook reads these from the pod, so
// a template-level annotation would enable LWS exclusive placement too).
func roleExclusiveTopologyAnnotation(role disaggv1.DisaggregatedRoleSpec) (string, bool) {
	template := role.Spec.LeaderWorkerTemplate
	for _, key := range exclusiveTopologyAnnotationKeys {
		if _, ok := role.ObjectMeta.Annotations[key]; ok {
			return key, true
		}
		if template.LeaderTemplate != nil {
			if _, ok := template.LeaderTemplate.Annotations[key]; ok {
				return key, true
			}
		}
		if _, ok := template.WorkerTemplate.Annotations[key]; ok {
			return key, true
		}
	}
	return "", false
}

// validateRoleRolloutStrategy validates the RolloutStrategy fields for a role.
// DisaggregatedSet handles rolling updates differently from LWS and does not support:
// - RolloutStrategy.Type other than RollingUpdate (or empty, which defaults to RollingUpdate)
// - RolloutStrategy.RollingUpdateConfiguration.Partition
func (w *DisaggregatedSetWebhook) validateRoleRolloutStrategy(role disaggv1.DisaggregatedRoleSpec, rolePath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	rolloutPath := rolePath.Child("spec", "rolloutStrategy")

	// Validate Type - must be empty or RollingUpdate
	if role.Spec.RolloutStrategy.Type != "" && role.Spec.RolloutStrategy.Type != leaderworkerset.RollingUpdateStrategyType {
		allErrs = append(allErrs, field.NotSupported(
			rolloutPath.Child("type"),
			role.Spec.RolloutStrategy.Type,
			[]string{string(leaderworkerset.RollingUpdateStrategyType), ""},
		))
	}

	if role.Spec.RolloutStrategy.RollingUpdateConfiguration != nil {
		rucPath := rolloutPath.Child("rollingUpdateConfiguration")
		ruc := role.Spec.RolloutStrategy.RollingUpdateConfiguration

		// Validate Partition - must not be set (DisaggregatedSet manages rollouts across roles)
		if ruc.Partition != nil && *ruc.Partition != 0 {
			allErrs = append(allErrs, field.Forbidden(
				rucPath.Child("partition"),
				"partition is not supported by DisaggregatedSet; rolling updates are managed across roles by the DisaggregatedSet controller",
			))
		}

		// External roles ignore spec.replicas, so their target is unknown.
		replicas := ptr.Deref(validationReplicasForRole(role), 0)
		if replicas > 0 && role.Scaling != nil && role.Scaling.Mode == disaggv1.RoleScalingExternal {
			allErrs = append(allErrs, webhooks.ValidateUnknownTargetRollingUpdateBudget(ruc, rucPath)...)
		} else {
			allErrs = append(allErrs, webhooks.ValidateRollingUpdateBudget(ruc, replicas, rucPath)...)
		}
	}

	return allErrs
}

func validateSubRoleRolloutStrategy(role *disaggv1.DisaggregatedRoleSpec, child *disaggv1.DisaggregatedSubRoleSpec, path *field.Path) field.ErrorList {
	if child.MaxSurge == nil && child.MaxUnavailable == nil {
		// Preserve inherited budgets, including the existing runtime fallback
		// when both resolved budgets are zero for a small child.
		return nil
	}
	config := disaggregatedsetutils.SubRoleRolloutConfig(role, child)
	if child.Scaling != nil && child.Scaling.Mode == disaggv1.RoleScalingExternal {
		return webhooks.ValidateUnknownTargetRollingUpdateBudget(&config, path)
	}
	return webhooks.ValidateRollingUpdateBudget(&config, ptr.Deref(child.Replicas, 1), path)
}
