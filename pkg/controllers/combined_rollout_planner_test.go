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
	"fmt"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
)

func combinedTestGroup(index int, revision string, ready bool) combinedModelGroup {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("example-%d", index), Namespace: "default",
			UID: types.UID(fmt.Sprintf("%s-%d", revision, index)), ResourceVersion: "10",
			Labels:          map[string]string{leaderworkerset.RevisionKey: revision, appsv1.ControllerRevisionHashLabelKey: "native-" + revision},
			Annotations:     map[string]string{leaderworkerset.SizeAnnotationKey: "1"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "example", UID: "leader-sts", Controller: ptr.To(true)}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return combinedModelGroup{leader: pod}
}

func combinedTestInput(baseline, desired int32, unavailable int) combinedModelInput {
	in := combinedModelInput{
		baseline: baseline, desired: desired, replicas: desired, partition: baseline,
		generation: 2, revision: "new", nativeRevision: "native-new", stsUID: "leader-sts",
		unavailable: intstr.FromInt(unavailable), surge: intstr.FromInt(0),
	}
	for i := int32(0); i < desired; i++ {
		revision, ready := "old", true
		if i >= baseline {
			revision, ready = "new", false
		}
		in.groups = append(in.groups, combinedTestGroup(int(i), revision, ready))
	}
	return in
}

func combinedTestPlan(t *testing.T, in combinedModelInput) combinedModelPlan {
	t.Helper()
	p, err := combinedModelPlanUpdate(in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func combinedDeleteNames(p combinedModelPlan) []string {
	names := []string{}
	for _, pod := range p.deletes {
		names = append(names, pod.Name)
	}
	return names
}

func TestCombinedPlannerBudgets(t *testing.T) {
	tests := []struct {
		name string
		in   combinedModelInput
		want []string
	}{
		{"one to two pending addition permits old replacement", combinedTestInput(1, 2, 1), []string{"example-0"}},
		{"four to eight default permits one paid replacement", combinedTestInput(4, 8, 1), []string{"example-3"}},
		{"four to eight budget two permits two replacements", combinedTestInput(4, 8, 2), []string{"example-3", "example-2"}},
	}
	zero := combinedTestInput(1, 2, 0)
	zero.surge = intstr.FromInt(1)
	tests = append(tests, struct {
		name string
		in   combinedModelInput
		want []string
	}{"zero budget waits for additional capacity", zero, []string{}})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := combinedTestPlan(t, tc.in)
			if got := combinedDeleteNames(p); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("deletes = %v, want %v", got, tc.want)
			}
			if p.credited < p.floor {
				t.Fatalf("credited %d below floor %d", p.credited, p.floor)
			}
		})
	}
	zero.groups[1] = combinedTestGroup(1, "new", true)
	if p := combinedTestPlan(t, zero); len(p.deletes) != 1 || p.floor != 1 {
		t.Fatalf("Ready addition should finance old replacement: %+v", p)
	}
	percent := combinedTestInput(4, 8, 1)
	percent.unavailable = intstr.FromString("25%")
	if p := combinedTestPlan(t, percent); p.floor != 2 || len(p.deletes) != 2 {
		t.Fatalf("percentages must resolve against desired replicas: %+v", p)
	}
	percent = combinedTestInput(1, 2, 1)
	percent.unavailable = intstr.FromString("49%")
	percent.surge = intstr.FromString("1%")
	if p := combinedTestPlan(t, percent); p.floor != 1 || len(p.deletes) != 0 {
		t.Fatalf("floor U and ceil S rounding changed: %+v", p)
	}
}

