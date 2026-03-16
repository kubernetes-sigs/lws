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
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

const testNamespace = "default"

// Test phase names used in tests
const (
	testPhasePrefill = "prefill"
	testPhaseDecode  = "decode"
)

// testPhaseNames returns the standard test phase names in order
func testPhaseNames() []string {
	return []string{testPhasePrefill, testPhaseDecode}
}

// testSchemeForUnit creates a scheme with all required types registered.
func testSchemeForUnit() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = disaggv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = leaderworkerset.AddToScheme(scheme)
	return scheme
}

func newTestReconciler(fakeClient client.Client) *DisaggregatedSetReconciler {
	scheme := testSchemeForUnit()
	return &DisaggregatedSetReconciler{
		Client:          fakeClient,
		Scheme:          scheme,
		WorkloadManager: NewLeaderWorkerSetManager(fakeClient),
		ServiceManager:  NewServiceManager(fakeClient, scheme),
		Recorder:        record.NewFakeRecorder(100),
	}
}

// newTestExecutor creates a RollingUpdateExecutor with a FakeRecorder for testing.
func newTestExecutor(fakeClient client.Client) *RollingUpdateExecutor {
	return &RollingUpdateExecutor{
		Client:          fakeClient,
		WorkloadManager: NewLeaderWorkerSetManager(fakeClient),
		Recorder:        record.NewFakeRecorder(100),
	}
}

// createTestLWS creates a LeaderWorkerSet for testing.
func createTestLWS(
	name, namespace, phase, revision string,
	replicas, readyReplicas int32,
	creationTime time.Time,
) *leaderworkerset.LeaderWorkerSet {
	return &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Time{Time: creationTime},
			Labels: map[string]string{
				LabelDisaggPhase: phase,
				LabelDisaggName:  "test",
				LabelRevision:    revision,
			},
		},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Replicas: ptr.To(replicas),
		},
		Status: leaderworkerset.LeaderWorkerSetStatus{
			Replicas:      replicas,
			ReadyReplicas: readyReplicas,
		},
	}
}

// createTestLWSWithAnnotations creates a LeaderWorkerSet with custom annotations.
func createTestLWSWithAnnotations(
	name, namespace, phase, revision string,
	replicas, readyReplicas int32,
	creationTime time.Time,
	annotations map[string]string,
) *leaderworkerset.LeaderWorkerSet {
	lws := createTestLWS(name, namespace, phase, revision, replicas, readyReplicas, creationTime)
	lws.Annotations = annotations
	return lws
}

// getTestLWSReplicas is a helper to get the current replica count from a LWS.
func getTestLWSReplicas(fakeClient client.Client, namespace, name string) int32 {
	var leaderWorkerSet leaderworkerset.LeaderWorkerSet
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := fakeClient.Get(context.TODO(), key, &leaderWorkerSet); err != nil {
		return -1 // Not found
	}
	if leaderWorkerSet.Spec.Replicas == nil {
		return 0
	}
	return *leaderWorkerSet.Spec.Replicas
}

// =============================================================================
// Workload Test Helpers
// =============================================================================

// createWorkloadForTest creates a LeaderWorkerSet for integration tests.
func createWorkloadForTest(
	name string,
	labels map[string]string,
	specReplicas, readyReplicas int32,
	podSpec corev1.PodSpec,
	ownerRef metav1.OwnerReference,
) client.Object {
	return &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "default",
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Replicas: ptr.To(specReplicas),
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				WorkerTemplate: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       podSpec,
				},
			},
		},
		Status: leaderworkerset.LeaderWorkerSetStatus{
			Replicas:      specReplicas,
			ReadyReplicas: readyReplicas,
		},
	}
}

// statusSubresourceObjects returns the objects that need status subresource support in the fake client.
func statusSubresourceObjects() []client.Object {
	return []client.Object{&disaggv1alpha1.DisaggregatedSet{}, &leaderworkerset.LeaderWorkerSet{}}
}

// getWorkloadReplicas fetches a workload by name and returns its spec replica count.
// Returns exists=false if the workload doesn't exist.
func getWorkloadReplicas(
	fakeClient client.Client, name string,
) (specReplicas int32, exists bool, err error) {
	var leaderWorkerSet leaderworkerset.LeaderWorkerSet
	key := types.NamespacedName{Namespace: testNamespace, Name: name}
	if err := fakeClient.Get(context.TODO(), key, &leaderWorkerSet); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return 0, false, nil
		}
		return 0, false, err
	}
	spec := int32(0)
	if leaderWorkerSet.Spec.Replicas != nil {
		spec = *leaderWorkerSet.Spec.Replicas
	}
	return spec, true, nil
}

// simulateAllReady sets ReadyReplicas = Spec.Replicas for all LWS in the given namespace.
func simulateAllReady(fakeClient client.Client) {
	var list leaderworkerset.LeaderWorkerSetList
	_ = fakeClient.List(context.TODO(), &list, client.InNamespace("default"))
	for i := range list.Items {
		leaderWorkerSet := &list.Items[i]
		if leaderWorkerSet.Spec.Replicas != nil {
			leaderWorkerSet.Status.Replicas = *leaderWorkerSet.Spec.Replicas
			leaderWorkerSet.Status.ReadyReplicas = *leaderWorkerSet.Spec.Replicas
			_ = fakeClient.Status().Update(context.TODO(), leaderWorkerSet)
		}
	}
}

// abcScenarioRevisions holds computed revisions for A→B→C rollout tests.
type abcScenarioRevisions struct{ A, B, C string }

// makePhaseSpec creates a DisaggregatedPhaseSpec with the given parameters
func makePhaseSpec(
	name string,
	replicas int32,
	podSpec corev1.PodSpec,
	surge, unavail intstr.IntOrString,
) disaggv1alpha1.DisaggregatedPhaseSpec {
	return disaggv1alpha1.DisaggregatedPhaseSpec{
		Name:     name,
		Replicas: ptr.To(replicas),
		LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
			Size:           ptr.To(int32(1)),
			WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
		},
		RolloutStrategy: &disaggv1alpha1.RolloutStrategy{
			MaxSurge: &surge, MaxUnavailable: &unavail,
		},
	}
}

