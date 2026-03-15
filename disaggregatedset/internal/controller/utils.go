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

// NumRequiredPhases is the number of phases required in the DisaggregatedSet spec.
// The planner currently only supports exactly 2 phases.
const NumRequiredPhases = 2

// Label keys used for workload management
const (
	LabelDisaggPhase = "disaggregatedset.x-k8s.io/phase"
	LabelDisaggName  = "disaggregatedset.x-k8s.io/name"
	LabelRevision    = "disaggregatedset.x-k8s.io/revision"
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
// Returns a map from phase name to replica count.
func ComputeInitialReplicaState(lwsList []leaderworkerset.LeaderWorkerSet) map[string]int {
	state := make(map[string]int)

	for i := range lwsList {
		lws := &lwsList[i]
		phase := lws.Labels[LabelDisaggPhase]
		if phase == "" {
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

		state[phase] += replicas
	}

	return state
}

// WorkloadInfo represents the current state of a workload
type WorkloadInfo struct {
	// Name is the workload resource name
	Name string
	// Namespace is the workload namespace
	Namespace string
	// Phase is the disaggregated phase (prefill or decode)
	Phase string
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
	// Phase is the disaggregated phase (prefill or decode)
	Phase string
	// Config is the phase configuration
	Config *disaggv1alpha1.DisaggregatedPhaseSpec
	// Revision is the revision identifier for this workload
	Revision string
	// Labels are the labels to apply to the workload
	Labels map[string]string
	// Replicas is the desired replica count
	Replicas int
}

// GenerateName generates a unique name for the workload based on revision.
// Format: {baseName}-{revision}-{phase}
func GenerateName(baseName, phase, revision string) string {
	return fmt.Sprintf("%s-%s-%s", baseName, revision, phase)
}

// GenerateLabels generates the standard labels for a phase
func GenerateLabels(baseName, phase, revision string) map[string]string {
	return map[string]string{
		"app":            fmt.Sprintf("%s-%s", baseName, phase),
		LabelDisaggPhase: phase,
		LabelDisaggName:  baseName,
		LabelRevision:    revision,
	}
}

// revisionLength is the length of the truncated revision identifier used in resource names
const revisionLength = 8

// ComputeRevision computes a truncated revision identifier from all phase LeaderWorkerTemplates.
// This ensures all phases roll together when any field in LeaderWorkerTemplate changes
// (including Size, LeaderTemplate, WorkerTemplate, RestartPolicy, SubGroupPolicy, etc.).
// Returns an 8-character revision identifier suitable for use in resource names.
func ComputeRevision(phases []disaggv1alpha1.DisaggregatedPhaseSpec) string {
	// Create a slice of templates keyed by phase name for deterministic ordering
	type phaseTemplate struct {
		Name     string                               `json:"name"`
		Template leaderworkerset.LeaderWorkerTemplate `json:"template"`
	}

	templates := make([]phaseTemplate, 0, len(phases))
	for _, phase := range phases {
		templates = append(templates, phaseTemplate{
			Name:     phase.Name,
			Template: phase.LeaderWorkerTemplate,
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

// GetPhaseConfigs returns a map of phase configurations from the DisaggregatedSet spec.
// The map is keyed by phase name.
func GetPhaseConfigs(disaggregatedSet *disaggv1alpha1.DisaggregatedSet) map[string]*disaggv1alpha1.DisaggregatedPhaseSpec {
	phaseConfigs := make(map[string]*disaggv1alpha1.DisaggregatedPhaseSpec)

	for i := range disaggregatedSet.Spec.Phases {
		phase := &disaggregatedSet.Spec.Phases[i]
		phaseConfigs[phase.Name] = phase
	}

	return phaseConfigs
}

// GetPhaseNames returns the ordered list of phase names from the spec.
func GetPhaseNames(disaggregatedSet *disaggv1alpha1.DisaggregatedSet) []string {
	names := make([]string, len(disaggregatedSet.Spec.Phases))
	for i, phase := range disaggregatedSet.Spec.Phases {
		names[i] = phase.Name
	}
	return names
}
