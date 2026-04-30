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
	"time"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

const NumRequiredRoles = 2

const (
	LabelDisaggRole = "disaggregatedset.x-k8s.io/role"
	LabelDisaggName = "disaggregatedset.x-k8s.io/name"
	LabelRevision   = "disaggregatedset.x-k8s.io/revision"
)

const (
	AnnotationInitialReplicas = "disaggregatedset.x-k8s.io/initial-replicas"
)

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

func SetInitialReplicas(leaderWorkerSet *leaderworkerset.LeaderWorkerSet, replicas int32) {
	if leaderWorkerSet.Annotations == nil {
		leaderWorkerSet.Annotations = make(map[string]string)
	}
	leaderWorkerSet.Annotations[AnnotationInitialReplicas] = strconv.FormatInt(int64(replicas), 10)
}

func ComputeInitialReplicaState(lwsList []leaderworkerset.LeaderWorkerSet) map[string]int {
	state := make(map[string]int)

	for i := range lwsList {
		lws := &lwsList[i]
		role := lws.Labels[LabelDisaggRole]
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

type WorkloadInfo struct {
	Name                         string
	Namespace                    string
	Role                         string
	Revision                     string
	Replicas                     int
	ReadyReplicas                int
	InitialReplicas              int
	HasInitialReplicasAnnotation bool
	CreationTimestamp             time.Time
}

type CreateParams struct {
	DisaggregatedSet *disaggregatedsetv1.DisaggregatedSet
	Role             string
	Config           *disaggregatedsetv1.DisaggregatedRoleSpec
	Revision         string
	Labels           map[string]string
	Replicas         int
}

func GenerateName(baseName, role, revision string) string {
	return fmt.Sprintf("%s-%s-%s", baseName, revision, role)
}

func GenerateLabels(baseName, role, revision string) map[string]string {
	return map[string]string{
		"app":           fmt.Sprintf("%s-%s", baseName, role),
		LabelDisaggRole: role,
		LabelDisaggName: baseName,
		LabelRevision:   revision,
	}
}

const revisionLength = 8

func ComputeRevision(roles []disaggregatedsetv1.DisaggregatedRoleSpec) string {
	type roleTemplate struct {
		Name     string                               `json:"name"`
		Template leaderworkerset.LeaderWorkerTemplate `json:"template"`
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
