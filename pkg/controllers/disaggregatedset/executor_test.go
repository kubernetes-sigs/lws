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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/test/wrappers"
)

const testNamespace = "default"

// Test role names used in tests
const (
	testRolePrefill = "prefill"
	testRoleDecode  = "decode"
)

// testRoleNames returns the standard test role names in order
func testRoleNames() []string {
	return []string{testRolePrefill, testRoleDecode}
}

// testSchemeForUnit creates a scheme with all required types registered.
func testSchemeForUnit() *runtime.Scheme {
	return wrappers.DisaggregatedSetTestScheme()
}

func newTestReconciler(fakeClient client.Client) *DisaggregatedSetReconciler {
	scheme := testSchemeForUnit()
	return &DisaggregatedSetReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}
}

// newTestExecutor creates a RollingUpdateExecutor with a FakeRecorder for testing.
func newTestExecutor(fakeClient client.Client) *RollingUpdateExecutor {
	return &RollingUpdateExecutor{
		Client:     fakeClient,
		LWSManager: NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}
}

func buildTestLWS(name, namespace, role, revision string) *wrappers.LeaderWorkerSetWrapper {
	return wrappers.BuildBasicLeaderWorkerSet(name, namespace).
		Labels(map[string]string{
			disaggregatedsetv1.RoleLabelKey:     role,
			disaggregatedsetv1.SetNameLabelKey:  "test",
			disaggregatedsetv1.SliceLabelKey:    "0",
			disaggregatedsetv1.RevisionLabelKey: revision,
		})
}

// getTestLWSReplicas is a helper to get the current replica count from a LWS.
func getTestLWSReplicas(fakeClient client.Client, namespace, name string) int32 {
	var leaderWorkerSet leaderworkersetv1.LeaderWorkerSet
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := fakeClient.Get(context.TODO(), key, &leaderWorkerSet); err != nil {
		return -1 // Not found
	}
	if leaderWorkerSet.Spec.Replicas == nil {
		return 0
	}
	return *leaderWorkerSet.Spec.Replicas
}

// makeLWS creates a minimal LWS object for use in RevisionRoles test fixtures.
func makeLWS(opts ...func(*leaderworkersetv1.LeaderWorkerSet)) *leaderworkersetv1.LeaderWorkerSet {
	lws := &leaderworkersetv1.LeaderWorkerSet{}
	for _, opt := range opts {
		opt(lws)
	}
	return lws
}

func withName(name string) func(*leaderworkersetv1.LeaderWorkerSet) {
	return func(lws *leaderworkersetv1.LeaderWorkerSet) {
		lws.Name = name
	}
}

func withReplicas(r int) func(*leaderworkersetv1.LeaderWorkerSet) {
	return func(lws *leaderworkersetv1.LeaderWorkerSet) {
		r32 := int32(r)
		lws.Spec.Replicas = &r32
	}
}

func withReadyReplicas(r int) func(*leaderworkersetv1.LeaderWorkerSet) {
	return func(lws *leaderworkersetv1.LeaderWorkerSet) {
		lws.Status.ReadyReplicas = int32(r)
	}
}

func withCreationTimestamp(ts time.Time) func(*leaderworkersetv1.LeaderWorkerSet) {
	return func(lws *leaderworkersetv1.LeaderWorkerSet) {
		lws.CreationTimestamp = metav1.Time{Time: ts}
	}
}

//nolint:unparam // test helper, r will vary as more tests are added
func withInitialReplicasAnnotation(r int) func(*leaderworkersetv1.LeaderWorkerSet) {
	return func(lws *leaderworkersetv1.LeaderWorkerSet) {
		if lws.Annotations == nil {
			lws.Annotations = make(map[string]string)
		}
		lws.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey] = fmt.Sprintf("%d", r)
	}
}

// =============================================================================
// LWS Test Helpers
// =============================================================================

// createLWSForTest creates a LeaderWorkerSet for integration tests.
func createLWSForTest(
	name string,
	labels map[string]string,
	specReplicas, readyReplicas int32,
	podSpec corev1.PodSpec,
	ownerRef metav1.OwnerReference,
) client.Object {
	return wrappers.BuildBasicLeaderWorkerSet(name, "default").
		Labels(labels).
		Replica(int(specReplicas)).
		StatusReplicas(specReplicas).
		ReadyReplicas(readyReplicas).
		OwnerReference(ownerRef).
		WorkerTemplateSpec(podSpec).
		Obj()
}

// statusSubresourceObjects returns the objects that need status subresource support in the fake client.
func statusSubresourceObjects() []client.Object {
	return []client.Object{&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}}
}

