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

package controllers

import (
	"encoding/json"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
)

const (
	combinedRolloutAnnotation = "leaderworkerset.sigs.k8s.io/combined-rollout"
	// Bound persisted state independently of replica count. A full reservation
	// window pauses further suffix exposure until existing replacements recover.
	combinedReservationLimit = 64
	combinedStateLimit       = 24 * 1024
	combinedFreeze           = "freeze"
	combinedPublish          = "publish"
	combinedActive           = "active"
)

// These types retain the experiment's names so its exhaustive tests exercise
// precisely the production planner, rather than a duplicate test-only model.
type combinedModelReservation struct {
	UID      types.UID `json:"uid"`
	Revision string    `json:"revision"`
}

// Baseline is the initial non-surge target, never observed Ready capacity.
// Phase barriers belong to the actuator; the planner cannot acknowledge them.
type combinedModelState struct {
	Version        int                                `json:"v"`
	Baseline       int32                              `json:"b"`
	Desired        int32                              `json:"d"`
	Generation     int64                              `json:"g"`
	Revision       string                             `json:"r"`
	NativeRevision string                             `json:"native,omitempty"`
	TargetTemplate string                             `json:"template,omitempty"`
	Protected      int32                              `json:"protected,omitempty"`
	Phase          string                             `json:"phase,omitempty"`
	Reservations   map[int32]combinedModelReservation `json:"pending,omitempty"`
}

type combinedModelGroup struct {
	leader  *corev1.Pod
	workers *appsv1.StatefulSet
	pods    []corev1.Pod
}

func combinedModelOwnedBy(obj metav1.Object, kind string, uid types.UID) bool {
	owner := metav1.GetControllerOf(obj)
	return uid != "" && owner != nil && owner.Kind == kind && owner.UID == uid
}

// A group's size is stamped on its leader, including protected old revisions.
// Termination, stale owners and stale aggregate status never provide credit.
func (g combinedModelGroup) ready(stsUID types.UID) bool {
	p := g.leader
	if p == nil || p.DeletionTimestamp != nil || !combinedModelOwnedBy(p, "StatefulSet", stsUID) || !podutils.PodRunningAndReady(*p) {
		return false
	}
	size, err := strconv.Atoi(p.Annotations[leaderworkerset.SizeAnnotationKey])
	if err != nil || size < 1 || p.Labels[leaderworkerset.RevisionKey] == "" {
		return false
	}
	if size == 1 {
		return true
	}
	w := g.workers
	if w == nil || w.DeletionTimestamp != nil || !combinedModelOwnedBy(w, "Pod", p.UID) ||
		w.Spec.Replicas == nil || int(*w.Spec.Replicas) != size-1 || w.Status.ObservedGeneration < w.Generation ||
		w.Status.AvailableReplicas != *w.Spec.Replicas || w.Status.UpdateRevision == "" || w.Status.CurrentRevision != w.Status.UpdateRevision ||
		w.Labels[leaderworkerset.RevisionKey] != p.Labels[leaderworkerset.RevisionKey] {
		return false
	}
	seen := make(map[int]bool)
	for _, worker := range g.pods {
		i, err := strconv.Atoi(worker.Labels[leaderworkerset.WorkerIndexLabelKey])
		if err != nil || i < 1 || i >= size || worker.Name != fmt.Sprintf("%s-%d", p.Name, i) ||
			!combinedModelOwnedBy(&worker, "StatefulSet", w.UID) || worker.DeletionTimestamp != nil ||
			worker.Labels[leaderworkerset.RevisionKey] != p.Labels[leaderworkerset.RevisionKey] ||
			worker.Labels[appsv1.ControllerRevisionHashLabelKey] != w.Status.UpdateRevision || !podutils.PodRunningAndReady(worker) {
			continue
		}
		seen[i] = true
	}
	return len(seen) == size-1
}

type combinedModelInput struct {
	baseline, desired, replicas, partition, protected int32
	generation                                        int64
	revision                                          string
	nativeRevision                                    string
	stsUID                                            types.UID
	unavailable, surge                                intstr.IntOrString
	groups                                            []combinedModelGroup
	state                                             string
	// Set only after the actuator's freeze/publish generation barriers.
	templateFrozen bool
}