func TestCombinedPlannerReservationsAndRestart(t *testing.T) {
	in := combinedTestInput(4, 8, 1)
	first := combinedTestPlan(t, in)
	in.state, in.partition = first.state, first.partition
	// The same old leader is still Ready in the observation; its already-paid
	// deletion must be retried, not used to finance deleting ordinal 2 as well.
	second := combinedTestPlan(t, in)
	if !reflect.DeepEqual(combinedDeleteNames(first), combinedDeleteNames(second)) || first.state != second.state {
		t.Fatalf("restarted/repeated planning spent the reservation twice: %+v", second)
	}
	in.groups[3] = combinedTestGroup(3, "new", false)
	if p := combinedTestPlan(t, in); len(p.deletes) != 0 {
		t.Fatalf("replacement Pending must retain the payment: %+v", p)
	}
	in.groups[3] = combinedTestGroup(3, "new", true)
	if p := combinedTestPlan(t, in); !reflect.DeepEqual(combinedDeleteNames(p), []string{"example-2"}) {
		t.Fatalf("whole replacement Ready must release the reservation: %+v", p)
	}
}

func TestCombinedPlannerNeutralRepairDoesNotSerializePaidReplacement(t *testing.T) {
	in := combinedTestInput(3, 4, 2)
	in.groups[2] = combinedTestGroup(2, "old", false)
	p := combinedTestPlan(t, in)
	if !reflect.DeepEqual(combinedDeleteNames(p), []string{"example-2", "example-1"}) {
		t.Fatalf("unavailable high group must not block a paid lower replacement: %+v", p)
	}
	in = combinedTestInput(3, 4, 1)
	in.groups[2] = combinedTestGroup(2, "old", false)
	p = combinedTestPlan(t, in)
	if !reflect.DeepEqual(combinedDeleteNames(p), []string{"example-2"}) {
		t.Fatalf("initial unavailability consumes the paid budget: %+v", p)
	}
	// All initially unhealthy groups can be repaired without pretending that the
	// initial replica baseline has shrunk to zero.
	for i := 0; i < 3; i++ {
		in.groups[i] = combinedTestGroup(i, "old", false)
	}
	p = combinedTestPlan(t, in)
	if len(p.deletes) != 3 || p.floor != 2 {
		t.Fatalf("neutral repair should work while already below the floor: %+v", p)
	}
}

func TestCombinedPlannerUnavailableTogglePendingMiddle(t *testing.T) {
	in := combinedTestInput(2, 3, 1)
	p := combinedTestPlan(t, in)
	in.state, in.partition = p.state, p.partition
	in.groups[1] = combinedTestGroup(1, "new", false)
	in.groups[2] = combinedTestGroup(2, "new", true)
	in.generation++ // replicas/budget generation is independent of template revision
	in.unavailable, in.surge = intstr.FromInt(0), intstr.FromInt(1)
	p = combinedTestPlan(t, in)
	if len(p.deletes) != 0 || p.floor != 2 {
		t.Fatalf("U=0 should preserve two Ready groups: %+v", p)
	}
	in.state, in.partition = p.state, p.partition
	in.generation++
	in.unavailable = intstr.FromInt(1)
	p = combinedTestPlan(t, in)
	if p.partition != 0 || !reflect.DeepEqual(combinedDeleteNames(p), []string{"example-0"}) {
		t.Fatalf("U=1 must select lower old ordinal despite updated Pending ordinal 1: %+v", p)
	}
	// A model of the gate-disabled native descending loop stops at ordinal 1.
	// This is the actual distinction from merely lowering the partition.
	if target := combinedNativeGateOffTarget(in.groups, p.partition, "new"); target != -1 {
		t.Fatalf("native loop should be blocked, selected %d", target)
	}
}

// Only the destructive-update loop is modeled here. Native creation, cache
// propagation, status updates, and per-key workqueue serialization are NOT.
func combinedNativeGateOffTarget(groups []combinedModelGroup, partition int32, revision string) int {
	for i := len(groups) - 1; i >= int(partition); i-- {
		pod := groups[i].leader
		if pod == nil {
			return -1
		}
		if pod.Labels[appsv1.ControllerRevisionHashLabelKey] != "native-"+revision && pod.DeletionTimestamp == nil {
			return i
		}
		// Native StatefulSet sees leader readiness, not the LWS worker group.
		if pod.DeletionTimestamp != nil || !podutils.PodRunningAndReady(*pod) {
			return -1
		}
	}
	return -1
}