// setupABCScenario creates a multi-workload test scenario with workloads A, B, and C.
// Returns client, deployment, and computed revisions.
func setupABCScenario(
	targetPrefill, targetDecode int32,
	aPrefill, aDecode, bPrefill, bDecode int32,
	prefillSurge, prefillUnavail, decodeSurge, decodeUnavail int,
) (client.Client, *disaggv1alpha1.DisaggregatedSet, abcScenarioRevisions) {
	podSpecA := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:a"}}}
	podSpecB := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:b"}}}
	podSpecC := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:c"}}}

	pSurge, pUnavail := intstr.FromInt(prefillSurge), intstr.FromInt(prefillUnavail)
	dSurge, dUnavail := intstr.FromInt(decodeSurge), intstr.FromInt(decodeUnavail)

	phasesA := []disaggv1alpha1.DisaggregatedPhaseSpec{
		makePhaseSpec(testPhasePrefill, targetPrefill, podSpecA, pSurge, pUnavail),
		makePhaseSpec(testPhaseDecode, targetDecode, podSpecA, dSurge, dUnavail),
	}
	phasesB := []disaggv1alpha1.DisaggregatedPhaseSpec{
		makePhaseSpec(testPhasePrefill, targetPrefill, podSpecB, pSurge, pUnavail),
		makePhaseSpec(testPhaseDecode, targetDecode, podSpecB, dSurge, dUnavail),
	}
	phasesC := []disaggv1alpha1.DisaggregatedPhaseSpec{
		makePhaseSpec(testPhasePrefill, targetPrefill, podSpecC, pSurge, pUnavail),
		makePhaseSpec(testPhaseDecode, targetDecode, podSpecC, dSurge, dUnavail),
	}

	revisionA := ComputeRevision(phasesA)
	revisionB := ComputeRevision(phasesB)
	revisionC := ComputeRevision(phasesC)

	deployment := &disaggv1alpha1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid"},
		Spec:       disaggv1alpha1.DisaggregatedSetSpec{Phases: phasesC},
	}

	ownerRef := metav1.OwnerReference{
		APIVersion: disaggv1alpha1.GroupVersion.String(),
		Kind:       "DisaggregatedSet",
		Name:       "test",
		UID:        "uid",
	}
	makeLabels := func(phase, revision string) map[string]string {
		return map[string]string{LabelDisaggPhase: phase, LabelDisaggName: "test", LabelRevision: revision}
	}

	var objects []client.Object
	objects = append(objects, deployment)
	if aPrefill > 0 || aDecode > 0 {
		nameA := fmt.Sprintf("test-%s", revisionA)
		objects = append(objects,
			createWorkloadForTest(
				nameA+"-prefill", makeLabels(testPhasePrefill, revisionA),
				aPrefill, aPrefill, podSpecA, ownerRef),
			createWorkloadForTest(
				nameA+"-decode", makeLabels(testPhaseDecode, revisionA),
				aDecode, aDecode, podSpecA, ownerRef))
	}
	if bPrefill > 0 || bDecode > 0 {
		nameB := fmt.Sprintf("test-%s", revisionB)
		objects = append(objects,
			createWorkloadForTest(
				nameB+"-prefill", makeLabels(testPhasePrefill, revisionB),
				bPrefill, bPrefill, podSpecB, ownerRef),
			createWorkloadForTest(
				nameB+"-decode", makeLabels(testPhaseDecode, revisionB),
				bDecode, bDecode, podSpecB, ownerRef))
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testSchemeForUnit()).
		WithObjects(objects...).
		WithStatusSubresource(statusSubresourceObjects()...).
		Build()
	return fakeClient, deployment, abcScenarioRevisions{A: revisionA, B: revisionB, C: revisionC}
}

// runReconcileUntilStable runs reconcile cycles until stable (max iterations).
func runReconcileUntilStable(
	t *testing.T,
	fakeClient client.Client,
	deployment *disaggv1alpha1.DisaggregatedSet,
	maxIterations int,
) {
	reconciler := newTestReconciler(fakeClient)
	for i := range maxIterations {
		_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace},
		})
		require.NoError(t, err, "Reconcile iteration %d should succeed", i)
		simulateAllReady(fakeClient)
	}
}

// assertWorkloadDrained checks that a workload is drained (0 or deleted).
func assertWorkloadDrained(t *testing.T, fakeClient client.Client, revision, phase string) {
	replicas := getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-%s-%s", revision, phase))
	assert.True(t, replicas == 0 || replicas == -1, "%s %s should be drained, got %d", revision, phase, replicas)
}

// =============================================================================
// Full Reconciler Integration Tests
// =============================================================================

// reconcilerTestCase defines a reconciler integration test scenario.
type reconcilerTestCase struct {
	name                     string
	deployName               string
	targetReplicas           int32
	oldSpec, oldReady        int32 // -1 means don't create old workload
	newSpec, newReady        int32 // -1 means don't create new workload
	maxSurge, maxUnavailable *int
	expectRequeue            bool
	expectOldDeleted         bool
	expectOldScaledDown      bool
	expectNewCreated         bool
}