type combinedModelPlan struct {
	partition, replicas, floor, credited int32
	state                                string
	deletes                              []*corev1.Pod
	freeze, done, blocked                bool
}

func combinedModelDecode(raw string) (combinedModelState, error) {
	var s combinedModelState
	if len(raw) > combinedStateLimit {
		return s, fmt.Errorf("combined rollout state too large")
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, err
	}
	if s.Version != 1 || s.Baseline < 0 || s.Desired < 0 || s.Protected < 0 || s.Generation < 1 || s.Revision == "" || len(s.Revision) > 128 || len(s.NativeRevision) > 253 || len(s.TargetTemplate) > 64 || len(s.Reservations) > combinedReservationLimit {
		return s, fmt.Errorf("invalid combined rollout state")
	}
	if s.Phase != "" && s.Phase != combinedFreeze && s.Phase != combinedPublish && s.Phase != combinedActive {
		return s, fmt.Errorf("invalid combined rollout phase")
	}
	for i, reservation := range s.Reservations {
		if i < 0 || reservation.UID == "" || len(reservation.UID) > 128 || reservation.Revision == "" || len(reservation.Revision) > 128 {
			return s, fmt.Errorf("invalid reservation at %d", i)
		}
	}
	if s.Reservations == nil {
		s.Reservations = make(map[int32]combinedModelReservation)
	}
	return s, nil
}

func combinedModelEncode(s combinedModelState) string {
	b, _ := json.Marshal(s) // all fields are JSON-supported types
	return string(b)
}

func combinedLeaderAtTarget(p *corev1.Pod, revision, nativeRevision string) bool {
	return p != nil && nativeRevision != "" && p.Labels[leaderworkerset.RevisionKey] == revision &&
		p.Labels[appsv1.ControllerRevisionHashLabelKey] == nativeRevision
}