func TestCombinedPlannerNeutralRepairBehindReadyOldSuffix(t *testing.T) {
	in := combinedTestInput(3, 4, 1)
	in.groups[1] = combinedTestGroup(1, "old", false)
	// New leader 3 is Ready, but its workers are missing. It supplies no LWS
	// credit and does not block the native leader-only update loop.
	in.groups[3] = combinedTestGroup(3, "new", true)
	in.groups[3].leader.Annotations[leaderworkerset.SizeAnnotationKey] = "2"
	p := combinedTestPlan(t, in)
	if p.floor != 2 || p.credited != 2 || len(p.deletes) != 0 || p.partition != 3 {
		t.Fatalf("must preserve two Ready groups, not expose old ordinal 2: %+v", p)
	}
	// To recreate unavailable old ordinal 1 with the update template requires
	// partition <=1, exposing Ready old ordinal 2. A DaemonSet can select just
	// ordinal 1; StatefulSet's partition cannot represent that selection.
	if target := combinedNativeGateOffTarget(in.groups, 1, "new"); target != 2 {
		t.Fatalf("native loop should delete unreserved Ready ordinal 2; got %d", target)
	}
	if p.credited-1 >= p.floor {
		t.Fatal("counterexample must violate the availability floor")
	}
	// Deleting 1 while keeping partition 3 repairs at CURRENT revision. This
	// can fix transient failures, but not a failure fixed only by the update.
	if revision := combinedNativeCreationRevision(appsv1.RollingUpdateStatefulSetStrategyType, p.partition, 1); revision != "old" {
		t.Fatalf("partition-protected creation should use old template, got %q", revision)
	}
	t.Log("neutral repair to new template blocked behind a Ready stale higher ordinal at the floor")
}

// Characterization of newVersionedStatefulSetPod for explicit partitions and
// start ordinal zero in Kubernetes v1.35.0; does not run the native helper.
// With OnDelete, API validation requires rollingUpdate to be absent, so native
// creation chooses the update template; an LWS-only partition is invisible.
func combinedNativeCreationRevision(strategy appsv1.StatefulSetUpdateStrategyType, partition, ordinal int32) string {
	if strategy == appsv1.RollingUpdateStatefulSetStrategyType && ordinal < partition {
		return "old"
	}
	return "new"
}

func TestCombinedPlannerOnDeleteAlternativeLosesProtectedRecreation(t *testing.T) {
	const protected = int32(2)
	for ordinal := int32(0); ordinal < 4; ordinal++ {
		rolling := combinedNativeCreationRevision(appsv1.RollingUpdateStatefulSetStrategyType, protected, ordinal)
		onDelete := combinedNativeCreationRevision(appsv1.OnDeleteStatefulSetStrategyType, protected, ordinal)
		if ordinal < protected && (rolling != "old" || onDelete != "new") {
			t.Fatalf("protected ordinal %d: RollingUpdate=%s OnDelete=%s", ordinal, rolling, onDelete)
		}
		if ordinal >= protected && (rolling != "new" || onDelete != "new") {
			t.Fatalf("eligible ordinal %d should use updated template", ordinal)
		}
	}
}