func TestReconcilerIntegration(t *testing.T) {
	testCases := []reconcilerTestCase{
		{
			name: "completes and cleans up old workloads", deployName: "test-complete",
			targetReplicas: 2, oldSpec: 0, oldReady: 0, newSpec: 2, newReady: 2,
			expectRequeue: false, expectOldDeleted: true,
		},
		{
			name: "advances through rolling update", deployName: "test-advance",
			targetReplicas: 2, oldSpec: 2, oldReady: 2, newSpec: -1, newReady: -1,
			expectRequeue: true, expectNewCreated: true,
		},
		{
			name: "no scale down until new ready", deployName: "test-maxunavail",
			targetReplicas: 4, oldSpec: 4, oldReady: 4, newSpec: 2, newReady: 0,
			maxSurge: ptr.To(2), maxUnavailable: ptr.To(0),
			expectOldScaledDown: false,
		},
		{
			name: "scales down when new ready", deployName: "test-partial",
			targetReplicas: 4, oldSpec: 4, oldReady: 4, newSpec: 4, newReady: 4,
			maxSurge: ptr.To(2), maxUnavailable: ptr.To(0),
			expectOldScaledDown: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(testSchemeForUnit()).
				WithStatusSubresource(statusSubresourceObjects()...).
				Build()

			podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}}
			var rolloutStrategy *disaggv1alpha1.RolloutStrategy
			if tc.maxSurge != nil {
				surge, unavail := intstr.FromInt(*tc.maxSurge), intstr.FromInt(*tc.maxUnavailable)
				rolloutStrategy = &disaggv1alpha1.RolloutStrategy{
					MaxSurge: &surge, MaxUnavailable: &unavail,
				}
			}

			phases := []disaggv1alpha1.DisaggregatedPhaseSpec{
				{
					Name:     testPhasePrefill,
					Replicas: ptr.To(tc.targetReplicas),
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size:           ptr.To(int32(2)),
						WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
					},
					RolloutStrategy: rolloutStrategy,
				},
				{
					Name:     testPhaseDecode,
					Replicas: ptr.To(tc.targetReplicas),
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size:           ptr.To(int32(2)),
						WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
					},
					RolloutStrategy: rolloutStrategy,
				},
			}

			deployment := &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: tc.deployName, Namespace: "default", UID: "uid"},
				Spec:       disaggv1alpha1.DisaggregatedSetSpec{Phases: phases},
			}
			require.NoError(t, fakeClient.Create(context.TODO(), deployment))

			newRevision := ComputeRevision(phases)
			oldRevision := "oldhash"
			ownerRef := metav1.OwnerReference{
				APIVersion: disaggv1alpha1.GroupVersion.String(),
				Kind:       "DisaggregatedSet", Name: tc.deployName, UID: "uid",
			}
			makeLabels := func(phase, revision string) map[string]string {
				return map[string]string{
					LabelDisaggPhase: phase, LabelDisaggName: tc.deployName,
					LabelRevision: revision,
				}
			}

			// Create old workloads if specified
			if tc.oldSpec >= 0 {
				for _, phase := range testPhaseNames() {
					name := fmt.Sprintf("%s-%s-%s", tc.deployName, oldRevision, phase)
					obj := createWorkloadForTest(
						name, makeLabels(phase, oldRevision),
						tc.oldSpec, tc.oldReady, podSpec, ownerRef)
					require.NoError(t, fakeClient.Create(context.TODO(), obj))
				}
			}

			// Create new workloads if specified
			if tc.newSpec >= 0 {
				for _, phase := range testPhaseNames() {
					name := fmt.Sprintf("%s-%s-%s", tc.deployName, newRevision, phase)
					obj := createWorkloadForTest(
						name, makeLabels(phase, newRevision),
						tc.newSpec, tc.newReady, podSpec, ownerRef)
					require.NoError(t, fakeClient.Create(context.TODO(), obj))
				}
			}

			// Reconcile
			reconciler := newTestReconciler(fakeClient)
			result, err := reconciler.Reconcile(context.TODO(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: tc.deployName, Namespace: "default"},
			})
			require.NoError(t, err)

			// Assertions
			if tc.expectRequeue {
				assert.NotZero(t, result.RequeueAfter)
			}
			if tc.expectOldDeleted {
				for _, phase := range testPhaseNames() {
					name := fmt.Sprintf("%s-%s-%s", tc.deployName, oldRevision, phase)
					_, exists, _ := getWorkloadReplicas(fakeClient, name)
					assert.False(t, exists, "old %s should be deleted", phase)
				}
			}
			if tc.expectOldScaledDown {
				for _, phase := range testPhaseNames() {
					name := fmt.Sprintf("%s-%s-%s", tc.deployName, oldRevision, phase)
					replicas, _, _ := getWorkloadReplicas(fakeClient, name)
					assert.Less(t, replicas, tc.oldSpec, "old %s should scale down", phase)
				}
			}
			if tc.expectNewCreated {
				for _, phase := range testPhaseNames() {
					name := fmt.Sprintf("%s-%s-%s", tc.deployName, newRevision, phase)
					_, exists, _ := getWorkloadReplicas(fakeClient, name)
					assert.True(t, exists, "new %s should be created", phase)
				}
			}
		})
	}
}

// =============================================================================
// Unit Tests for sortByOldestTimestamp
// =============================================================================

func TestSortByOldestTimestamp(t *testing.T) {
	baseTime := time.Now()
	phaseNames := testPhaseNames()

	// Helper to create workload with offset from baseTime
	makeWorkload := func(hash string, offsetMinutes int) GroupedWorkload {
		ts := baseTime.Add(time.Duration(offsetMinutes) * time.Minute)
		return GroupedWorkload{
			Revision: hash,
			Phases: map[string]WorkloadInfo{
				testPhasePrefill: {CreationTimestamp: ts},
				testPhaseDecode:  {CreationTimestamp: ts},
			},
		}
	}

	testCases := []struct {
		name          string
		inputHashes   []string // hash names with implicit order by offset (0, 60, 120, ...)
		inputOffsets  []int    // offset in minutes from baseTime
		expectedOrder []string
	}{
		{"empty list", nil, nil, nil},
		{"single workload", []string{"hash1"}, []int{0}, []string{"hash1"}},
		{"three workloads unsorted", []string{"newest", "oldest", "middle"}, []int{120, 0, 60}, []string{"oldest", "middle", "newest"}},
		{"four workloads unsorted", []string{"d", "a", "c", "b"}, []int{30, 0, 20, 10}, []string{"a", "b", "c", "d"}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var workloads GroupedWorkloads
			for i, hash := range tc.inputHashes {
				workloads = append(workloads, makeWorkload(hash, tc.inputOffsets[i]))
			}
			result := sortByOldestTimestamp(workloads, phaseNames)
			require.Len(t, result, len(tc.expectedOrder))
			for i, expected := range tc.expectedOrder {
				assert.Equal(t, expected, result[i].Revision)
			}
		})
	}

	t.Run("does not modify original slice", func(t *testing.T) {
		workloads := GroupedWorkloads{makeWorkload("second", 60), makeWorkload("first", 0)}
		_ = sortByOldestTimestamp(workloads, phaseNames)
		assert.Equal(t, "second", workloads[0].Revision, "original slice should not be modified")
	})
}

// =============================================================================
// Unit Tests for isWorkloadStable
// =============================================================================

func TestIsWorkloadStable(t *testing.T) {
	phaseNames := testPhaseNames()

	testCases := []struct {
		name                                                       string
		prefillReplicas, prefillReady, decodeReplicas, decodeReady int
		expected                                                   bool
	}{
		{"all phases stable", 3, 3, 2, 2, true},
		{"prefill unstable", 3, 2, 2, 2, false},
		{"decode unstable", 3, 3, 2, 1, false},
		{"both phases unstable", 3, 1, 2, 0, false},
		{"zero replicas stable", 0, 0, 0, 0, true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			workload := GroupedWorkload{
				Revision: "hash1",
				Phases: map[string]WorkloadInfo{
					testPhasePrefill: {Replicas: tc.prefillReplicas, ReadyReplicas: tc.prefillReady},
					testPhaseDecode:  {Replicas: tc.decodeReplicas, ReadyReplicas: tc.decodeReady},
				},
			}
			assert.Equal(t, tc.expected, isWorkloadStable(workload, phaseNames))
		})
	}
}

// =============================================================================
// Unit Tests for GetPhaseConfigs
// =============================================================================

func TestGetPhaseConfigs(t *testing.T) {
	phases := []disaggv1alpha1.DisaggregatedPhaseSpec{
		{Name: testPhasePrefill, Replicas: ptr.To(int32(3))},
		{Name: testPhaseDecode, Replicas: ptr.To(int32(5))},
	}
	deployment := &disaggv1alpha1.DisaggregatedSet{
		Spec: disaggv1alpha1.DisaggregatedSetSpec{Phases: phases},
	}

	configs := GetPhaseConfigs(deployment)

	assert.Equal(t, int32(3), *configs[testPhasePrefill].Replicas)
	assert.Equal(t, int32(5), *configs[testPhaseDecode].Replicas)
}