// fetchLWSReplicas fetches an LWS by name and returns its spec replica count.
// Returns exists=false if the LWS doesn't exist.
func fetchLWSReplicas(
	fakeClient client.Client, name string,
) (specReplicas int32, exists bool, err error) {
	var leaderWorkerSet leaderworkersetv1.LeaderWorkerSet
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
	var list leaderworkersetv1.LeaderWorkerSetList
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

// makeRoleSpec creates a DisaggregatedRoleSpec with the given parameters
func makeRoleSpec(
	name string,
	replicas int32,
	podSpec corev1.PodSpec,
	surge, unavail intstr.IntOrString,
) disaggregatedsetv1.DisaggregatedRoleSpec {
	return wrappers.MakeRoleSpec(name, replicas, podSpec, surge, unavail)
}

// setupABCScenario creates a multi-workload test scenario with workloads A, B, and C.
// Returns client, deployment, and computed revisions.
func setupABCScenario(
	targetPrefill, targetDecode int32,
	aPrefill, aDecode, bPrefill, bDecode int32,
	prefillSurge, prefillUnavail, decodeSurge, decodeUnavail int,
) (client.Client, *disaggregatedsetv1.DisaggregatedSet, abcScenarioRevisions) {
	podSpecA := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:a"}}}
	podSpecB := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:b"}}}
	podSpecC := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:c"}}}

	pSurge, pUnavail := intstr.FromInt(prefillSurge), intstr.FromInt(prefillUnavail)
	dSurge, dUnavail := intstr.FromInt(decodeSurge), intstr.FromInt(decodeUnavail)

	rolesA := []disaggregatedsetv1.DisaggregatedRoleSpec{
		makeRoleSpec(testRolePrefill, targetPrefill, podSpecA, pSurge, pUnavail),
		makeRoleSpec(testRoleDecode, targetDecode, podSpecA, dSurge, dUnavail),
	}
	rolesB := []disaggregatedsetv1.DisaggregatedRoleSpec{
		makeRoleSpec(testRolePrefill, targetPrefill, podSpecB, pSurge, pUnavail),
		makeRoleSpec(testRoleDecode, targetDecode, podSpecB, dSurge, dUnavail),
	}
	rolesC := []disaggregatedsetv1.DisaggregatedRoleSpec{
		makeRoleSpec(testRolePrefill, targetPrefill, podSpecC, pSurge, pUnavail),
		makeRoleSpec(testRoleDecode, targetDecode, podSpecC, dSurge, dUnavail),
	}

	revisionA := disaggregatedsetutils.ComputeRevision(rolesA)
	revisionB := disaggregatedsetutils.ComputeRevision(rolesB)
	revisionC := disaggregatedsetutils.ComputeRevision(rolesC)

	deployment := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid"},
		Spec:       disaggregatedsetv1.DisaggregatedSetSpec{Roles: rolesC},
	}

	ownerRef := metav1.OwnerReference{
		APIVersion: disaggregatedsetv1.GroupVersion.String(),
		Kind:       "DisaggregatedSet",
		Name:       "test",
		UID:        "uid",
	}
	makeLabels := func(role, revision string) map[string]string {
		return map[string]string{disaggregatedsetv1.RoleLabelKey: role, disaggregatedsetv1.SetNameLabelKey: "test", disaggregatedsetv1.SliceLabelKey: "0", disaggregatedsetv1.RevisionLabelKey: revision}
	}

	var objects []client.Object
	objects = append(objects, deployment)
	if aPrefill > 0 || aDecode > 0 {
		nameA := fmt.Sprintf("test-0-%s", revisionA)
		objects = append(objects,
			createLWSForTest(
				nameA+"-prefill", makeLabels(testRolePrefill, revisionA),
				aPrefill, aPrefill, podSpecA, ownerRef),
			createLWSForTest(
				nameA+"-decode", makeLabels(testRoleDecode, revisionA),
				aDecode, aDecode, podSpecA, ownerRef))
	}
	if bPrefill > 0 || bDecode > 0 {
		nameB := fmt.Sprintf("test-0-%s", revisionB)
		objects = append(objects,
			createLWSForTest(
				nameB+"-prefill", makeLabels(testRolePrefill, revisionB),
				bPrefill, bPrefill, podSpecB, ownerRef),
			createLWSForTest(
				nameB+"-decode", makeLabels(testRoleDecode, revisionB),
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
	deployment *disaggregatedsetv1.DisaggregatedSet,
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

// assertLWSDrained checks that a workload is drained (0 or deleted).
func assertLWSDrained(t *testing.T, fakeClient client.Client, revision, role string) {
	replicas := getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-0-%s-%s", revision, role))
	assert.True(t, replicas == 0 || replicas == -1, "%s %s should be drained, got %d", revision, role, replicas)
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
			var rolloutConfig *leaderworkersetv1.RollingUpdateConfiguration
			if tc.maxSurge != nil {
				surge, unavail := intstr.FromInt(*tc.maxSurge), intstr.FromInt(*tc.maxUnavailable)
				rolloutConfig = &leaderworkersetv1.RollingUpdateConfiguration{
					MaxSurge: surge, MaxUnavailable: unavail,
				}
			}

			roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
				{
					Name: testRolePrefill,
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						Replicas: ptr.To(tc.targetReplicas),
						LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
							Size:           ptr.To(int32(2)),
							WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
						},
						RolloutStrategy: leaderworkersetv1.RolloutStrategy{
							RollingUpdateConfiguration: rolloutConfig,
						},
					}},
				},
				{
					Name: testRoleDecode,
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						Replicas: ptr.To(tc.targetReplicas),
						LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
							Size:           ptr.To(int32(2)),
							WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
						},
						RolloutStrategy: leaderworkersetv1.RolloutStrategy{
							RollingUpdateConfiguration: rolloutConfig,
						},
					}},
				},
			}

			deployment := &disaggregatedsetv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: tc.deployName, Namespace: "default", UID: "uid"},
				Spec:       disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
			}
			require.NoError(t, fakeClient.Create(context.TODO(), deployment))

			newRevision := disaggregatedsetutils.ComputeRevision(roles)
			oldRevision := "oldhash"
			ownerRef := metav1.OwnerReference{
				APIVersion: disaggregatedsetv1.GroupVersion.String(),
				Kind:       "DisaggregatedSet", Name: tc.deployName, UID: "uid",
			}
			makeLabels := func(role, revision string) map[string]string {
				return map[string]string{
					disaggregatedsetv1.RoleLabelKey: role, disaggregatedsetv1.SetNameLabelKey: tc.deployName,
					disaggregatedsetv1.SliceLabelKey:    "0",
					disaggregatedsetv1.RevisionLabelKey: revision,
				}
			}

			// Create old workloads if specified
			if tc.oldSpec >= 0 {
				for _, role := range testRoleNames() {
					name := fmt.Sprintf("%s-0-%s-%s", tc.deployName, oldRevision, role)
					obj := createLWSForTest(
						name, makeLabels(role, oldRevision),
						tc.oldSpec, tc.oldReady, podSpec, ownerRef)
					require.NoError(t, fakeClient.Create(context.TODO(), obj))
				}
			}

			// Create new workloads if specified
			if tc.newSpec >= 0 {
				for _, role := range testRoleNames() {
					name := fmt.Sprintf("%s-0-%s-%s", tc.deployName, newRevision, role)
					obj := createLWSForTest(
						name, makeLabels(role, newRevision),
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
				for _, role := range testRoleNames() {
					name := fmt.Sprintf("%s-0-%s-%s", tc.deployName, oldRevision, role)
					_, exists, _ := fetchLWSReplicas(fakeClient, name)
					assert.False(t, exists, "old %s should be deleted", role)
				}
			}
			if tc.expectOldScaledDown {
				for _, role := range testRoleNames() {
					name := fmt.Sprintf("%s-0-%s-%s", tc.deployName, oldRevision, role)
					replicas, _, _ := fetchLWSReplicas(fakeClient, name)
					assert.Less(t, replicas, tc.oldSpec, "old %s should scale down", role)
				}
			}
			if tc.expectNewCreated {
				for _, role := range testRoleNames() {
					name := fmt.Sprintf("%s-0-%s-%s", tc.deployName, newRevision, role)
					_, exists, _ := fetchLWSReplicas(fakeClient, name)
					assert.True(t, exists, "new %s should be created", role)
				}
			}
		})
	}
}