func TestCombinedPlannerSupersessionAndDownscale(t *testing.T) {
	in := combinedTestInput(4, 8, 1)
	p := combinedTestPlan(t, in)
	in.state, in.partition = p.state, p.partition
	in.baseline = 8 // a newly observed target is NOT a new baseline
	in.generation++
	in.revision = "newer"
	in.nativeRevision = "native-newer"
	p = combinedTestPlan(t, in)
	if !p.freeze || p.partition != 8 || len(p.deletes) != 0 {
		t.Fatalf("supersession must freeze before changing the template: %+v", p)
	}
	s, err := combinedModelDecode(p.state)
	if err != nil || s.Baseline != 4 || s.Revision != "new" || len(s.Reservations) != 1 {
		t.Fatalf("freeze lost baseline/revision/obligation: %+v, %v", s, err)
	}
	in.templateFrozen = true
	p = combinedTestPlan(t, in)
	s, err = combinedModelDecode(p.state)
	if err != nil || s.Baseline != 4 || s.Generation != in.generation || s.Revision != "newer" {
		t.Fatalf("supersession changed the baseline or lost the active generation: %+v, %v", s, err)
	}
	in.state, in.partition = p.state, p.partition
	in.desired, in.generation = 2, in.generation+1
	p = combinedTestPlan(t, in)
	if p.replicas != 2 || p.floor != 1 {
		t.Fatalf("explicit downscale must cap the baseline, excluding condemned credit: %+v", p)
	}
	in.state, in.partition = p.state, p.partition
	in.desired, in.generation = 0, in.generation+1
	p = combinedTestPlan(t, in)
	if !p.done || p.replicas != 0 || p.floor != 0 || len(p.deletes) != 0 {
		t.Fatalf("scale to zero should leave scale-down to StatefulSet: %+v", p)
	}
	fromZero := combinedTestInput(0, 3, 1)
	if p := combinedTestPlan(t, fromZero); p.replicas != 3 || len(p.deletes) != 0 {
		t.Fatalf("from zero has no initial old groups to delete: %+v", p)
	}
}

func TestCombinedPlannerPartitionAndMalformedState(t *testing.T) {
	in := combinedTestInput(3, 4, 3)
	in.protected = 2
	p := combinedTestPlan(t, in)
	if p.partition < 2 || !reflect.DeepEqual(combinedDeleteNames(p), []string{"example-2"}) {
		t.Fatalf("user partition was violated: %+v", p)
	}
	for _, raw := range []string{"{", "null", `{}`, `{"v":2,"b":3,"d":4,"g":2,"r":"new"}`, `{"v":1,"b":-1,"d":4,"g":2,"r":"new"}`, `{"v":1,"b":3,"d":4,"g":2,"r":"new","pending":{"2":{"uid":"","revision":"new"}}}`} {
		in.state = raw
		if p, err := combinedModelPlanUpdate(in); err == nil || len(p.deletes) != 0 {
			t.Fatalf("malformed state %q was not rejected", raw)
		}
	}
	in = combinedTestInput(3, 4, 1)
	in.state = combinedModelEncode(combinedModelState{Version: 1, Baseline: 3, Desired: 4, Generation: 2, Revision: "new", NativeRevision: "native-new"})
	in.partition = 1 // previous actor exposed two old leaders without reserving
	if _, err := combinedModelPlanUpdate(in); err == nil {
		t.Fatal("must not silently repair an unreserved native suffix")
	}
}

func TestCombinedPlannerSnapshotInvariantExhaustive(t *testing.T) {
	// Exercise every readiness mask for a 4 -> 8 transition with budgets 1..4.
	// This checks decision accounting, not a real controller's concurrent writes.
	for u := 1; u <= 4; u++ {
		for mask := 0; mask < 256; mask++ {
			in := combinedTestInput(4, 8, u)
			ready := int32(0)
			for i := range in.groups {
				rev := in.groups[i].leader.Labels[leaderworkerset.RevisionKey]
				in.groups[i] = combinedTestGroup(i, rev, mask&(1<<i) != 0)
				if in.groups[i].ready(in.stsUID) {
					ready++
				}
			}
			p := combinedTestPlan(t, in)
			if p.credited < min(ready, p.floor) {
				t.Fatalf("u=%d mask=%08b: paid delete below attainable floor: %+v", u, mask, p)
			}
			state, err := combinedModelDecode(p.state)
			if err != nil {
				t.Fatal(err)
			}
			for i := p.partition; i < 4; i++ {
				if _, ok := state.Reservations[i]; !ok {
					t.Fatalf("u=%d mask=%08b: native suffix ordinal %d not reserved", u, mask, i)
				}
			}
		}
	}
}

