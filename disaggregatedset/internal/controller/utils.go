/*
Copyright 2026.

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

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

// NumRequiredRoles is the number of roles required in the DisaggregatedSet spec.
// The planner currently only supports exactly 2 roles.
const NumRequiredRoles = 2

// Label keys used for workload management
const (
	LabelDisaggRole = "disaggregatedset.x-k8s.io/role"
	LabelDisaggName = "disaggregatedset.x-k8s.io/name"
	LabelRevision   = "disaggregatedset.x-k8s.io/revision"
)

// Annotation keys used for workload management
const (
	// AnnotationInitialReplicas stores the initial/stable replica count for a LWS.
	// This is used to track the original replica count during rolling updates.
	// Value is an integer stored as a string (e.g., "3").
	AnnotationInitialReplicas = "disaggregatedset.x-k8s.io/initial-replicas"
)

// GetInitialReplicas retrieves the initial replica count from the LWS annotation.
// Returns the replica count and true if the annotation exists and is valid.
// Returns 0 and false if the annotation is missing, empty, or cannot be parsed as an integer.
func GetInitialReplicas(leaderWorkerSet *leaderworkerset.LeaderWorkerSet) (int32, bool) {
	if leaderWorkerSet.Annotations == nil {
		return 0, false
	}
	value, exists := leaderWorkerSet.Annotations[AnnotationInitialReplicas]
	if !exists || value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(parsed), true
}

// SetInitialReplicas sets the initial replica count annotation on a LWS.
// The replica count is stored as a string.
func SetInitialReplicas(leaderWorkerSet *leaderworkerset.LeaderWorkerSet, replicas int32) {
	if leaderWorkerSet.Annotations == nil {
		leaderWorkerSet.Annotations = make(map[string]string)
	}
	leaderWorkerSet.Annotations[AnnotationInitialReplicas] = strconv.FormatInt(int64(replicas), 10)
}

// ComputeInitialReplicaState computes the total initial replica counts
// from a list of LWS resources by summing their initial-replicas annotations.
// If an annotation is missing or invalid, it falls back to spec.Replicas.
// Returns a map from role name to replica count.
func ComputeInitialReplicaState(lwsList []leaderworkerset.LeaderWorkerSet) map[string]int {
	state := make(map[string]int)

	for i := range lwsList {
		lws := &lwsList[i]
		role := lws.Labels[LabelDisaggRole]
		if role == "" {
			continue
		}

		// Try to get initial replicas from annotation
		var replicas int
		replicasInt32, ok := GetInitialReplicas(lws)
		if ok {
			replicas = int(replicasInt32)
		} else {
			// Fallback to spec.Replicas
			if lws.Spec.Replicas != nil {
				replicas = int(*lws.Spec.Replicas)
			} else {
				replicas = 1 // Default if nil
			}
		}

		state[role] += replicas
	}

	return state
}

// WorkloadInfo represents the current state of a workload
type WorkloadInfo struct {
	// Name is the workload resource name
	Name string
	// Namespace is the workload namespace
	Namespace string
	// Role is the disaggregated role (prefill or decode)
	Role string
	// Revision is the revision identifier for this workload
	Revision string
	// Replicas is the desired replica count
	Replicas int
	// ReadyReplicas is the number of ready replicas
	ReadyReplicas int
	// InitialReplicas is the initial replica count from annotation (for rolling update planning)
	InitialReplicas int
	// HasInitialReplicasAnnotation indicates if InitialReplicas came from annotation (true) or fallback (false)
	HasInitialReplicasAnnotation bool
	// CreationTimestamp is when the workload was created
	CreationTimestamp time.Time
}

// CreateParams groups parameters for workload creation
type CreateParams struct {
	// DisaggregatedSet is the parent resource
	DisaggregatedSet *disaggv1alpha1.DisaggregatedSet
	// Role is the disaggregated role (prefill or decode)
	Role string
	// Config is the role configuration
	Config *disaggv1alpha1.DisaggregatedRoleSpec
	// Revision is the revision identifier for this workload
	Revision string
	// Labels are the labels to apply to the workload
	Labels map[string]string
	// Replicas is the desired replica count
	Replicas int
}

// GenerateName generates a unique name for the workload based on revision.
// Format: {baseName}-{revision}-{role}
func GenerateName(baseName, role, revision string) string {
	return fmt.Sprintf("%s-%s-%s", baseName, revision, role)
}

// GenerateLabels generates the standard labels for a role
func GenerateLabels(baseName, role, revision string) map[string]string {
	return map[string]string{
		"app":           fmt.Sprintf("%s-%s", baseName, role),
		LabelDisaggRole: role,
		LabelDisaggName: baseName,
		LabelRevision:   revision,
	}
}

// revisionLength is the length of the truncated revision identifier used in resource names
const revisionLength = 8

// ComputeRevision computes a truncated revision identifier from all role LeaderWorkerTemplates.
// This ensures all roles roll together when any field in LeaderWorkerTemplate changes
// (including Size, LeaderTemplate, WorkerTemplate, RestartPolicy, SubGroupPolicy, etc.).
// Returns an 8-character revision identifier suitable for use in resource names.
func ComputeRevision(roles []disaggv1alpha1.DisaggregatedRoleSpec) string {
	// Create a slice of templates keyed by role name for deterministic ordering
	type roleTemplate struct {
		Name     string                               `json:"name"`
		Template leaderworkerset.LeaderWorkerTemplate `json:"template"`
	}

	templates := make([]roleTemplate, 0, len(roles))
	for _, role := range roles {
		templates = append(templates, roleTemplate{
			Name:     role.Name,
			Template: role.LeaderWorkerTemplate,
		})
	}

	jsonData, err := json.Marshal(templates)
	if err != nil {
		return ""
	}

	hash := sha256.Sum256(jsonData)
	fullHash := hex.EncodeToString(hash[:])
	if len(fullHash) >= revisionLength {
		return fullHash[:revisionLength]
	}
	return fullHash
}

// GetRoleConfigs returns a map of role configurations from the DisaggregatedSet spec.
// The map is keyed by role name.
func GetRoleConfigs(disaggregatedSet *disaggv1alpha1.DisaggregatedSet) map[string]*disaggv1alpha1.DisaggregatedRoleSpec {
	roleConfigs := make(map[string]*disaggv1alpha1.DisaggregatedRoleSpec)

	for i := range disaggregatedSet.Spec.Roles {
		role := &disaggregatedSet.Spec.Roles[i]
		roleConfigs[role.Name] = role
	}

	return roleConfigs
}

// GetRoleNames returns the ordered list of role names from the spec.
func GetRoleNames(disaggregatedSet *disaggv1alpha1.DisaggregatedSet) []string {
	names := make([]string, len(disaggregatedSet.Spec.Roles))
	for i, role := range disaggregatedSet.Spec.Roles {
		names[i] = role.Name
	}
	return names
}