// =============================================================================
// Unit Tests for extractRollingUpdateConfig
// =============================================================================

func TestExtractRollingUpdateConfig(t *testing.T) {
	intPtr := func(v int) *intstr.IntOrString { i := intstr.FromInt(v); return &i }

	testCases := []struct {
		name                                                     string
		prefillSurge, prefillUnavail, decodeSurge, decodeUnavail *intstr.IntOrString
		expectedPrefillSurge, expectedPrefillUnavail             int
		expectedDecodeSurge, expectedDecodeUnavail               int
	}{
		{"defaults when nil", nil, nil, nil, nil, 1, 0, 1, 0},
		{"custom prefill only", intPtr(3), intPtr(1), nil, nil, 3, 1, 1, 0},
		{"custom decode only", nil, nil, intPtr(2), intPtr(0), 1, 0, 2, 0},
		{"partial prefill (surge only)", intPtr(5), nil, nil, nil, 5, 0, 1, 0},
		{"both custom", intPtr(2), intPtr(1), intPtr(3), intPtr(2), 2, 1, 3, 2},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var prefillRollout, decodeRollout *disaggv1alpha1.RolloutStrategy
			if tc.prefillSurge != nil || tc.prefillUnavail != nil {
				prefillRollout = &disaggv1alpha1.RolloutStrategy{}
				if tc.prefillSurge != nil {
					prefillRollout.MaxSurge = tc.prefillSurge
				}
				if tc.prefillUnavail != nil {
					prefillRollout.MaxUnavailable = tc.prefillUnavail
				}
			}
			if tc.decodeSurge != nil || tc.decodeUnavail != nil {
				decodeRollout = &disaggv1alpha1.RolloutStrategy{}
				if tc.decodeSurge != nil {
					decodeRollout.MaxSurge = tc.decodeSurge
				}
				if tc.decodeUnavail != nil {
					decodeRollout.MaxUnavailable = tc.decodeUnavail
				}
			}

			phases := []disaggv1alpha1.DisaggregatedPhaseSpec{
				{Name: testPhasePrefill, Replicas: ptr.To(int32(3)), RolloutStrategy: prefillRollout},
				{Name: testPhaseDecode, Replicas: ptr.To(int32(2)), RolloutStrategy: decodeRollout},
			}

			ds := &disaggv1alpha1.DisaggregatedSet{
				Spec: disaggv1alpha1.DisaggregatedSetSpec{Phases: phases},
			}

			phaseNames := []string{testPhasePrefill, testPhaseDecode}
			specPhaseSet := map[string]bool{testPhasePrefill: true, testPhaseDecode: true}
			config := extractRollingUpdateConfig(ds, phaseNames, specPhaseSet)

			assert.Equal(t, tc.expectedPrefillSurge, config.MaxSurge[0])
			assert.Equal(t, tc.expectedPrefillUnavail, config.MaxUnavailable[0])
			assert.Equal(t, tc.expectedDecodeSurge, config.MaxSurge[1])
			assert.Equal(t, tc.expectedDecodeUnavail, config.MaxUnavailable[1])
		})
	}
}

// =============================================================================
// Unit Tests for scaleDownOld
// =============================================================================

// workloadDef defines a workload for scaleDownOld test cases.
type workloadDef struct {
	revision                        string
	prefill, decode                 int32
	ageHours                        int // hours after baseTime
	expectedPrefill, expectedDecode int32
}

func TestScaleDownOld(t *testing.T) {
	baseTime := time.Now()
	phaseNames := testPhaseNames()

	testCases := []struct {
		name                        string
		workloads                   []workloadDef
		prefillBudget, decodeBudget int
	}{
		{
			name:          "single workload scales down",
			workloads:     []workloadDef{{revision: "hash1", prefill: 4, decode: 4, ageHours: 0, expectedPrefill: 2, expectedDecode: 2}},
			prefillBudget: 2, decodeBudget: 2,
		},
		{
			name: "multiple workloads drain oldest first",
			workloads: []workloadDef{
				{revision: "oldest", prefill: 2, decode: 2, ageHours: 0, expectedPrefill: 0, expectedDecode: 0},
				{revision: "newer", prefill: 2, decode: 2, ageHours: 1, expectedPrefill: 2, expectedDecode: 2},
			},
			prefillBudget: 2, decodeBudget: 2,
		},
		{
			name:          "coordinated drain when one phase reaches zero",
			workloads:     []workloadDef{{revision: "hash1", prefill: 3, decode: 2, ageHours: 0, expectedPrefill: 0, expectedDecode: 0}},
			prefillBudget: 1, decodeBudget: 2,
		},
		{
			name:          "budget exhaustion stops mid-workload",
			workloads:     []workloadDef{{revision: "hash1", prefill: 6, decode: 6, ageHours: 0, expectedPrefill: 4, expectedDecode: 4}},
			prefillBudget: 2, decodeBudget: 2,
		},
		{
			name: "three workloads drain oldest then middle",
			workloads: []workloadDef{
				{revision: "oldest", prefill: 2, decode: 2, ageHours: 0, expectedPrefill: 0, expectedDecode: 0},
				{revision: "middle", prefill: 2, decode: 2, ageHours: 1, expectedPrefill: 0, expectedDecode: 0},
				{revision: "newest", prefill: 2, decode: 2, ageHours: 2, expectedPrefill: 2, expectedDecode: 2},
			},
			prefillBudget: 4, decodeBudget: 4,
		},
		{
			name: "coordinated drain with budget recycling",
			workloads: []workloadDef{
				{revision: "hashA", prefill: 1, decode: 2, ageHours: 0, expectedPrefill: 0, expectedDecode: 0},
				{revision: "hashB", prefill: 3, decode: 3, ageHours: 1, expectedPrefill: 3, expectedDecode: 2},
			},
			prefillBudget: 1, decodeBudget: 1,
		},
		{
			name: "drain oldest then spill to newer",
			workloads: []workloadDef{
				{revision: "oldest", prefill: 1, decode: 1, ageHours: 0, expectedPrefill: 0, expectedDecode: 0},
				{revision: "newer", prefill: 3, decode: 3, ageHours: 1, expectedPrefill: 2, expectedDecode: 2},
			},
			prefillBudget: 2, decodeBudget: 2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objects []client.Object
			var grouped GroupedWorkloads
			for _, workload := range tc.workloads {
				creationTime := baseTime.Add(time.Duration(workload.ageHours) * time.Hour)
				baseName := fmt.Sprintf("test-%s", workload.revision)
				objects = append(objects,
					createTestLWS(
						baseName+"-prefill", testNamespace, testPhasePrefill, workload.revision,
						workload.prefill, workload.prefill, creationTime),
					createTestLWS(
						baseName+"-decode", testNamespace, testPhaseDecode, workload.revision,
						workload.decode, workload.decode, creationTime))
				grouped = append(grouped, GroupedWorkload{
					Revision: workload.revision,
					Phases: map[string]WorkloadInfo{
						testPhasePrefill: {Replicas: int(workload.prefill), CreationTimestamp: creationTime},
						testPhaseDecode:  {Replicas: int(workload.decode), CreationTimestamp: creationTime},
					},
				})
			}

			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(objects...).WithStatusSubresource(&leaderworkerset.LeaderWorkerSet{}).Build()
			executor := newTestExecutor(fakeClient)
			ds := &disaggv1alpha1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace}}

			// Convert budget to current/target format
			current := PhaseReplicaState{
				grouped.GetTotalReplicasPerPhase(testPhasePrefill),
				grouped.GetTotalReplicasPerPhase(testPhaseDecode),
			}
			target := PhaseReplicaState{
				current[0] - tc.prefillBudget,
				current[1] - tc.decodeBudget,
			}
			err := executor.scaleDownOld(context.TODO(), ds, grouped, phaseNames, current, target)
			require.NoError(t, err)

			for _, workload := range tc.workloads {
				prefillName := fmt.Sprintf("test-%s-prefill", workload.revision)
				decodeName := fmt.Sprintf("test-%s-decode", workload.revision)
				assert.Equal(t, workload.expectedPrefill,
					getTestLWSReplicas(fakeClient, testNamespace, prefillName))
				assert.Equal(t, workload.expectedDecode,
					getTestLWSReplicas(fakeClient, testNamespace, decodeName))
			}
		})
	}
}