// =============================================================================
// Unit Tests for sortByNewestTimestamp
// =============================================================================

func TestSortByNewestTimestamp(t *testing.T) {
	baseTime := time.Now()
	roleNames := testRoleNames()

	// Helper to create workload with offset from baseTime
	makeRevision := func(hash string, offsetMinutes int) disaggregatedsetutils.RevisionRoles {
		ts := baseTime.Add(time.Duration(offsetMinutes) * time.Minute)
		return disaggregatedsetutils.RevisionRoles{
			Revision: hash,
			Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
				testRolePrefill: makeLWS(withCreationTimestamp(ts)),
				testRoleDecode:  makeLWS(withCreationTimestamp(ts)),
			},
		}
	}

	testCases := []struct {
		name          string
		inputHashes   []string
		inputOffsets  []int
		expectedOrder []string
	}{
		{"empty list", nil, nil, nil},
		{"single workload", []string{"hash1"}, []int{0}, []string{"hash1"}},
		{"three workloads unsorted", []string{"newest", "oldest", "middle"}, []int{120, 0, 60}, []string{"newest", "middle", "oldest"}},
		{"four workloads unsorted", []string{"d", "a", "c", "b"}, []int{30, 0, 20, 10}, []string{"d", "c", "b", "a"}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var workloads disaggregatedsetutils.RevisionRolesList
			for i, hash := range tc.inputHashes {
				workloads = append(workloads, makeRevision(hash, tc.inputOffsets[i]))
			}
			result := sortByNewestTimestamp(workloads, roleNames)
			require.Len(t, result, len(tc.expectedOrder))
			for i, expected := range tc.expectedOrder {
				assert.Equal(t, expected, result[i].Revision)
			}
		})
	}

	t.Run("does not modify original slice", func(t *testing.T) {
		workloads := disaggregatedsetutils.RevisionRolesList{makeRevision("second", 60), makeRevision("first", 0)}
		_ = sortByNewestTimestamp(workloads, roleNames)
		assert.Equal(t, "second", workloads[0].Revision, "original slice should not be modified")
	})

	t.Run("uses max timestamp across roles", func(t *testing.T) {
		ts1 := baseTime
		ts2 := baseTime.Add(10 * time.Minute)
		ts3 := baseTime.Add(20 * time.Minute)
		workloads := disaggregatedsetutils.RevisionRolesList{
			{Revision: "A", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
				testRolePrefill: makeLWS(withCreationTimestamp(ts1)),
				testRoleDecode:  makeLWS(withCreationTimestamp(ts3)),
			}},
			{Revision: "B", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
				testRolePrefill: makeLWS(withCreationTimestamp(ts2)),
				testRoleDecode:  makeLWS(withCreationTimestamp(ts2)),
			}},
		}
		result := sortByNewestTimestamp(workloads, roleNames)
		assert.Equal(t, "A", result[0].Revision, "A has max ts=20min, should come first (newest)")
		assert.Equal(t, "B", result[1].Revision, "B has max ts=10min, should come second")
	})
}

// =============================================================================
// Unit Tests for isRevisionStable
// =============================================================================

func TestIsRevisionStable(t *testing.T) {
	roleNames := testRoleNames()

	testCases := []struct {
		name                                                       string
		prefillReplicas, prefillReady, decodeReplicas, decodeReady int
		expected                                                   bool
	}{
		{"all roles stable", 3, 3, 2, 2, true},
		{"prefill unstable", 3, 2, 2, 2, false},
		{"decode unstable", 3, 3, 2, 1, false},
		{"both roles unstable", 3, 1, 2, 0, false},
		{"zero replicas stable", 0, 0, 0, 0, true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			workload := disaggregatedsetutils.RevisionRoles{
				Revision: "hash1",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testRolePrefill: makeLWS(withReplicas(tc.prefillReplicas), withReadyReplicas(tc.prefillReady)),
					testRoleDecode:  makeLWS(withReplicas(tc.decodeReplicas), withReadyReplicas(tc.decodeReady)),
				},
			}
			assert.Equal(t, tc.expected, isRevisionStable(workload, roleNames))
		})
	}
}

// =============================================================================
// Unit Tests for disaggregatedsetutils.GetRoleConfigs
// =============================================================================

