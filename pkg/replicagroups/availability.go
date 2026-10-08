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

package replicagroups

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// Availability separates observed whole-group readiness from retained credit.
// Neither value is a reservation against later failures or rollout actions.
type Availability struct {
	ReadyReplicas         int32 // Includes Ready groups committed to removal.
	RetainedReadyReplicas int32 // Conservative Ready credit after observed scale-downs.
}

// Availability computes Ready capacity from this observation without any writes.
// Acknowledged generations prevent crediting unapplied scale intent; they do
// not mean all terminating Pods have disappeared. Those are excluded individually.
// Until the acknowledgement chain is complete, retained credit is zero, without
// hiding the detailed groups from other consumers.
//
// Retained credit accounts for native scale-downs, not independent Pod removals.
// A concurrent health replacement can leave a ReplicaSet retry using an older
// Pod cohort, even after its generation is acknowledged. Neither this ordered
// observation nor status counts fence those hidden victims. Callers requiring
// specific survivors must enforce deletion authority, not treat this as a lock.
//
// For Hash identity, native ReplicaSets choose their victims dynamically. Each
// reserves only its remaining unissued deletions, using the same Pod list as
// readiness: max(0, readyNonTerminatingGroups - max(0, activeLeaders - target)).
// Already terminating leaders must not be reserved a second time. Neither
// active counts nor Ready counts are taken from asynchronous status counters.
func (s *Snapshot) Availability() Availability {
	lws := s.LWS
	replicas := ptr.Deref(lws.Spec.Replicas, 1)
	result := Availability{}
	// LWS acknowledgement is needed as well as native-controller acknowledgements:
	// a child still at 3 must not look current during an unapplied 3 -> 2 -> 3 ABA.
	acknowledged := lws.DeletionTimestamp.IsZero() && lws.Status.ObservedGeneration >= lws.Generation
	targets := make(map[types.UID]int32)
	start := 0
	hash := lws.Spec.GroupIdentity == leaderworkersetv1.GroupIdentityHash
	if hash {
		deployment := s.LeaderDeployment
		if deployment == nil {
			return result
		}
		acknowledged = acknowledged && deployment.DeletionTimestamp.IsZero() &&
			ptr.Deref(deployment.Spec.Replicas, 1) == replicas &&
			deployment.Status.ObservedGeneration >= deployment.Generation
		issued := int32(0)
		for _, rs := range s.ReplicaSets {
			target := ptr.Deref(rs.Spec.Replicas, 1)
			if !rs.DeletionTimestamp.IsZero() {
				target = 0
			}
			targets[rs.UID] = target
			issued += target
			acknowledged = acknowledged && rs.Status.ObservedGeneration >= rs.Generation
		}
		// Deployment status only acknowledges writes to ReplicaSets, not their
		// Pod deletions. Verify the actual Pod controllers and their targets too.
		acknowledged = acknowledged && issued == replicas
	} else {
		sts := s.LeaderStatefulSet
		if sts == nil {
			return result
		}
		acknowledged = acknowledged && sts.DeletionTimestamp.IsZero() &&
			ptr.Deref(sts.Spec.Replicas, 1) == replicas &&
			sts.Status.ObservedGeneration >= sts.Generation
		targets[sts.UID] = replicas
		if sts.Spec.Ordinals != nil {
			start = int(sts.Spec.Ordinals.Start)
		}
	}

	active, ready := make(map[types.UID]int32), make(map[types.UID]int32)
	for _, group := range s.Groups {
		leader := group.Leader
		owner := metav1.GetControllerOf(leader)
		if leader.DeletionTimestamp.IsZero() && leader.Status.Phase != corev1.PodSucceeded && leader.Status.Phase != corev1.PodFailed {
			active[owner.UID]++
		}
		if !group.Ready {
			continue
		}
		result.ReadyReplicas++
		if !acknowledged || group.Terminating ||
			leader.Annotations[leaderworkersetv1.GroupRestartBudgetExhaustedAnnotationKey] == "true" ||
			leader.UID != "" && leader.Annotations[leaderworkersetv1.GroupReplacementDeleteAnnotationKey] == string(leader.UID) {
			continue
		}
		if workers := group.WorkerStatefulSet; workers != nil && workers.Status.ObservedGeneration < workers.Generation {
			continue
		}
		if !hash && (group.Ordinal < start || group.Ordinal >= start+int(replicas)) {
			continue
		}
		ready[owner.UID]++
	}
	for uid, target := range targets {
		credit := ready[uid]
		if hash {
			credit = max(0, credit-max(0, active[uid]-target))
		}
		result.RetainedReadyReplicas += credit
	}
	result.RetainedReadyReplicas = min(replicas, result.RetainedReadyReplicas)
	return result
}