// TestScaleDownOldWithMissingPhase tests that phases not present in
// old workloads don't trigger false coordinated drain. This was a bug where
// adding a new phase would cause all old workloads to be brutally drained to 0
// because the new phase (with 0 replicas in old workload) would trigger
// coordinated drain logic.
func TestScaleDownOldWithMissingPhase(t *testing.T) {
	baseTime := time.Now()
	// 3 phases: prefill, decode, encode - but old workload only has prefill and decode
	threePhaseNames := []string{"prefill", "decode", "encode"}

	testCases := []struct {
		name            string
		prefillBudget   int
		decodeBudget    int
		encodeBudget    int
		expectedPrefill int32
		expectedDecode  int32
	}{
		{
			name:            "missing phase should not trigger coordinated drain",
			prefillBudget:   0, // planner says don't scale down prefill
			decodeBudget:    1, // planner says scale down 1 decode
			encodeBudget:    0, // encode doesn't exist in old workload, budget is 0
			expectedPrefill: 4, // should stay at 4 (no coordinated drain!)
			expectedDecode:  3, // should scale down from 4 to 3
		},
		{
			name:            "normal drain with missing phase",
			prefillBudget:   1,
			decodeBudget:    1,
			encodeBudget:    0,
			expectedPrefill: 3,
			expectedDecode:  3,
		},
		{
			name:            "coordinated drain only when existing phase reaches zero",
			prefillBudget:   4, // drain all prefill
			decodeBudget:    0,
			encodeBudget:    0,
			expectedPrefill: 0,
			expectedDecode:  0, // should be 0 due to coordinated drain (prefill reached 0)
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create old workload with only prefill and decode phases (no encode)
			objects := []client.Object{
				createTestLWS("test-oldhash-prefill", testNamespace, "prefill", "oldhash",
					4, 4, baseTime),
				createTestLWS("test-oldhash-decode", testNamespace, "decode", "oldhash",
					4, 4, baseTime),
				// Note: NO encode LWS exists in old workload
			}

			// GroupedWorkload only has prefill and decode
			grouped := GroupedWorkloads{
				{
					Revision: "oldhash",
					Phases: map[string]WorkloadInfo{
						"prefill": {Replicas: 4, CreationTimestamp: baseTime},
						"decode":  {Replicas: 4, CreationTimestamp: baseTime},
						// Note: encode is NOT in this map
					},
				},
			}

			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(objects...).WithStatusSubresource(&leaderworkerset.LeaderWorkerSet{}).Build()
			executor := newTestExecutor(fakeClient)
			ds := &disaggv1alpha1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace}}

			// Convert budget to current/target format for 3 phases
			current := PhaseReplicaState{
				grouped.GetTotalReplicasPerPhase("prefill"),
				grouped.GetTotalReplicasPerPhase("decode"),
				grouped.GetTotalReplicasPerPhase("encode"), // 0 since it doesn't exist
			}
			target := PhaseReplicaState{
				current[0] - tc.prefillBudget,
				current[1] - tc.decodeBudget,
				current[2] - tc.encodeBudget,
			}
			err := executor.scaleDownOld(context.TODO(), ds, grouped, threePhaseNames, current, target)
			require.NoError(t, err)

			// Verify prefill and decode were scaled correctly
			assert.Equal(t, tc.expectedPrefill,
				getTestLWSReplicas(fakeClient, testNamespace, "test-oldhash-prefill"),
				"prefill replicas mismatch")
			assert.Equal(t, tc.expectedDecode,
				getTestLWSReplicas(fakeClient, testNamespace, "test-oldhash-decode"),
				"decode replicas mismatch")
		})
	}
}

// =============================================================================
// Unit Tests for scaleUpNew
// =============================================================================

func TestScaleUpNew(t *testing.T) {
	baseTime := time.Now()
	namespace := testNamespace
	phaseNames := testPhaseNames()
	specPhaseSet := map[string]bool{testPhasePrefill: true, testPhaseDecode: true}

	testCases := []struct {
		name                            string
		initPrefill, initDecode         int32
		workloadPrefill, workloadDecode int
		targetPrefill, targetDecode     int
		expectedPrefill, expectedDecode int32
	}{
		{"scales up prefill only", 2, 4, 2, 4, 4, 4, 4, 4},
		{"scales up decode only", 4, 2, 4, 2, 4, 4, 4, 4},
		{"scales up both phases", 1, 2, 1, 2, 4, 4, 4, 4},
		{"no-op when at target", 4, 4, 4, 4, 4, 4, 4, 4},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(
					createTestLWS("test-newhash-prefill", namespace, testPhasePrefill, "newhash", tc.initPrefill, tc.initPrefill, baseTime),
					createTestLWS("test-newhash-decode", namespace, testPhaseDecode, "newhash", tc.initDecode, tc.initDecode, baseTime),
				).
				WithStatusSubresource(&leaderworkerset.LeaderWorkerSet{}).
				Build()

			executor := newTestExecutor(fakeClient)

			ds := &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: namespace},
			}

			newWorkload := GroupedWorkload{
				Revision: "newhash",
				Phases: map[string]WorkloadInfo{
					testPhasePrefill: {Replicas: tc.workloadPrefill},
					testPhaseDecode:  {Replicas: tc.workloadDecode},
				},
			}

			current := PhaseReplicaState{tc.workloadPrefill, tc.workloadDecode}
			target := PhaseReplicaState{tc.targetPrefill, tc.targetDecode}
			err := executor.scaleUpNew(context.TODO(), ds, newWorkload, phaseNames, specPhaseSet, current, target)
			require.NoError(t, err)

			assert.Equal(t, tc.expectedPrefill, getTestLWSReplicas(fakeClient, namespace, "test-newhash-prefill"))
			assert.Equal(t, tc.expectedDecode, getTestLWSReplicas(fakeClient, namespace, "test-newhash-decode"))
		})
	}
}

