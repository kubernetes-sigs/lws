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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

const NumRequiredRoles = 2

func GetInitialReplicas(leaderWorkerSet *leaderworkersetv1.LeaderWorkerSet) (int32, bool) {
	if leaderWorkerSet.Annotations == nil {
		return 0, false
	}
	value, exists := leaderWorkerSet.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey]
	if !exists || value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(parsed), true
}

func SetInitialReplicas(leaderWorkerSet *leaderworkersetv1.LeaderWorkerSet, replicas int32) {
	if leaderWorkerSet.Annotations == nil {
		leaderWorkerSet.Annotations = make(map[string]string)
	}
	leaderWorkerSet.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey] = strconv.FormatInt(int64(replicas), 10)
}

func ComputeInitialReplicaState(lwsList []leaderworkersetv1.LeaderWorkerSet) map[string]int {
	state := make(map[string]int)

	for i := range lwsList {
		lws := &lwsList[i]
		role := lws.Labels[disaggregatedsetv1.RoleLabelKey]
		if role == "" {
			continue
		}

		var replicas int
		replicasInt32, ok := GetInitialReplicas(lws)
		if ok {
			replicas = int(replicasInt32)
		} else {
			if lws.Spec.Replicas != nil {
				replicas = int(*lws.Spec.Replicas)
			} else {
				replicas = 1
			}
		}

		state[role] += replicas
	}

	return state
}

type CreateParams struct {
	DisaggregatedSet *disaggregatedsetv1.DisaggregatedSet
	Role             string
	Slice            int
	Config           *disaggregatedsetv1.DisaggregatedRoleSpec
	Revision         string
	Labels           map[string]string
	Replicas         int
}

func GenerateName(baseName string, slice int, revision, role string) string {
	return fmt.Sprintf("%s-%d-%s-%s", baseName, slice, revision, role)
}

// GenerateLegacyName returns the pre-slices LWS name (no slice segment). Used to
// find and adopt objects created before the slices feature, which belong to slice 0.
func GenerateLegacyName(baseName, revision, role string) string {
	return fmt.Sprintf("%s-%s-%s", baseName, revision, role)
}

func GenerateLabels(baseName string, slice int, revision, role string) map[string]string {
	return map[string]string{
		"app":                               fmt.Sprintf("%s-%d-%s", baseName, slice, role),
		disaggregatedsetv1.RoleLabelKey:     role,
		disaggregatedsetv1.SliceLabelKey:    strconv.Itoa(slice),
		disaggregatedsetv1.SetNameLabelKey:  baseName,
		disaggregatedsetv1.RevisionLabelKey: revision,
	}
}

// GetSlices returns the desired slice count, defaulting to 1.
func GetSlices(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet) int32 {
	if disaggregatedSet.Spec.Slices == nil {
		return 1
	}
	return *disaggregatedSet.Spec.Slices
}

// SliceLabelMatches reports whether an object's labels place it in the given slice.
// A slice < 0 matches all slices. Slice 0 also matches a legacy object that carries
// no slice label, so pre-slices objects are adopted as slice 0.
func SliceLabelMatches(labels map[string]string, slice int) bool {
	if slice < 0 {
		return true
	}
	value, ok := labels[disaggregatedsetv1.SliceLabelKey]
	if !ok || value == "" {
		return slice == 0
	}
	return value == strconv.Itoa(slice)
}

// HasSliceLabel reports whether an object carries a non-empty slice label, i.e. it
// was created by the slices-aware controller rather than a pre-slices release.
func HasSliceLabel(labels map[string]string) bool {
	value, ok := labels[disaggregatedsetv1.SliceLabelKey]
	return ok && value != ""
}

const revisionLength = 8

func ComputeRevision(roles []disaggregatedsetv1.DisaggregatedRoleSpec) string {
	type roleTemplate struct {
		Name     string                                 `json:"name"`
		Template leaderworkersetv1.LeaderWorkerTemplate `json:"template"`
	}

	templates := make([]roleTemplate, 0, len(roles))
	for _, role := range roles {
		templates = append(templates, roleTemplate{
			Name:     role.Name,
			Template: role.Spec.LeaderWorkerTemplate,
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

func GetRoleConfigs(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet) map[string]*disaggregatedsetv1.DisaggregatedRoleSpec {
	roleConfigs := make(map[string]*disaggregatedsetv1.DisaggregatedRoleSpec)

	for i := range disaggregatedSet.Spec.Roles {
		role := &disaggregatedSet.Spec.Roles[i]
		roleConfigs[role.Name] = role
	}

	return roleConfigs
}

func GetRoleNames(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet) []string {
	names := make([]string, len(disaggregatedSet.Spec.Roles))
	for i, role := range disaggregatedSet.Spec.Roles {
		names[i] = role.Name
	}
	return names
}

type RevisionRoles struct {
	Revision string
	Roles    map[string]*leaderworkersetv1.LeaderWorkerSet
}

type RevisionRolesList []RevisionRoles

func getLWSReplicas(lws *leaderworkersetv1.LeaderWorkerSet) int {
	if lws.Spec.Replicas == nil {
		return 1
	}
	return int(*lws.Spec.Replicas)
}

func (revisions RevisionRolesList) GetTotalReplicasPerRole(role string) int {
	total := 0
	for _, rev := range revisions {
		if lws := rev.Roles[role]; lws != nil {
			total += getLWSReplicas(lws)
		}
	}
	return total
}

func (revisions RevisionRolesList) GetTotalInitialReplicasPerRole(role string) int {
	total := 0
	for _, rev := range revisions {
		if lws := rev.Roles[role]; lws != nil {
			initialReplicas, ok := GetInitialReplicas(lws)
			if ok {
				total += int(initialReplicas)
			} else {
				total += getLWSReplicas(lws)
			}
		}
	}
	return total
}

func GroupByRevision(lwsList []*leaderworkersetv1.LeaderWorkerSet) RevisionRolesList {
	byRevision := make(map[string]*RevisionRoles)
	for _, lws := range lwsList {
		revision := lws.Labels[disaggregatedsetv1.RevisionLabelKey]
		role := lws.Labels[disaggregatedsetv1.RoleLabelKey]
		if byRevision[revision] == nil {
			byRevision[revision] = &RevisionRoles{
				Revision: revision,
				Roles:    make(map[string]*leaderworkersetv1.LeaderWorkerSet),
			}
		}
		byRevision[revision].Roles[role] = lws
	}

	result := make(RevisionRolesList, 0, len(byRevision))
	for _, grouped := range byRevision {
		result = append(result, *grouped)
	}
	return result
}