func TestGetRoleConfigs(t *testing.T) {
	roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
		{Name: testRolePrefill, LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(int32(3))}}},
		{Name: testRoleDecode, LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(int32(5))}}},
	}
	deployment := &disaggregatedsetv1.DisaggregatedSet{
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
	}

	configs := disaggregatedsetutils.GetRoleConfigs(deployment)

	assert.Equal(t, int32(3), *configs[testRolePrefill].Spec.Replicas)
	assert.Equal(t, int32(5), *configs[testRoleDecode].Spec.Replicas)
}

// =============================================================================
// Unit Tests for extractRollingUpdateConfig
// =============================================================================

func TestExtractRollingUpdateConfig(t *testing.T) {
	intVal := func(v int) intstr.IntOrString { return intstr.FromInt(v) }

	testCases := []struct {
		name                                                     string
		prefillSurge, prefillUnavail, decodeSurge, decodeUnavail *int
		expectedPrefillSurge, expectedPrefillUnavail             int
		expectedDecodeSurge, expectedDecodeUnavail               int
	}{
		{"defaults when nil", nil, nil, nil, nil, 1, 0, 1, 0},
		{"custom prefill only", ptr.To(3), ptr.To(1), nil, nil, 3, 1, 1, 0},
		{"custom decode only", nil, nil, ptr.To(2), ptr.To(0), 1, 0, 2, 0},
		{"partial prefill (surge only)", ptr.To(5), ptr.To(0), nil, nil, 5, 0, 1, 0},
		{"both custom", ptr.To(2), ptr.To(1), ptr.To(3), ptr.To(2), 2, 1, 3, 2},
		{"surge=0 with unavail allows zero surge", ptr.To(0), ptr.To(4), ptr.To(0), ptr.To(2), 0, 4, 0, 2},
		{"surge=0 without unavail keeps default", ptr.To(0), ptr.To(0), ptr.To(0), ptr.To(0), 1, 0, 1, 0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var prefillRolloutConfig, decodeRolloutConfig *leaderworkersetv1.RollingUpdateConfiguration
			if tc.prefillSurge != nil || tc.prefillUnavail != nil {
				prefillRolloutConfig = &leaderworkersetv1.RollingUpdateConfiguration{}
				if tc.prefillSurge != nil {
					prefillRolloutConfig.MaxSurge = intVal(*tc.prefillSurge)
				}
				if tc.prefillUnavail != nil {
					prefillRolloutConfig.MaxUnavailable = intVal(*tc.prefillUnavail)
				}
			}
			if tc.decodeSurge != nil || tc.decodeUnavail != nil {
				decodeRolloutConfig = &leaderworkersetv1.RollingUpdateConfiguration{}
				if tc.decodeSurge != nil {
					decodeRolloutConfig.MaxSurge = intVal(*tc.decodeSurge)
				}
				if tc.decodeUnavail != nil {
					decodeRolloutConfig.MaxUnavailable = intVal(*tc.decodeUnavail)
				}
			}

			roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
				{
					Name: testRolePrefill,
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						Replicas: ptr.To(int32(3)),
						RolloutStrategy: leaderworkersetv1.RolloutStrategy{
							RollingUpdateConfiguration: prefillRolloutConfig,
						},
					}},
				},
				{
					Name: testRoleDecode,
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						Replicas: ptr.To(int32(2)),
						RolloutStrategy: leaderworkersetv1.RolloutStrategy{
							RollingUpdateConfiguration: decodeRolloutConfig,
						},
					}},
				},
			}

			ds := &disaggregatedsetv1.DisaggregatedSet{
				Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
			}

			roleNames := []string{testRolePrefill, testRoleDecode}
			config := extractRollingUpdateConfig(ds, roleNames)

			assert.Equal(t, tc.expectedPrefillSurge, config[0].MaxSurge)
			assert.Equal(t, tc.expectedPrefillUnavail, config[0].MaxUnavailable)
			assert.Equal(t, tc.expectedDecodeSurge, config[1].MaxSurge)
			assert.Equal(t, tc.expectedDecodeUnavail, config[1].MaxUnavailable)
		})
	}
}