// =============================================================================
// Unit Tests for ensureNewWorkloadExists
// =============================================================================

func TestEnsureNewWorkloadExists(t *testing.T) {
	testCases := []struct {
		name             string
		existingReplicas int32 // -1 means no existing workload
		expectedCreated  bool
		expectedReplicas int32
	}{
		{"creates workload when missing", -1, true, 2},
		{"no-op when workload already exists", 3, false, 3},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objects []client.Object
			if tc.existingReplicas >= 0 {
				objects = append(objects, createTestLWS(
					"test-newhash-prefill", testNamespace, testPhasePrefill, "newhash",
					tc.existingReplicas, tc.existingReplicas, time.Now()))
			}
			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(objects...).
				WithStatusSubresource(&leaderworkerset.LeaderWorkerSet{}).
				Build()

			executor := newTestExecutor(fakeClient)
			podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}}
			phases := []disaggv1alpha1.DisaggregatedPhaseSpec{
				{
					Name:     testPhasePrefill,
					Replicas: ptr.To(int32(4)),
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size:           ptr.To(int32(1)),
						WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
					},
				},
				{
					Name:     testPhaseDecode,
					Replicas: ptr.To(int32(4)),
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size:           ptr.To(int32(1)),
						WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
					},
				},
			}
			deployment := &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "test-uid"},
				Spec:       disaggv1alpha1.DisaggregatedSetSpec{Phases: phases},
			}

			created, err := executor.ensureNewWorkloadExists(
				context.TODO(), deployment, "newhash", testPhasePrefill,
				&phases[0], 2)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedCreated, created)
			assert.Equal(t, tc.expectedReplicas, getTestLWSReplicas(fakeClient, testNamespace, "test-newhash-prefill"))
		})
	}
}

// =============================================================================
// Integration Tests for Multiple Old Workloads (A→B→C rollout scenario)
// =============================================================================

// abcExecutorScenario defines inputs for ReconcileRollingUpdate executor tests.
type abcExecutorScenario struct {
	name                            string
	aPrefill, aDecode               int32
	bPrefill, bDecode               int32
	cPrefill, cDecode               int32
	expectedA, expectedB, expectedC [2]int32 // [prefill, decode]
}

func TestReconcileRollingUpdateABCScenario(t *testing.T) {
	baseTime := time.Now()

	testCases := []abcExecutorScenario{
		{
			name: "drains oldest first", aPrefill: 2, aDecode: 2, bPrefill: 2, bDecode: 2, cPrefill: 0, cDecode: 0,
			expectedA: [2]int32{2, 2}, expectedB: [2]int32{2, 2}, expectedC: [2]int32{1, 1},
		},
		{
			name: "C scales up while B stays", aPrefill: 0, aDecode: 0, bPrefill: 2, bDecode: 2, cPrefill: 2, cDecode: 2,
			expectedA: [2]int32{0, 0}, expectedB: [2]int32{2, 2}, expectedC: [2]int32{3, 3},
		},
	}

	initAnnot := map[string]string{AnnotationInitialReplicas: "2"}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objects []client.Object
			if tc.aPrefill > 0 || tc.aDecode > 0 {
				objects = append(objects,
					createTestLWSWithAnnotations(
						"test-hashA-prefill", testNamespace, testPhasePrefill, "hashA",
						tc.aPrefill, tc.aPrefill, baseTime, initAnnot),
					createTestLWSWithAnnotations(
						"test-hashA-decode", testNamespace, testPhaseDecode, "hashA",
						tc.aDecode, tc.aDecode, baseTime, initAnnot))
			}
			bTime := baseTime.Add(1 * time.Hour)
			cTime := baseTime.Add(2 * time.Hour)
			objects = append(objects,
				createTestLWSWithAnnotations(
					"test-hashB-prefill", testNamespace, testPhasePrefill, "hashB",
					tc.bPrefill, tc.bPrefill, bTime, initAnnot),
				createTestLWSWithAnnotations(
					"test-hashB-decode", testNamespace, testPhaseDecode, "hashB",
					tc.bDecode, tc.bDecode, bTime, initAnnot),
				createTestLWS(
					"test-hashC-prefill", testNamespace, testPhasePrefill, "hashC",
					tc.cPrefill, tc.cPrefill, cTime),
				createTestLWS(
					"test-hashC-decode", testNamespace, testPhaseDecode, "hashC",
					tc.cDecode, tc.cDecode, cTime))

			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(objects...).
				WithStatusSubresource(&leaderworkerset.LeaderWorkerSet{}).
				Build()
			executor := newTestExecutor(fakeClient)
			phases := []disaggv1alpha1.DisaggregatedPhaseSpec{
				{Name: testPhasePrefill, Replicas: ptr.To(int32(4))},
				{Name: testPhaseDecode, Replicas: ptr.To(int32(4))},
			}
			deployment := &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace},
				Spec:       disaggv1alpha1.DisaggregatedSetSpec{Phases: phases},
			}

			oldWorkloads := GroupedWorkloads{
				{Revision: "hashA", Phases: map[string]WorkloadInfo{
					testPhasePrefill: {
						Replicas: int(tc.aPrefill), ReadyReplicas: int(tc.aPrefill),
						InitialReplicas: 2, CreationTimestamp: baseTime},
					testPhaseDecode: {
						Replicas: int(tc.aDecode), ReadyReplicas: int(tc.aDecode),
						InitialReplicas: 2, CreationTimestamp: baseTime}}},
				{Revision: "hashB", Phases: map[string]WorkloadInfo{
					testPhasePrefill: {
						Replicas: int(tc.bPrefill), ReadyReplicas: int(tc.bPrefill),
						InitialReplicas: 2, CreationTimestamp: bTime},
					testPhaseDecode: {
						Replicas: int(tc.bDecode), ReadyReplicas: int(tc.bDecode),
						InitialReplicas: 2, CreationTimestamp: bTime}}},
			}
			newWorkload := GroupedWorkload{Revision: "hashC", Phases: map[string]WorkloadInfo{
				testPhasePrefill: {Replicas: int(tc.cPrefill), ReadyReplicas: int(tc.cPrefill)},
				testPhaseDecode:  {Replicas: int(tc.cDecode), ReadyReplicas: int(tc.cDecode)}}}

			_, err := executor.ReconcileRollingUpdate(context.TODO(), deployment, oldWorkloads, newWorkload)
			require.NoError(t, err)

			if tc.aPrefill > 0 || tc.aDecode > 0 {
				assert.Equal(t, tc.expectedA[0], getTestLWSReplicas(fakeClient, testNamespace, "test-hashA-prefill"))
				assert.Equal(t, tc.expectedA[1], getTestLWSReplicas(fakeClient, testNamespace, "test-hashA-decode"))
			}
			assert.Equal(t, tc.expectedB[0], getTestLWSReplicas(fakeClient, testNamespace, "test-hashB-prefill"))
			assert.Equal(t, tc.expectedB[1], getTestLWSReplicas(fakeClient, testNamespace, "test-hashB-decode"))
			assert.Equal(t, tc.expectedC[0], getTestLWSReplicas(fakeClient, testNamespace, "test-hashC-prefill"))
			assert.Equal(t, tc.expectedC[1], getTestLWSReplicas(fakeClient, testNamespace, "test-hashC-decode"))
		})
	}
}