// Reserve ALL stale leaders in the exposed suffix, not only direct-delete
// targets: the native controller may act independently. This is a bound on
// controller-authorized disruption using observed health, not on failures.
func combinedModelPlanUpdate(in combinedModelInput) (combinedModelPlan, error) {
	p := combinedModelPlan{partition: max(in.partition, in.protected), replicas: in.replicas}
	if in.baseline < 0 || in.desired < 0 || in.replicas < 0 || in.protected < 0 || in.generation < 1 || in.revision == "" || in.nativeRevision == "" || in.stsUID == "" {
		return p, fmt.Errorf("invalid rollout input")
	}
	u, err := intstr.GetScaledValueFromIntOrPercent(&in.unavailable, int(in.desired), false)
	if err != nil || u < 0 {
		return p, fmt.Errorf("invalid maxUnavailable")
	}
	surge, err := intstr.GetScaledValueFromIntOrPercent(&in.surge, int(in.desired), true)
	if err != nil || surge < 0 || (in.desired > 0 && u == 0 && surge == 0) {
		return p, fmt.Errorf("invalid maxSurge or zero resolved budgets")
	}
	s := combinedModelState{Version: 1, Baseline: in.baseline, Desired: in.desired, Generation: in.generation, Revision: in.revision, NativeRevision: in.nativeRevision, Reservations: make(map[int32]combinedModelReservation)}
	if in.state != "" {
		s, err = combinedModelDecode(in.state)
		if err != nil || s.Generation > in.generation {
			return p, fmt.Errorf("invalid or newer persisted state; do not apply or delete")
		}
	}
	p.floor = max(0, min(s.Baseline, in.desired)-int32(min(u, int(in.desired))))
	if (s.Revision != in.revision || s.NativeRevision != in.nativeRevision) && !in.templateFrozen {
		p.freeze, p.partition, p.state = true, max(in.replicas, in.protected), combinedModelEncode(s)
		return p, nil
	}
	// Validate the OLD exposed suffix before retiring any condemned obligations.
	// The replica reduction and reservation removal are then persisted atomically.
	for i := in.partition; i < min(in.replicas, int32(len(in.groups))); i++ {
		g := in.groups[i]
		if g.leader != nil && !combinedLeaderAtTarget(g.leader, in.revision, in.nativeRevision) {
			if _, ok := s.Reservations[i]; !ok && in.state != "" && !in.templateFrozen {
				return combinedModelPlan{}, fmt.Errorf("unreserved stale leader exposed at %d", i)
			}
		}
	}
	p.replicas = in.desired
	if in.desired >= s.Desired {
		p.replicas = max(in.desired, min(in.replicas, in.desired+int32(min(surge, int(in.desired)))))
	}
	for i, reservation := range s.Reservations {
		// Supersession changes the replacement obligation, not its payment.
		// Keep the old UID until a whole group at the new target is Ready.
		if s.Revision != in.revision {
			reservation.Revision = in.revision
			s.Reservations[i] = reservation
		}
		if i >= p.replicas {
			delete(s.Reservations, i) // condemned Pods cannot supply credit below
			continue
		}
		if i < int32(len(in.groups)) {
			g := in.groups[i]
			if g.ready(in.stsUID) && g.leader.UID != reservation.UID && combinedLeaderAtTarget(g.leader, reservation.Revision, in.nativeRevision) {
				delete(s.Reservations, i)
			}
		}
	}
	s.Desired, s.Generation, s.Revision = in.desired, in.generation, in.revision
	s.NativeRevision, s.Protected = in.nativeRevision, in.protected
	converged := true
	for i := int32(0); i < p.replicas; i++ {
		if i >= int32(len(in.groups)) {
			if i < in.desired {
				converged = false
			}
			continue
		}
		g := in.groups[i]
		_, reserved := s.Reservations[i]
		if g.ready(in.stsUID) && !reserved {
			p.credited++
		}
		if i < in.desired && (!g.ready(in.stsUID) || reserved || (i >= in.protected && !combinedLeaderAtTarget(g.leader, in.revision, in.nativeRevision))) {
			converged = false
		}
	}
	if converged && len(s.Reservations) == 0 {
		p.done, p.partition, p.replicas = true, in.protected, in.desired
		return p, nil
	}
	p.partition = max(p.replicas, in.protected)
	for i := p.replicas - 1; i >= in.protected; i-- {
		if i >= int32(len(in.groups)) || in.groups[i].leader == nil {
			// Below the previous target a native sync could still create an old
			// Pod. Reserve that zero-credit hole before exposing it. Fresh scale
			// additions cannot have been created by a sync of the smaller set.
			if i < in.replicas {
				if _, ok := s.Reservations[i]; !ok {
					if len(s.Reservations) == combinedReservationLimit {
						p.blocked = true
						break
					}
					s.Reservations[i] = combinedModelReservation{UID: "missing", Revision: in.revision}
				}
			}
			p.partition = i
			continue
		}
		g, leader := in.groups[i], in.groups[i].leader
		if !combinedModelOwnedBy(leader, "StatefulSet", in.stsUID) || leader.UID == "" || leader.ResourceVersion == "" {
			p.blocked = true
			break
		}
		if combinedLeaderAtTarget(leader, in.revision, in.nativeRevision) {
			p.partition = i
			continue
		}
		_, reserved := s.Reservations[i]
		if !reserved {
			if len(s.Reservations) == combinedReservationLimit {
				p.blocked = true
				break
			}
			if g.ready(in.stsUID) {
				if p.credited <= p.floor {
					p.blocked = true
					break
				}
				p.credited--
			}
		}
		s.Reservations[i] = combinedModelReservation{UID: leader.UID, Revision: in.revision}
		p.partition = i
		if leader.DeletionTimestamp == nil {
			p.deletes = append(p.deletes, leader.DeepCopy())
		}
	}
	// Pending desired additions already offer additional credit. Only add surge
	// when growth no longer offers it; do not reclaim counted surge until done.
	if len(p.deletes) == 0 && in.desired <= s.Baseline && !converged {
		p.replicas = in.desired + int32(min(surge, int(in.desired)))
	}
	p.state = combinedModelEncode(s)
	return p, nil
}