func TestExtractRollingUpdateConfigWithPercentages(t *testing.T) {
	strVal := func(v string) intstr.IntOrString { return intstr.FromString(v) }

	testCases := []struct {
		name                                         string
		prefillReplicas, decodeReplicas              int32
		prefillSurge, prefillUnavail                 string
		decodeSurge, decodeUnavail                   string
		expectedPrefillSurge, expectedPrefillUnavail int
		expectedDecodeSurge, expectedDecodeUnavail   int
	}{
		{
			name:                   "50% surge on 4 replicas = 2",
			prefillReplicas:        4,
			decodeReplicas:         4,
			prefillSurge:           "50%",
			prefillUnavail:         "0",
			decodeSurge:            "50%",
			decodeUnavail:          "0",
			expectedPrefillSurge:   2,
			expectedPrefillUnavail: 0,
			expectedDecodeSurge:    2,
			expectedDecodeUnavail:  0,
		},
		{
			name:                   "25% unavailable on 4 replicas = 1",
			prefillReplicas:        4,
			decodeReplicas:         4,
			prefillSurge:           "0",
			prefillUnavail:         "25%",
			decodeSurge:            "0",
			decodeUnavail:          "25%",
			expectedPrefillSurge:   0,
			expectedPrefillUnavail: 1,
			expectedDecodeSurge:    0,
			expectedDecodeUnavail:  1,
		},
		{
			name:                   "surge rounds up, unavail rounds down",
			prefillReplicas:        10,
			decodeReplicas:         10,
			prefillSurge:           "25%", // 2.5 -> 3 (round up)
			prefillUnavail:         "25%", // 2.5 -> 2 (round down)
			decodeSurge:            "25%",
			decodeUnavail:          "25%",
			expectedPrefillSurge:   3,
			expectedPrefillUnavail: 2,
			expectedDecodeSurge:    3,
			expectedDecodeUnavail:  2,
		},
		{
			name:                   "100% surge",
			prefillReplicas:        5,
			decodeReplicas:         5,
			prefillSurge:           "100%",
			prefillUnavail:         "0",
			decodeSurge:            "100%",
			decodeUnavail:          "0",
			expectedPrefillSurge:   5,
			expectedPrefillUnavail: 0,
			expectedDecodeSurge:    5,
			expectedDecodeUnavail:  0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
				{
					Name: testRolePrefill,
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						Replicas: ptr.To(tc.prefillReplicas),
						RolloutStrategy: leaderworkersetv1.RolloutStrategy{
							RollingUpdateConfiguration: &leaderworkersetv1.RollingUpdateConfiguration{
								MaxSurge:       strVal(tc.prefillSurge),
								MaxUnavailable: strVal(tc.prefillUnavail),
							},
						},
					}},
				},
				{
					Name: testRoleDecode,
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						Replicas: ptr.To(tc.decodeReplicas),
						RolloutStrategy: leaderworkersetv1.RolloutStrategy{
							RollingUpdateConfiguration: &leaderworkersetv1.RollingUpdateConfiguration{
								MaxSurge:       strVal(tc.decodeSurge),
								MaxUnavailable: strVal(tc.decodeUnavail),
							},
						},
					}},
				},
			}

			ds := &disaggregatedsetv1.DisaggregatedSet{
				Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
			}

			roleNames := []string{testRolePrefill, testRoleDecode}
			config := extractRollingUpdateConfig(ds, roleNames)

			assert.Equal(t, tc.expectedPrefillSurge, config[0].MaxSurge)
			assert.Equal(t, tc.expectedPrefillUnavail, config[0].MaxUnavailable)
			assert.Equal(t, tc.expectedDecodeSurge, config[1].MaxSurge)
			assert.Equal(t, tc.expectedDecodeUnavail, config[1].MaxUnavailable)
		})
	}
}

// =============================================================================
// Unit Tests for scaleDownOld
// =============================================================================

// lwsDef defines a workload for scaleDownOld test cases.
type lwsDef struct {
	revision                        string
	prefill, decode                 int32
	ageHours                        int // hours after baseTime
	expectedPrefill, expectedDecode int32
}

func TestScaleDownOld(t *testing.T) {
	baseTime := time.Now()
	roleNames := testRoleNames()

	testCases := []struct {
		name                        string
		workloads                   []lwsDef
		prefillBudget, decodeBudget int
	}{
		{
			name:          "single workload scales down",
			workloads:     []lwsDef{{revision: "hash1", prefill: 4, decode: 4, ageHours: 0, expectedPrefill: 2, expectedDecode: 2}},
			prefillBudget: 2, decodeBudget: 2,
		},
		{
			name: "multiple workloads drain newest first",
			workloads: []lwsDef{
				{revision: "oldest", prefill: 2, decode: 2, ageHours: 0, expectedPrefill: 2, expectedDecode: 2},
				{revision: "newer", prefill: 2, decode: 2, ageHours: 1, expectedPrefill: 0, expectedDecode: 0},
			},
			prefillBudget: 2, decodeBudget: 2,
		},
		{
			name:          "coordinated drain when one role reaches zero",
			workloads:     []lwsDef{{revision: "hash1", prefill: 3, decode: 2, ageHours: 0, expectedPrefill: 0, expectedDecode: 0}},
			prefillBudget: 1, decodeBudget: 2,
		},
		{
			name:          "budget exhaustion stops mid-workload",
			workloads:     []lwsDef{{revision: "hash1", prefill: 6, decode: 6, ageHours: 0, expectedPrefill: 4, expectedDecode: 4}},
			prefillBudget: 2, decodeBudget: 2,
		},
		{
			name: "three workloads drain newest then middle",
			workloads: []lwsDef{
				{revision: "oldest", prefill: 2, decode: 2, ageHours: 0, expectedPrefill: 2, expectedDecode: 2},
				{revision: "middle", prefill: 2, decode: 2, ageHours: 1, expectedPrefill: 0, expectedDecode: 0},
				{revision: "newest", prefill: 2, decode: 2, ageHours: 2, expectedPrefill: 0, expectedDecode: 0},
			},
			prefillBudget: 4, decodeBudget: 4,
		},
		{
			name: "partial drain of newest without coordinated trigger",
			workloads: []lwsDef{
				{revision: "hashA", prefill: 1, decode: 2, ageHours: 0, expectedPrefill: 1, expectedDecode: 2},
				{revision: "hashB", prefill: 3, decode: 3, ageHours: 1, expectedPrefill: 2, expectedDecode: 2},
			},
			prefillBudget: 1, decodeBudget: 1,
		},
		{
			name: "drains newest without spilling to older",
			workloads: []lwsDef{
				{revision: "oldest", prefill: 1, decode: 1, ageHours: 0, expectedPrefill: 1, expectedDecode: 1},
				{revision: "newer", prefill: 3, decode: 3, ageHours: 1, expectedPrefill: 1, expectedDecode: 1},
			},
			prefillBudget: 2, decodeBudget: 2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objects []client.Object
			var grouped disaggregatedsetutils.RevisionRolesList
			for _, workload := range tc.workloads {
				creationTime := baseTime.Add(time.Duration(workload.ageHours) * time.Hour)
				baseName := fmt.Sprintf("test-0-%s", workload.revision)
				objects = append(objects,
					buildTestLWS(baseName+"-prefill", testNamespace, testRolePrefill, workload.revision).
						Replica(int(workload.prefill)).StatusReplicas(workload.prefill).ReadyReplicas(workload.prefill).CreationTimestamp(creationTime).Obj(),
					buildTestLWS(baseName+"-decode", testNamespace, testRoleDecode, workload.revision).
						Replica(int(workload.decode)).StatusReplicas(workload.decode).ReadyReplicas(workload.decode).CreationTimestamp(creationTime).Obj())
				grouped = append(grouped, disaggregatedsetutils.RevisionRoles{
					Revision: workload.revision,
					Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
						testRolePrefill: makeLWS(withName(baseName+"-prefill"), withReplicas(int(workload.prefill)), withCreationTimestamp(creationTime)),
						testRoleDecode:  makeLWS(withName(baseName+"-decode"), withReplicas(int(workload.decode)), withCreationTimestamp(creationTime)),
					},
				})
			}

			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(objects...).WithStatusSubresource(&leaderworkersetv1.LeaderWorkerSet{}).Build()
			executor := newTestExecutor(fakeClient)
			ds := &disaggregatedsetv1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace}}

			// Convert budget to current/target format
			current := RoleReplicaState{
				grouped.GetTotalReplicasPerRole(testRolePrefill),
				grouped.GetTotalReplicasPerRole(testRoleDecode),
			}
			target := RoleReplicaState{
				current[0] - tc.prefillBudget,
				current[1] - tc.decodeBudget,
			}
			err := executor.scaleDownOld(context.TODO(), ds, grouped, roleNames, current, target)
			require.NoError(t, err)

			for _, workload := range tc.workloads {
				prefillName := fmt.Sprintf("test-0-%s-prefill", workload.revision)
				decodeName := fmt.Sprintf("test-0-%s-decode", workload.revision)
				assert.Equal(t, workload.expectedPrefill,
					getTestLWSReplicas(fakeClient, testNamespace, prefillName))
				assert.Equal(t, workload.expectedDecode,
					getTestLWSReplicas(fakeClient, testNamespace, decodeName))
			}
		})
	}
}