// =============================================================================
// Multi-Step Scenario Tests
// =============================================================================

// TestMidRolloutABC tests the A→B→C rolling update scenario where C is triggered
// while A→B is still in progress. Scenario: A(1,2) + B(1,2) = (2,4) total, target (2,4).
func TestMidRolloutABC(t *testing.T) {
	// Setup: A(1,2), B(1,2), target (2,4), maxSurge=1, maxUnavailable=0
	fakeClient, deployment, revisions := setupABCScenario(2, 4, 1, 2, 1, 2, 1, 0, 1, 0)

	runReconcileUntilStable(t, fakeClient, deployment, 20)

	// Verify: A and B drained, C at target (2,4)
	assertWorkloadDrained(t, fakeClient, revisions.A, testPhasePrefill)
	assertWorkloadDrained(t, fakeClient, revisions.A, testPhaseDecode)
	assertWorkloadDrained(t, fakeClient, revisions.B, testPhasePrefill)
	assertWorkloadDrained(t, fakeClient, revisions.B, testPhaseDecode)
	assert.Equal(t, int32(2), getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-%s-prefill", revisions.C)))
	assert.Equal(t, int32(4), getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-%s-decode", revisions.C)))
}

// TestAsymmetricSizesCoordinatedDrain tests coordinated draining with asymmetric workloads.
// Scenario: A(1,2), B(3,1), target (4,3). Coordinated draining ensures no orphans.
func TestAsymmetricSizesCoordinatedDrain(t *testing.T) {
	// Setup: A(1,2), B(3,1), target (4,3), prefill maxSurge=1, decode maxSurge=2
	fakeClient, deployment, revisions := setupABCScenario(4, 3, 1, 2, 3, 1, 1, 0, 2, 0)

	reconciler := newTestReconciler(fakeClient)
	normalize := func(v int32) int32 {
		if v == -1 {
			return 0
		}
		return v
	}

	// Run reconcile cycles and check for orphans at each step
	for i := range 20 {
		_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace},
		})
		require.NoError(t, err, "Reconcile iteration %d", i)
		simulateAllReady(fakeClient)

		// Check no orphans (if one phase is 0, the other must also be 0)
		aPrefill := normalize(getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-%s-prefill", revisions.A)))
		aDecode := normalize(getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-%s-decode", revisions.A)))
		bPrefill := normalize(getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-%s-prefill", revisions.B)))
		bDecode := normalize(getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-%s-decode", revisions.B)))

		assert.False(t, (aPrefill == 0) != (aDecode == 0), "Step %d: A orphaned - prefill=%d, decode=%d", i, aPrefill, aDecode)
		assert.False(t, (bPrefill == 0) != (bDecode == 0), "Step %d: B orphaned - prefill=%d, decode=%d", i, bPrefill, bDecode)
	}

	// Verify final state: A and B drained, C at target (4,3)
	assertWorkloadDrained(t, fakeClient, revisions.A, testPhasePrefill)
	assertWorkloadDrained(t, fakeClient, revisions.A, testPhaseDecode)
	assertWorkloadDrained(t, fakeClient, revisions.B, testPhasePrefill)
	assertWorkloadDrained(t, fakeClient, revisions.B, testPhaseDecode)
	assert.Equal(t, int32(4), getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-%s-prefill", revisions.C)))
	assert.Equal(t, int32(3), getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-%s-decode", revisions.C)))
}

// =============================================================================
// Unit Tests for PhasePolicy Enforcement
// =============================================================================

// setupPhasePolicyScenario creates a scenario for testing phasePolicy enforcement.
// Returns client, deployment, executor, and the new revision.
func setupPhasePolicyScenario(
	t *testing.T,
	oldPhaseNames []string,
	newPhaseNames []string,
	phasePolicy disaggv1alpha1.PhasePolicy,
) (client.Client, *disaggv1alpha1.DisaggregatedSet, *RollingUpdateExecutor, string) {
	podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:old"}}}
	podSpecNew := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:new"}}}

	// Create old phases
	var oldPhases []disaggv1alpha1.DisaggregatedPhaseSpec
	for _, name := range oldPhaseNames {
		oldPhases = append(oldPhases, disaggv1alpha1.DisaggregatedPhaseSpec{
			Name:     name,
			Replicas: ptr.To(int32(2)),
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				Size:           ptr.To(int32(1)),
				WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
			},
		})
	}

	// Create new phases (for the deployment spec)
	var newPhases []disaggv1alpha1.DisaggregatedPhaseSpec
	for _, name := range newPhaseNames {
		newPhases = append(newPhases, disaggv1alpha1.DisaggregatedPhaseSpec{
			Name:     name,
			Replicas: ptr.To(int32(2)),
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				Size:           ptr.To(int32(1)),
				WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpecNew},
			},
		})
	}

	oldRevision := ComputeRevision(oldPhases)
	newRevision := ComputeRevision(newPhases)

	deployment := &disaggv1alpha1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "uid"},
		Spec: disaggv1alpha1.DisaggregatedSetSpec{
			Phases:      newPhases,
			PhasePolicy: phasePolicy,
		},
	}

	ownerRef := metav1.OwnerReference{
		APIVersion: disaggv1alpha1.GroupVersion.String(),
		Kind:       "DisaggregatedSet",
		Name:       "test",
		UID:        "uid",
	}

	var objects []client.Object
	objects = append(objects, deployment)

	// Create old workloads
	baseTime := time.Now()
	for _, phaseName := range oldPhaseNames {
		labels := map[string]string{
			LabelDisaggPhase: phaseName,
			LabelDisaggName:  "test",
			LabelRevision:    oldRevision,
		}
		objects = append(objects, createWorkloadForTest(
			fmt.Sprintf("test-%s-%s", oldRevision, phaseName),
			labels, 2, 2, podSpec, ownerRef))
	}

	// Create new workloads (empty, to simulate rolling update in progress)
	for _, phaseName := range newPhaseNames {
		labels := map[string]string{
			LabelDisaggPhase: phaseName,
			LabelDisaggName:  "test",
			LabelRevision:    newRevision,
		}
		lws := createWorkloadForTest(
			fmt.Sprintf("test-%s-%s", newRevision, phaseName),
			labels, 0, 0, podSpecNew, ownerRef)
		lws.(*leaderworkerset.LeaderWorkerSet).ObjectMeta.CreationTimestamp = metav1.Time{Time: baseTime.Add(time.Hour)}
		objects = append(objects, lws)
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testSchemeForUnit()).
		WithObjects(objects...).
		WithStatusSubresource(statusSubresourceObjects()...).
		Build()

	executor := newTestExecutor(fakeClient)
	return fakeClient, deployment, executor, newRevision
}