func TestCombinedPlannerWholeGroupReadiness(t *testing.T) {
	makeGroup := func() combinedModelGroup {
		g := combinedTestGroup(0, "old", true)
		g.leader.Annotations[leaderworkerset.SizeAnnotationKey] = "2"
		g.workers = &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: g.leader.Name, UID: "workers", Generation: 2,
				Labels:          map[string]string{leaderworkerset.RevisionKey: "old"},
				OwnerReferences: []metav1.OwnerReference{{Kind: "Pod", UID: g.leader.UID, Controller: ptr.To(true)}}},
			Spec:   appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
			Status: appsv1.StatefulSetStatus{ObservedGeneration: 2, AvailableReplicas: 1, CurrentRevision: "worker-rev", UpdateRevision: "worker-rev"},
		}
		worker := combinedTestGroup(0, "old", true).leader
		worker.Name = "example-0-1"
		worker.Labels[leaderworkerset.WorkerIndexLabelKey] = "1"
		worker.Labels[appsv1.ControllerRevisionHashLabelKey] = "worker-rev"
		worker.OwnerReferences[0].UID = g.workers.UID
		g.pods = []corev1.Pod{*worker}
		return g
	}
	if !makeGroup().ready("leader-sts") {
		t.Fatal("valid old-revision group must supply Ready credit")
	}
	tests := map[string]func(*combinedModelGroup){
		"leader terminating":                         func(g *combinedModelGroup) { now := metav1.Now(); g.leader.DeletionTimestamp = &now },
		"foreign leader owner":                       func(g *combinedModelGroup) { g.leader.OwnerReferences[0].UID = "foreign" },
		"workers terminating":                        func(g *combinedModelGroup) { now := metav1.Now(); g.workers.DeletionTimestamp = &now },
		"previous leader UID":                        func(g *combinedModelGroup) { g.workers.OwnerReferences[0].UID = "previous" },
		"previous worker revision":                   func(g *combinedModelGroup) { g.workers.Labels[leaderworkerset.RevisionKey] = "previous" },
		"worker terminating despite Ready aggregate": func(g *combinedModelGroup) { now := metav1.Now(); g.pods[0].DeletionTimestamp = &now },
		"stale worker UID":                           func(g *combinedModelGroup) { g.pods[0].OwnerReferences[0].UID = "previous-workers" },
		"stale worker revision":                      func(g *combinedModelGroup) { g.pods[0].Labels[leaderworkerset.RevisionKey] = "previous" },
		"same LWS label stale worker native hash": func(g *combinedModelGroup) {
			g.pods[0].Labels[appsv1.ControllerRevisionHashLabelKey] = "previous-native"
		},
		"missing worker native target": func(g *combinedModelGroup) {
			g.workers.Status.CurrentRevision = ""
			g.workers.Status.UpdateRevision = ""
		},
		"worker missing":               func(g *combinedModelGroup) { g.pods = nil },
		"worker Pending":               func(g *combinedModelGroup) { g.pods[0].Status.Conditions = nil },
		"unobserved worker generation": func(g *combinedModelGroup) { g.workers.Status.ObservedGeneration = 1 },
		"invalid revision size":        func(g *combinedModelGroup) { g.leader.Annotations[leaderworkerset.SizeAnnotationKey] = "garbage" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			g := makeGroup()
			mutate(&g)
			if g.ready("leader-sts") {
				t.Fatal("invalid/incomplete group supplied Ready credit")
			}
		})
	}
	// A replacement leader alone does not release a reservation; old worker
	// ownership/status cannot make a new group look ready.
	in := combinedTestInput(2, 3, 1)
	p := combinedTestPlan(t, in)
	in.state, in.partition = p.state, p.partition
	in.groups[1] = makeGroup()
	in.groups[1].leader.Name, in.groups[1].leader.UID = "example-1", "new-1"
	in.groups[1].leader.Labels[leaderworkerset.RevisionKey] = "new"
	in.groups[1].leader.Labels[appsv1.ControllerRevisionHashLabelKey] = "native-new"
	if p := combinedTestPlan(t, in); len(p.deletes) != 0 {
		t.Fatalf("stale workers released the paid reservation: %+v", p)
	}
}

