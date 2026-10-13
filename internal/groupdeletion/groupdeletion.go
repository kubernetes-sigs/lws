/*
Copyright 2026 The Kubernetes Authors.

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

// Package groupdeletion defines the internal, persisted protocol for protected
// group deletion. Annotation values remain compatible across controller restarts
// and upgrades even though they are not part of the public LWS Go API.
package groupdeletion

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ProtectionAnnotation binds persistent leader protection to "<LWS UID>/<Pod UID>".
	// Removing it is the explicit administrative override for deleting a protected group.
	ProtectionAnnotation = "leaderworkerset.sigs.k8s.io/scale-protection"
	// PlanAnnotation is the armed transaction ID on the parent, committed with its
	// replica count. It grants no deletion permission by itself.
	PlanAnnotation = "leaderworkerset.sigs.k8s.io/scale-plan"
	// VictimAnnotation selects a leader as "<plan ID>/<Pod UID>". Deletion requires
	// the parent to arm that plan and the Pod UID to match.
	VictimAnnotation = "leaderworkerset.sigs.k8s.io/scale-victim"
	// ReplacementAnnotation authorizes native replacement of this exact leader UID.
	// That group no longer contributes retained readiness.
	ReplacementAnnotation = "leaderworkerset.sigs.k8s.io/replacement-delete"
)

// ProtectionOwner returns the parent UID only when protection identifies this Pod.
func ProtectionOwner(pod *corev1.Pod) types.UID {
	marker := strings.Split(pod.Annotations[ProtectionAnnotation], "/")
	if pod.UID == "" || len(marker) != 2 || marker[0] == "" || marker[1] != string(pod.UID) {
		return ""
	}
	return types.UID(marker[0])
}

// IsVictim checks an exact Pod UID token for the supplied nonempty plan.
func IsVictim(pod *corev1.Pod, planID string) bool {
	return planID != "" && pod.UID != "" && pod.Annotations[VictimAnnotation] == planID+"/"+string(pod.UID)
}

// ReplacementPending reports native replacement intent for this exact Pod UID.
func ReplacementPending(pod *corev1.Pod) bool {
	return pod.UID != "" && pod.Annotations[ReplacementAnnotation] == string(pod.UID)
}

// DeleteForReplacement records native replacement intent before requesting
// foreground deletion. Protected groups must not retain readiness credit while
// their DELETE is in flight or waiting to be retried.
func DeleteForReplacement(ctx context.Context, c client.Client, leader *corev1.Pod) error {
	if leader.Annotations[ProtectionAnnotation] != "" {
		if leader.UID == "" {
			return fmt.Errorf("cannot authorize replacement of protected leader %s without its UID", leader.Name)
		}
		if !ReplacementPending(leader) {
			desired := leader.DeepCopy()
			// Persist only this annotation so unrelated metadata updates do not
			// conflict after the restart budget was saved. The UID condition
			// rejects a same-name replacement without a resource-version lock.
			data, _ := json.Marshal(map[string]any{"metadata": map[string]any{
				"uid": leader.UID, "annotations": map[string]string{ReplacementAnnotation: string(leader.UID)},
			}})
			if err := c.Patch(ctx, desired, client.RawPatch(types.MergePatchType, data)); err != nil {
				return err
			}
			*leader = *desired
		}
	}
	deletionOpt := metav1.DeletePropagationForeground
	opts := &client.DeleteOptions{PropagationPolicy: &deletionOpt}
	if leader.UID != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &leader.UID}
	}
	return c.Delete(ctx, leader, opts)
}