func TestPhasePolicyStrictBlocksPhaseAddition(t *testing.T) {
	// Old: prefill, decode -> New: prefill, decode, encode (added encode)
	_, deployment, executor, revision := setupPhasePolicyScenario(
		t,
		[]string{"prefill", "decode"},
		[]string{"prefill", "decode", "encode"},
		disaggv1alpha1.PhasePolicyStrict,
	)

	_, err := executor.ReconcileRollingUpdateNew(context.TODO(), deployment, revision)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "phasePolicy is Strict but phases changed")
	assert.Contains(t, err.Error(), "added=[encode]")
}

func TestPhasePolicyStrictBlocksPhaseRemoval(t *testing.T) {
	// Old: prefill, decode, encode -> New: prefill, decode (removed encode)
	_, deployment, executor, revision := setupPhasePolicyScenario(
		t,
		[]string{"prefill", "decode", "encode"},
		[]string{"prefill", "decode"},
		disaggv1alpha1.PhasePolicyStrict,
	)

	_, err := executor.ReconcileRollingUpdateNew(context.TODO(), deployment, revision)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "phasePolicy is Strict but phases changed")
	assert.Contains(t, err.Error(), "removed=[encode]")
}

func TestPhasePolicyStrictBlocksPhaseRename(t *testing.T) {
	// Old: prefill, decode, encode -> New: prefill, decode, decode-long-context (renamed encode)
	_, deployment, executor, revision := setupPhasePolicyScenario(
		t,
		[]string{"prefill", "decode", "encode"},
		[]string{"prefill", "decode", "decode-long-context"},
		disaggv1alpha1.PhasePolicyStrict,
	)

	_, err := executor.ReconcileRollingUpdateNew(context.TODO(), deployment, revision)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "phasePolicy is Strict but phases changed")
	assert.Contains(t, err.Error(), "added=[decode-long-context]")
	assert.Contains(t, err.Error(), "removed=[encode]")
}

func TestPhasePolicyFlexibleAllowsPhaseRename(t *testing.T) {
	// Old: prefill, decode, encode -> New: prefill, decode, decode-long-context
	fakeClient, deployment, executor, revision := setupPhasePolicyScenario(
		t,
		[]string{"prefill", "decode", "encode"},
		[]string{"prefill", "decode", "decode-long-context"},
		disaggv1alpha1.PhasePolicyFlexible,
	)

	// Get old revision for checking drain
	podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:old"}}}
	oldPhases := []disaggv1alpha1.DisaggregatedPhaseSpec{
		{Name: "prefill", Replicas: ptr.To(int32(2)), LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
			Size: ptr.To(int32(1)), WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec}}},
		{Name: "decode", Replicas: ptr.To(int32(2)), LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
			Size: ptr.To(int32(1)), WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec}}},
		{Name: "encode", Replicas: ptr.To(int32(2)), LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
			Size: ptr.To(int32(1)), WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec}}},
	}
	oldRevision := ComputeRevision(oldPhases)

	// Run reconciles until encode is drained to 0 (progressive drain takes multiple reconciles)
	// We also simulate new workloads becoming ready after each reconcile for the Planner to progress.
	for i := 0; i < 20; i++ {
		_, err := executor.ReconcileRollingUpdateNew(context.TODO(), deployment, revision)
		require.NoError(t, err)

		// Simulate new workloads becoming ready by updating their status
		for _, phase := range []string{"prefill", "decode", "decode-long-context"} {
			lwsName := fmt.Sprintf("test-%s-%s", revision, phase)
			lws := &leaderworkerset.LeaderWorkerSet{}
			if err := fakeClient.Get(context.TODO(), client.ObjectKey{Namespace: testNamespace, Name: lwsName}, lws); err == nil {
				if lws.Spec.Replicas != nil {
					lws.Status.ReadyReplicas = *lws.Spec.Replicas
					lws.Status.Replicas = *lws.Spec.Replicas
					_ = fakeClient.Status().Update(context.TODO(), lws)
				}
			}
		}

		// Check if encode is drained
		encodeReplicas := getTestLWSReplicas(fakeClient, testNamespace, fmt.Sprintf("test-%s-encode", oldRevision))
		if encodeReplicas == 0 {
			break
		}
	}

	// Verify encode phase was drained (scaled to 0)
	encodeReplicas := getTestLWSReplicas(fakeClient, testNamespace, fmt.Sprintf("test-%s-encode", oldRevision))
	assert.Equal(t, int32(0), encodeReplicas, "encode phase should be drained to 0")
}

func TestPhasePolicyDefaultIsStrict(t *testing.T) {
	// When phasePolicy is not set (empty), it should default to Strict
	_, deployment, executor, revision := setupPhasePolicyScenario(
		t,
		[]string{"prefill", "decode"},
		[]string{"prefill", "decode", "encode"},
		"", // Empty = should default to Strict
	)

	_, err := executor.ReconcileRollingUpdateNew(context.TODO(), deployment, revision)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "phasePolicy is Strict but phases changed")
}

func TestPhasePolicyStrictAllowsSamePhases(t *testing.T) {
	// Old: prefill, decode -> New: prefill, decode (same phases, just image change)
	_, deployment, executor, revision := setupPhasePolicyScenario(
		t,
		[]string{"prefill", "decode"},
		[]string{"prefill", "decode"},
		disaggv1alpha1.PhasePolicyStrict,
	)

	// Should not error - phases are the same
	_, err := executor.ReconcileRollingUpdateNew(context.TODO(), deployment, revision)
	require.NoError(t, err)
}