func TestCombinedPlannerSameLWSRevisionDifferentNative(t *testing.T) {
	in := combinedTestInput(2, 3, 0)
	in.surge = intstr.FromInt(1)
	for i := 0; i < 2; i++ {
		in.groups[i].leader.Labels[leaderworkerset.RevisionKey] = "new"
	}
	// A Ready added leader with missing workers cannot pay for either native
	// replacement, even though all leaders carry the desired LWS label.
	in.groups[2] = combinedTestGroup(2, "new", true)
	in.groups[2].leader.Annotations[leaderworkerset.SizeAnnotationKey] = "2"
	p := combinedTestPlan(t, in)
	if p.partition != 2 || len(p.deletes) != 0 || p.credited != p.floor {
		t.Fatalf("same LWS label exposed unpaid native replacements: %+v", p)
	}
	in.groups[2].leader.Annotations[leaderworkerset.SizeAnnotationKey] = "1"
	in.state, in.partition = p.state, p.partition
	p = combinedTestPlan(t, in)
	if !reflect.DeepEqual(combinedDeleteNames(p), []string{"example-1"}) {
		t.Fatalf("native-stale leader must be reserved and selected: %+v", p)
	}
	in.state, in.partition = p.state, p.partition
	in.groups[1].leader.UID = "replacement-but-native-stale"
	p = combinedTestPlan(t, in)
	s, err := combinedModelDecode(p.state)
	if err != nil || len(s.Reservations) != 1 || len(p.deletes) != 1 {
		t.Fatalf("wrong native replacement discharged obligation: %+v, %v", p, err)
	}
}

func TestCombinedPlannerDownscaleBeforeReservedDeletion(t *testing.T) {
	in := combinedTestInput(4, 8, 1)
	p := combinedTestPlan(t, in)
	in.state, in.partition, in.desired = p.state, p.partition, 2
	in.generation++
	// Production active input, NOT the synthetic supersession bypass.
	in.templateFrozen = false
	p = combinedTestPlan(t, in)
	s, err := combinedModelDecode(p.state)
	if err != nil || p.replicas != 2 || p.floor != 1 {
		t.Fatalf("downscale trapped by old reservation: %+v, %v", p, err)
	}
	if _, ok := s.Reservations[3]; ok {
		t.Fatal("condemned reservation survived atomic downscale")
	}
	in.state, in.partition, in.replicas = p.state, p.partition, p.replicas
	again := combinedTestPlan(t, in)
	if again.state != p.state {
		t.Fatal("downscale retry spent a second reservation")
	}
}

func TestCombinedPlannerSameUIDRecoveryDoesNotRefund(t *testing.T) {
	in := combinedTestInput(2, 3, 1)
	p := combinedTestPlan(t, in)
	in.state, in.partition = p.state, p.partition
	in.groups[1].leader.Labels[leaderworkerset.RevisionKey] = in.revision
	in.groups[1].leader.Labels[appsv1.ControllerRevisionHashLabelKey] = in.nativeRevision
	p = combinedTestPlan(t, in)
	s, err := combinedModelDecode(p.state)
	if err != nil || len(s.Reservations) != 1 || len(p.deletes) != 0 {
		t.Fatalf("same UID refunded without acknowledged withdrawal: %+v, %v", p, err)
	}
}