// TestScaleDownOldWithMissingRole tests that roles not present in
// old workloads don't trigger false coordinated drain. This was a bug where
// adding a new role would cause all old workloads to be brutally drained to 0
// because the new role (with 0 replicas in old workload) would trigger
// coordinated drain logic.
func TestScaleDownOldWithMissingRole(t *testing.T) {
	baseTime := time.Now()
	// 3 roles: prefill, decode, encode - but old workload only has prefill and decode
	threeRoleNames := []string{"prefill", "decode", "encode"}

	testCases := []struct {
		name            string
		prefillBudget   int
		decodeBudget    int
		encodeBudget    int
		expectedPrefill int32
		expectedDecode  int32
	}{
		{
			name:            "missing role should not trigger coordinated drain",
			prefillBudget:   0, // planner says don't scale down prefill
			decodeBudget:    1, // planner says scale down 1 decode
			encodeBudget:    0, // encode doesn't exist in old workload, budget is 0
			expectedPrefill: 4, // should stay at 4 (no coordinated drain!)
			expectedDecode:  3, // should scale down from 4 to 3
		},
		{
			name:            "normal drain with missing role",
			prefillBudget:   1,
			decodeBudget:    1,
			encodeBudget:    0,
			expectedPrefill: 3,
			expectedDecode:  3,
		},
		{
			name:            "coordinated drain only when existing role reaches zero",
			prefillBudget:   4, // drain all prefill
			decodeBudget:    0,
			encodeBudget:    0,
			expectedPrefill: 0,
			expectedDecode:  0, // should be 0 due to coordinated drain (prefill reached 0)
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create old workload with only prefill and decode roles (no encode)
			objects := []client.Object{
				buildTestLWS("test-0-oldhash-prefill", testNamespace, "prefill", "oldhash").
					Replica(4).StatusReplicas(4).ReadyReplicas(4).CreationTimestamp(baseTime).Obj(),
				buildTestLWS("test-0-oldhash-decode", testNamespace, "decode", "oldhash").
					Replica(4).StatusReplicas(4).ReadyReplicas(4).CreationTimestamp(baseTime).Obj(),
				// Note: NO encode LWS exists in old workload
			}

			// disaggregatedsetutils.RevisionRoles only has prefill and decode
			grouped := disaggregatedsetutils.RevisionRolesList{
				{
					Revision: "oldhash",
					Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
						"prefill": makeLWS(withName("test-0-oldhash-prefill"), withReplicas(4), withCreationTimestamp(baseTime)),
						"decode":  makeLWS(withName("test-0-oldhash-decode"), withReplicas(4), withCreationTimestamp(baseTime)),
						// Note: encode is NOT in this map
					},
				},
			}

			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(objects...).WithStatusSubresource(&leaderworkersetv1.LeaderWorkerSet{}).Build()
			executor := newTestExecutor(fakeClient)
			ds := &disaggregatedsetv1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace}}

			// Convert budget to current/target format for 3 roles
			current := RoleReplicaState{
				grouped.GetTotalReplicasPerRole("prefill"),
				grouped.GetTotalReplicasPerRole("decode"),
				grouped.GetTotalReplicasPerRole("encode"), // 0 since it doesn't exist
			}
			target := RoleReplicaState{
				current[0] - tc.prefillBudget,
				current[1] - tc.decodeBudget,
				current[2] - tc.encodeBudget,
			}
			err := executor.scaleDownOld(context.TODO(), ds, grouped, threeRoleNames, current, target)
			require.NoError(t, err)

			// Verify prefill and decode were scaled correctly
			assert.Equal(t, tc.expectedPrefill,
				getTestLWSReplicas(fakeClient, testNamespace, "test-0-oldhash-prefill"),
				"prefill replicas mismatch")
			assert.Equal(t, tc.expectedDecode,
				getTestLWSReplicas(fakeClient, testNamespace, "test-0-oldhash-decode"),
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
	roleNames := testRoleNames()
	specRoleSet := map[string]bool{testRolePrefill: true, testRoleDecode: true}

	testCases := []struct {
		name                            string
		initPrefill, initDecode         int32
		workloadPrefill, workloadDecode int
		targetPrefill, targetDecode     int
		expectedPrefill, expectedDecode int32
	}{
		{"scales up prefill only", 2, 4, 2, 4, 4, 4, 4, 4},
		{"scales up decode only", 4, 2, 4, 2, 4, 4, 4, 4},
		{"scales up both roles", 1, 2, 1, 2, 4, 4, 4, 4},
		{"no-op when at target", 4, 4, 4, 4, 4, 4, 4, 4},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(
					buildTestLWS("test-0-newhash-prefill", namespace, testRolePrefill, "newhash").
						Replica(int(tc.initPrefill)).StatusReplicas(tc.initPrefill).ReadyReplicas(tc.initPrefill).CreationTimestamp(baseTime).Obj(),
					buildTestLWS("test-0-newhash-decode", namespace, testRoleDecode, "newhash").
						Replica(int(tc.initDecode)).StatusReplicas(tc.initDecode).ReadyReplicas(tc.initDecode).CreationTimestamp(baseTime).Obj(),
				).
				WithStatusSubresource(&leaderworkersetv1.LeaderWorkerSet{}).
				Build()

			executor := newTestExecutor(fakeClient)

			ds := &disaggregatedsetv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: namespace},
			}

			newRevision := disaggregatedsetutils.RevisionRoles{
				Revision: "newhash",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testRolePrefill: makeLWS(withReplicas(tc.workloadPrefill)),
					testRoleDecode:  makeLWS(withReplicas(tc.workloadDecode)),
				},
			}

			current := RoleReplicaState{tc.workloadPrefill, tc.workloadDecode}
			target := RoleReplicaState{tc.targetPrefill, tc.targetDecode}
			err := executor.scaleUpNew(context.TODO(), ds, 0, newRevision, roleNames, specRoleSet, current, target)
			require.NoError(t, err)

			assert.Equal(t, tc.expectedPrefill, getTestLWSReplicas(fakeClient, namespace, "test-0-newhash-prefill"))
			assert.Equal(t, tc.expectedDecode, getTestLWSReplicas(fakeClient, namespace, "test-0-newhash-decode"))
		})
	}
}

// =============================================================================
// Unit Tests for ensureNewLWSExists
// =============================================================================

func TestEnsureNewLWSExists(t *testing.T) {
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
				objects = append(objects, buildTestLWS("test-0-newhash-prefill", testNamespace, testRolePrefill, "newhash").
					Replica(int(tc.existingReplicas)).StatusReplicas(tc.existingReplicas).ReadyReplicas(tc.existingReplicas).CreationTimestamp(time.Now()).Obj())
			}
			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(objects...).
				WithStatusSubresource(&leaderworkersetv1.LeaderWorkerSet{}).
				Build()

			executor := newTestExecutor(fakeClient)
			podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}}
			roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
				{
					Name: testRolePrefill,
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						Replicas: ptr.To(int32(4)),
						LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
							Size:           ptr.To(int32(1)),
							WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
						},
					}},
				},
				{
					Name: testRoleDecode,
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						Replicas: ptr.To(int32(4)),
						LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
							Size:           ptr.To(int32(1)),
							WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
						},
					}},
				},
			}
			deployment := &disaggregatedsetv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "test-uid"},
				Spec:       disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
			}

			created, err := executor.ensureNewLWSExists(
				context.TODO(), deployment, 0, "newhash", testRolePrefill,
				&roles[0], 2)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedCreated, created)
			assert.Equal(t, tc.expectedReplicas, getTestLWSReplicas(fakeClient, testNamespace, "test-0-newhash-prefill"))
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
			name: "first step scales up C without drain", aPrefill: 2, aDecode: 2, bPrefill: 2, bDecode: 2, cPrefill: 0, cDecode: 0,
			expectedA: [2]int32{2, 2}, expectedB: [2]int32{2, 2}, expectedC: [2]int32{1, 1},
		},
		{
			name: "C scales up while B stays", aPrefill: 0, aDecode: 0, bPrefill: 2, bDecode: 2, cPrefill: 2, cDecode: 2,
			expectedA: [2]int32{0, 0}, expectedB: [2]int32{2, 2}, expectedC: [2]int32{3, 3},
		},
		{
			name: "drains newest old workload first", aPrefill: 2, aDecode: 2, bPrefill: 2, bDecode: 2, cPrefill: 2, cDecode: 2,
			expectedA: [2]int32{2, 2}, expectedB: [2]int32{1, 1}, expectedC: [2]int32{2, 2},
		},
	}

	initAnnot := map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "2"}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objects []client.Object
			if tc.aPrefill > 0 || tc.aDecode > 0 {
				objects = append(objects,
					buildTestLWS("test-0-hashA-prefill", testNamespace, testRolePrefill, "hashA").
						Replica(int(tc.aPrefill)).StatusReplicas(tc.aPrefill).ReadyReplicas(tc.aPrefill).CreationTimestamp(baseTime).Annotation(initAnnot).Obj(),
					buildTestLWS("test-0-hashA-decode", testNamespace, testRoleDecode, "hashA").
						Replica(int(tc.aDecode)).StatusReplicas(tc.aDecode).ReadyReplicas(tc.aDecode).CreationTimestamp(baseTime).Annotation(initAnnot).Obj())
			}
			bTime := baseTime.Add(1 * time.Hour)
			cTime := baseTime.Add(2 * time.Hour)
			objects = append(objects,
				buildTestLWS("test-0-hashB-prefill", testNamespace, testRolePrefill, "hashB").
					Replica(int(tc.bPrefill)).StatusReplicas(tc.bPrefill).ReadyReplicas(tc.bPrefill).CreationTimestamp(bTime).Annotation(initAnnot).Obj(),
				buildTestLWS("test-0-hashB-decode", testNamespace, testRoleDecode, "hashB").
					Replica(int(tc.bDecode)).StatusReplicas(tc.bDecode).ReadyReplicas(tc.bDecode).CreationTimestamp(bTime).Annotation(initAnnot).Obj(),
				buildTestLWS("test-0-hashC-prefill", testNamespace, testRolePrefill, "hashC").
					Replica(int(tc.cPrefill)).StatusReplicas(tc.cPrefill).ReadyReplicas(tc.cPrefill).CreationTimestamp(cTime).Obj(),
				buildTestLWS("test-0-hashC-decode", testNamespace, testRoleDecode, "hashC").
					Replica(int(tc.cDecode)).StatusReplicas(tc.cDecode).ReadyReplicas(tc.cDecode).CreationTimestamp(cTime).Obj())

			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(objects...).
				WithStatusSubresource(&leaderworkersetv1.LeaderWorkerSet{}).
				Build()
			executor := newTestExecutor(fakeClient)
			roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
				{Name: testRolePrefill, LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(int32(4))}}},
				{Name: testRoleDecode, LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(int32(4))}}},
			}
			deployment := &disaggregatedsetv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace},
				Spec:       disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
			}

			oldRevisions := disaggregatedsetutils.RevisionRolesList{
				{Revision: "hashA", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testRolePrefill: makeLWS(withName("test-0-hashA-prefill"), withReplicas(int(tc.aPrefill)), withReadyReplicas(int(tc.aPrefill)),
						withInitialReplicasAnnotation(2), withCreationTimestamp(baseTime)),
					testRoleDecode: makeLWS(withName("test-0-hashA-decode"), withReplicas(int(tc.aDecode)), withReadyReplicas(int(tc.aDecode)),
						withInitialReplicasAnnotation(2), withCreationTimestamp(baseTime))}},
				{Revision: "hashB", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testRolePrefill: makeLWS(withName("test-0-hashB-prefill"), withReplicas(int(tc.bPrefill)), withReadyReplicas(int(tc.bPrefill)),
						withInitialReplicasAnnotation(2), withCreationTimestamp(bTime)),
					testRoleDecode: makeLWS(withName("test-0-hashB-decode"), withReplicas(int(tc.bDecode)), withReadyReplicas(int(tc.bDecode)),
						withInitialReplicasAnnotation(2), withCreationTimestamp(bTime))}},
			}
			newRevision := disaggregatedsetutils.RevisionRoles{Revision: "hashC", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
				testRolePrefill: makeLWS(withReplicas(int(tc.cPrefill)), withReadyReplicas(int(tc.cPrefill))),
				testRoleDecode:  makeLWS(withReplicas(int(tc.cDecode)), withReadyReplicas(int(tc.cDecode)))}}

			_, err := executor.ReconcileRollingUpdate(context.TODO(), deployment, 0, oldRevisions, newRevision)
			require.NoError(t, err)

			if tc.aPrefill > 0 || tc.aDecode > 0 {
				assert.Equal(t, tc.expectedA[0], getTestLWSReplicas(fakeClient, testNamespace, "test-0-hashA-prefill"))
				assert.Equal(t, tc.expectedA[1], getTestLWSReplicas(fakeClient, testNamespace, "test-0-hashA-decode"))
			}
			assert.Equal(t, tc.expectedB[0], getTestLWSReplicas(fakeClient, testNamespace, "test-0-hashB-prefill"))
			assert.Equal(t, tc.expectedB[1], getTestLWSReplicas(fakeClient, testNamespace, "test-0-hashB-decode"))
			assert.Equal(t, tc.expectedC[0], getTestLWSReplicas(fakeClient, testNamespace, "test-0-hashC-prefill"))
			assert.Equal(t, tc.expectedC[1], getTestLWSReplicas(fakeClient, testNamespace, "test-0-hashC-decode"))
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
	assertLWSDrained(t, fakeClient, revisions.A, testRolePrefill)
	assertLWSDrained(t, fakeClient, revisions.A, testRoleDecode)
	assertLWSDrained(t, fakeClient, revisions.B, testRolePrefill)
	assertLWSDrained(t, fakeClient, revisions.B, testRoleDecode)
	assert.Equal(t, int32(2), getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-0-%s-prefill", revisions.C)))
	assert.Equal(t, int32(4), getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-0-%s-decode", revisions.C)))
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

		// Check no orphans (if one role is 0, the other must also be 0)
		aPrefill := normalize(getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-0-%s-prefill", revisions.A)))
		aDecode := normalize(getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-0-%s-decode", revisions.A)))
		bPrefill := normalize(getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-0-%s-prefill", revisions.B)))
		bDecode := normalize(getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-0-%s-decode", revisions.B)))

		assert.False(t, (aPrefill == 0) != (aDecode == 0), "Step %d: A orphaned - prefill=%d, decode=%d", i, aPrefill, aDecode)
		assert.False(t, (bPrefill == 0) != (bDecode == 0), "Step %d: B orphaned - prefill=%d, decode=%d", i, bPrefill, bDecode)
	}

	// Verify final state: A and B drained, C at target (4,3)
	assertLWSDrained(t, fakeClient, revisions.A, testRolePrefill)
	assertLWSDrained(t, fakeClient, revisions.A, testRoleDecode)
	assertLWSDrained(t, fakeClient, revisions.B, testRolePrefill)
	assertLWSDrained(t, fakeClient, revisions.B, testRoleDecode)
	assert.Equal(t, int32(4), getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-0-%s-prefill", revisions.C)))
	assert.Equal(t, int32(3), getTestLWSReplicas(fakeClient, "default", fmt.Sprintf("test-0-%s-decode", revisions.C)))
}
