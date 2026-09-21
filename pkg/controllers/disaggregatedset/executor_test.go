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
	"k8s.io/apimachinery/pkg/util/sets"
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
	recorder := events.NewFakeRecorder(100)
	return &DisaggregatedSetReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: NewServiceManager(fakeClient, scheme),
		ScalerManager:  NewScalerManager(fakeClient, recorder),
		Record:         recorder,
	}
}

// newTestExecutor creates a RollingUpdateExecutor with a FakeRecorder for testing.
func newTestExecutor(fakeClient client.Client) *RollingUpdateExecutor {
	return &RollingUpdateExecutor{
		LWSManager: NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}
}

// testDSOwnerRef is the controller OwnerReference matching the "test"/"uid"
// DisaggregatedSet fixture convention used throughout this file (see
// setupABCScenario and the inline `ds := &disaggregatedsetv1.DisaggregatedSet{
// ObjectMeta: metav1.ObjectMeta{Name: "test", ...}}` fixtures) — Scale checks
// ownership (#981), so LWS fixtures built via buildTestLWS must carry it too.
var testDSOwnerRef = metav1.OwnerReference{
	APIVersion: disaggregatedsetv1.GroupVersion.String(),
	Kind:       "DisaggregatedSet",
	Name:       "test",
	UID:        "uid",
	Controller: ptr.To(true),
}

func buildTestLWS(name, namespace, role, revision string) *wrappers.LeaderWorkerSetWrapper {
	return wrappers.BuildBasicLeaderWorkerSet(name, namespace).
		Labels(map[string]string{
			disaggregatedsetv1.RoleLabelKey:     role,
			disaggregatedsetv1.SetNameLabelKey:  "test",
			disaggregatedsetv1.SliceLabelKey:    "0",
			disaggregatedsetv1.RevisionLabelKey: revision,
		}).
		OwnerReference(testDSOwnerRef)
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
	createdAt ...time.Time,
) client.Object {
	lws := wrappers.BuildBasicLeaderWorkerSet(name, "default").
		Labels(labels).
		Replica(int(specReplicas)).
		StatusReplicas(specReplicas).
		ReadyReplicas(readyReplicas).
		OwnerReference(ownerRef).
		WorkerTemplateSpec(podSpec)
	if len(createdAt) > 0 {
		lws.CreationTimestamp(createdAt[0])
	}
	return lws.Obj()
}

func withInitialReplicas(obj client.Object, replicas int32) client.Object {
	lws := obj.(*leaderworkersetv1.LeaderWorkerSet)
	setInitialReplicasAnnotation(lws, int(replicas))
	return lws
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
		Controller: ptr.To(true),
	}
	makeLabels := func(role, revision string) map[string]string {
		return map[string]string{disaggregatedsetv1.RoleLabelKey: role, disaggregatedsetv1.SetNameLabelKey: "test", disaggregatedsetv1.SliceLabelKey: "0", disaggregatedsetv1.RevisionLabelKey: revision}
	}

	var objects []client.Object
	objects = append(objects, deployment)
	createdAt := time.Now()
	if aPrefill > 0 || aDecode > 0 {
		nameA := fmt.Sprintf("test-0-%s", revisionA)
		objects = append(objects,
			withInitialReplicas(createLWSForTest(
				nameA+"-prefill", makeLabels(testRolePrefill, revisionA),
				aPrefill, aPrefill, podSpecA, ownerRef, createdAt), targetPrefill),
			withInitialReplicas(createLWSForTest(
				nameA+"-decode", makeLabels(testRoleDecode, revisionA),
				aDecode, aDecode, podSpecA, ownerRef, createdAt), targetDecode))
	}
	createdAt = createdAt.Add(time.Second)
	if bPrefill > 0 || bDecode > 0 {
		nameB := fmt.Sprintf("test-0-%s", revisionB)
		objects = append(objects,
			withInitialReplicas(createLWSForTest(
				nameB+"-prefill", makeLabels(testRolePrefill, revisionB),
				bPrefill, bPrefill, podSpecB, ownerRef, createdAt), targetPrefill),
			withInitialReplicas(createLWSForTest(
				nameB+"-decode", makeLabels(testRoleDecode, revisionB),
				bDecode, bDecode, podSpecB, ownerRef, createdAt), targetDecode))
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
	expectOldRetained        bool
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
			name: "keeps drained old workloads until the target is ready", deployName: "test-wait-ready",
			targetReplicas: 2, oldSpec: 0, oldReady: 0, newSpec: 2, newReady: 1,
			expectRequeue: true, expectOldRetained: true,
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
				Controller: ptr.To(true),
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
			if tc.expectOldRetained {
				for _, role := range testRoleNames() {
					name := fmt.Sprintf("%s-0-%s-%s", tc.deployName, oldRevision, role)
					_, exists, _ := fetchLWSReplicas(fakeClient, name)
					assert.True(t, exists, "old %s should remain until the target is ready", role)
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

func TestReconcileExistingRolloutDoesNotReuseReadyCapacityFromPendingDrain(t *testing.T) {
	ctx := context.Background()
	podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}}
	role := makeRoleSpec(
		testRolePrefill,
		6,
		podSpec,
		intstr.FromInt(1),
		intstr.FromInt(1),
	)
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "uid"},
		Spec:       disaggregatedsetv1.DisaggregatedSetSpec{Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{role}},
	}
	newRevision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
	const oldRevision = "old"
	labels := func(revision string) map[string]string {
		return map[string]string{
			disaggregatedsetv1.RoleLabelKey:     testRolePrefill,
			disaggregatedsetv1.SetNameLabelKey:  ds.Name,
			disaggregatedsetv1.SliceLabelKey:    "0",
			disaggregatedsetv1.RevisionLabelKey: revision,
		}
	}
	oldName := fmt.Sprintf("%s-0-%s-%s", ds.Name, oldRevision, testRolePrefill)
	newName := fmt.Sprintf("%s-0-%s-%s", ds.Name, newRevision, testRolePrefill)
	oldLWS := withInitialReplicas(createLWSForTest(
		oldName, labels(oldRevision), 4, 3, podSpec, testDSOwnerRef,
	), 6)
	newLWS := withInitialReplicas(createLWSForTest(
		newName, labels(newRevision), 3, 3, podSpec, testDSOwnerRef,
	), 6)
	fakeClient := fake.NewClientBuilder().
		WithScheme(testSchemeForUnit()).
		WithObjects(oldLWS, newLWS).
		WithStatusSubresource(statusSubresourceObjects()...).
		Build()
	executor := newTestExecutor(fakeClient)

	reconcile := func() {
		oldRevisions, currentRevision, err := executor.LWSManager.GetRevisionRolesList(ctx, ds, 0, newRevision)
		require.NoError(t, err)
		require.NotNil(t, currentRevision)
		_, complete, err := executor.reconcileExistingRollout(
			ctx, ds, oldRevisions, *currentRevision, map[string]int{testRolePrefill: 6},
		)
		require.NoError(t, err)
		assert.False(t, complete)
	}

	// The first reconciliation spends the only replica above the availability
	// floor and requests a scale-down from four old replicas to three.
	reconcile()
	assert.EqualValues(t, 3, getTestLWSReplicas(fakeClient, testNamespace, oldName))

	// Keep status stale to model the next reconciliation arriving before the
	// underlying scale-down finishes. status.replicas=4 exposes one pending
	// deletion; status.readyReplicas=3 must not be treated as three survivors.
	var observedOld leaderworkersetv1.LeaderWorkerSet
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: oldName}, &observedOld))
	assert.EqualValues(t, 4, observedOld.Status.Replicas)
	assert.EqualValues(t, 3, observedOld.Status.ReadyReplicas)
	reconcile()
	assert.EqualValues(t, 3, getTestLWSReplicas(fakeClient, testNamespace, oldName),
		"a pending deletion must reserve the Ready replica it may remove")

	// If status catches up and confirms that three Ready replicas survived, the
	// availability slot is real again and the rollout may continue.
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: oldName}, &observedOld))
	observedOld.Status.Replicas = 3
	observedOld.Status.ReadyReplicas = 3
	require.NoError(t, fakeClient.Status().Update(ctx, &observedOld))
	reconcile()
	assert.EqualValues(t, 2, getTestLWSReplicas(fakeClient, testNamespace, oldName))
}

func TestBuildRolloutSnapshotPreservesExternalSpecDuringRollout(t *testing.T) {
	const roleName = "prefill"
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{{
			Name:    roleName,
			Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal},
		}}},
	}
	newRevision := disaggregatedsetutils.RevisionRoles{
		Revision: "new",
		Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			roleName: makeLWS(withReplicas(5), withReadyReplicas(3)),
		},
	}
	oldRevisions := disaggregatedsetutils.RevisionRolesList{{Revision: "old"}}
	desiredReplicasByRole := map[string]int{roleName: 2}
	config := []RollingUpdateConfig{{MaxSurge: 1}}

	state := buildRolloutSnapshot(
		ds,
		[]string{roleName},
		sets.New(roleName),
		oldRevisions,
		newRevision,
		desiredReplicasByRole,
		config,
	)

	assert.Equal(t, 5, state[0].NewSpecReplicas, "planner progress must follow issued spec")
	assert.Equal(t, 3, state[0].NewReadyReplicas, "availability must follow ready replicas")
	assert.Equal(t, 5, state[0].NewTargetReplicas, "an HPA shrink must not reduce the in-flight new revision spec")
}

func TestSelectRevisionToDrainPrefersUnreadyOverNewest(t *testing.T) {
	createdAt := time.Now()
	oldestUnready := disaggregatedsetutils.RevisionRoles{Revision: "A", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: makeLWS(withReplicas(1), withCreationTimestamp(createdAt)),
	}}
	newestReady := disaggregatedsetutils.RevisionRoles{Revision: "B", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: makeLWS(withReplicas(1), withReadyReplicas(1), withCreationTimestamp(createdAt.Add(time.Second))),
	}}

	selected, found, fullyUnready := selectRevisionToDrain(disaggregatedsetutils.RevisionRolesList{oldestUnready, newestReady})
	require.True(t, found)
	assert.True(t, fullyUnready)
	assert.Equal(t, "A", selected.Revision)

	oldestUnready.Roles[testRolePrefill].Status.ReadyReplicas = 1
	selected, found, fullyUnready = selectRevisionToDrain(disaggregatedsetutils.RevisionRolesList{oldestUnready, newestReady})
	require.True(t, found)
	assert.False(t, fullyUnready)
	assert.Equal(t, "B", selected.Revision)
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
			config := extractRollingUpdateConfig(ds, roleNames, resolveDesiredReplicasByRole(ds, nil))

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
			config := extractRollingUpdateConfig(ds, roleNames, resolveDesiredReplicasByRole(ds, nil))

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
	statusPrefill, statusDecode     int32
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
			// Planned drain (1P, 2D) would leave the incomplete state (2P, 0D).
			// Budget cannot cover full retirement (P=3 > budget=1), so convert
			// the full D drain into the largest partial drain. Both roles retain
			// at least one replica.
			name:          "aliveness skips orphaning drain when retire not budgeted",
			workloads:     []lwsDef{{revision: "hash1", prefill: 3, decode: 2, ageHours: 0, expectedPrefill: 2, expectedDecode: 1}},
			prefillBudget: 1, decodeBudget: 2,
		},
		{
			name:          "budget exhaustion stops mid-workload",
			workloads:     []lwsDef{{revision: "hash1", prefill: 6, decode: 6, ageHours: 0, expectedPrefill: 4, expectedDecode: 4}},
			prefillBudget: 2, decodeBudget: 2,
		},
		{
			// Newest-first drain-order: only ONE revision is drained per
			// call (executor breaks after any revision receives drain calls).
			// Next reconcile handles the middle. This preserves the
			// observable "B drained to 0 before A" property.
			name: "three workloads drain newest only, per call",
			workloads: []lwsDef{
				{revision: "oldest", prefill: 2, decode: 2, ageHours: 0, expectedPrefill: 2, expectedDecode: 2},
				{revision: "middle", prefill: 2, decode: 2, ageHours: 1, expectedPrefill: 2, expectedDecode: 2},
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
		{
			name: "retired newest revision with stale status is skipped",
			workloads: []lwsDef{
				{revision: "old", prefill: 4, decode: 4, expectedPrefill: 3, expectedDecode: 3},
				{revision: "retired", statusPrefill: 1, statusDecode: 1, ageHours: 1},
			},
			prefillBudget: 1, decodeBudget: 1,
		},
		{
			name: "whole revision retirement cannot exceed safe drain",
			workloads: []lwsDef{
				{revision: "oldest", prefill: 2, decode: 1, expectedPrefill: 2, expectedDecode: 1},
				{revision: "newest", prefill: 1, decode: 2, ageHours: 1, expectedPrefill: 1, expectedDecode: 1},
			},
			prefillBudget: 1, decodeBudget: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objects []client.Object
			var grouped disaggregatedsetutils.RevisionRolesList
			for _, workload := range tc.workloads {
				creationTime := baseTime.Add(time.Duration(workload.ageHours) * time.Hour)
				baseName := fmt.Sprintf("test-0-%s", workload.revision)
				statusPrefill := max(workload.prefill, workload.statusPrefill)
				statusDecode := max(workload.decode, workload.statusDecode)
				objects = append(objects,
					buildTestLWS(baseName+"-prefill", testNamespace, testRolePrefill, workload.revision).
						Replica(int(workload.prefill)).StatusReplicas(statusPrefill).ReadyReplicas(workload.prefill).CreationTimestamp(creationTime).Obj(),
					buildTestLWS(baseName+"-decode", testNamespace, testRoleDecode, workload.revision).
						Replica(int(workload.decode)).StatusReplicas(statusDecode).ReadyReplicas(workload.decode).CreationTimestamp(creationTime).Obj())
				grouped = append(grouped, disaggregatedsetutils.RevisionRoles{
					Revision: workload.revision,
					Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
						testRolePrefill: makeLWS(withName(baseName+"-prefill"), withReplicas(int(workload.prefill)), withReadyReplicas(int(workload.prefill)), withCreationTimestamp(creationTime)),
						testRoleDecode:  makeLWS(withName(baseName+"-decode"), withReplicas(int(workload.decode)), withReadyReplicas(int(workload.decode)), withCreationTimestamp(creationTime)),
					},
				})
			}

			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(objects...).WithStatusSubresource(&leaderworkersetv1.LeaderWorkerSet{}).Build()
			executor := newTestExecutor(fakeClient)
			ds := &disaggregatedsetv1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "uid"}}

			// Convert budget to current/target format
			current := RoleReplicaState{
				grouped.GetTotalReplicasPerRole(testRolePrefill),
				grouped.GetTotalReplicasPerRole(testRoleDecode),
			}
			target := RoleReplicaState{current[0] - tc.prefillBudget, current[1] - tc.decodeBudget}
			state := rolloutSnapshot{
				{
					InitialOldReplicas: current[0], OldSpecReplicas: current[0], OldReadyReplicas: current[0], NewTargetReplicas: current[0],
					Config: RollingUpdateConfig{MaxUnavailable: tc.prefillBudget},
				},
				{
					InitialOldReplicas: current[1], OldSpecReplicas: current[1], OldReadyReplicas: current[1], NewTargetReplicas: current[1],
					Config: RollingUpdateConfig{MaxUnavailable: tc.decodeBudget},
				},
			}
			err := executor.scaleDownOld(context.TODO(), ds, grouped, roleNames,
				state, target, make(RoleReplicaState, len(roleNames)), nil)
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

func TestCoordinateRevisionDrain(t *testing.T) {
	roles := func(prefill, decode int) map[string]*leaderworkersetv1.LeaderWorkerSet {
		return map[string]*leaderworkersetv1.LeaderWorkerSet{
			testRolePrefill: makeLWS(withReplicas(prefill), withReadyReplicas(prefill)),
			testRoleDecode:  makeLWS(withReplicas(decode), withReadyReplicas(decode)),
		}
	}

	t.Run("retires the whole revision when every role is safe", func(t *testing.T) {
		drain := RoleReplicaState{1, 0}
		newTargets := RoleReplicaState{1, 1}
		snapshot := rolloutSnapshot{
			{InitialOldReplicas: 1, OldSpecReplicas: 1, OldReadyReplicas: 1, NewSpecReplicas: 1, NewReadyReplicas: 1, NewTargetReplicas: 1},
			{InitialOldReplicas: 1, OldSpecReplicas: 1, OldReadyReplicas: 1, NewSpecReplicas: 1, NewReadyReplicas: 1, NewTargetReplicas: 1},
		}

		blocked := coordinateRevisionDrain(testRoleNames(), roles(1, 1), drain, newTargets, nil, snapshot)

		assert.False(t, blocked)
		assert.Equal(t, RoleReplicaState{1, 1}, drain)
	})

	t.Run("turns a full drain into a partial drain", func(t *testing.T) {
		drain := RoleReplicaState{2, 0}
		newTargets := RoleReplicaState{0, 1}
		snapshot := rolloutSnapshot{
			{InitialOldReplicas: 2, OldSpecReplicas: 2, OldReadyReplicas: 2, NewTargetReplicas: 1, Config: RollingUpdateConfig{MaxUnavailable: 1}},
			{InitialOldReplicas: 3, OldSpecReplicas: 1, OldReadyReplicas: 1, NewSpecReplicas: 1, NewReadyReplicas: 1, NewTargetReplicas: 3, Config: RollingUpdateConfig{MaxUnavailable: 1}},
		}

		blocked := coordinateRevisionDrain(testRoleNames(), roles(2, 1), drain, newTargets, nil, snapshot)

		assert.False(t, blocked)
		assert.Equal(t, RoleReplicaState{1, 0}, drain)
		assert.Equal(t, RoleReplicaState{0, 1}, newTargets)
	})

	t.Run("uses another partial drain when a singleton cannot retire", func(t *testing.T) {
		// Aggregate A+B progress asks to remove B's only Decode. That would
		// leave B incomplete, but one of B's two Prefill replicas can safely
		// drain and open capacity for C instead.
		drain := RoleReplicaState{0, 1}
		newTargets := RoleReplicaState{4, 1}
		snapshot := rolloutSnapshot{
			{InitialOldReplicas: 6, OldSpecReplicas: 3, OldReadyReplicas: 3, NewSpecReplicas: 4, NewReadyReplicas: 4, NewTargetReplicas: 6,
				Config: RollingUpdateConfig{MaxSurge: 1}},
			{InitialOldReplicas: 2, OldSpecReplicas: 2, OldReadyReplicas: 2, NewSpecReplicas: 1, NewReadyReplicas: 1, NewTargetReplicas: 2,
				Config: RollingUpdateConfig{MaxSurge: 1}},
		}

		blocked := coordinateRevisionDrain(testRoleNames(), roles(2, 1), drain, newTargets, nil, snapshot)

		assert.False(t, blocked)
		assert.Equal(t, RoleReplicaState{1, 0}, drain)
		assert.Equal(t, RoleReplicaState{4, 1}, newTargets)
	})

	t.Run("grows the role blocking coordinated retirement", func(t *testing.T) {
		drain := RoleReplicaState{1, 0}
		newTargets := RoleReplicaState{0, 1}
		snapshot := rolloutSnapshot{
			{InitialOldReplicas: 1, OldSpecReplicas: 1, OldReadyReplicas: 1, NewTargetReplicas: 1, Config: RollingUpdateConfig{MaxUnavailable: 1}},
			{InitialOldReplicas: 3, OldSpecReplicas: 1, OldReadyReplicas: 1, NewSpecReplicas: 1, NewReadyReplicas: 1, NewTargetReplicas: 3, Config: RollingUpdateConfig{MaxUnavailable: 1}},
		}

		blocked := coordinateRevisionDrain(testRoleNames(), roles(1, 1), drain, newTargets, nil, snapshot)

		assert.False(t, blocked)
		assert.Equal(t, RoleReplicaState{0, 0}, drain)
		assert.Equal(t, RoleReplicaState{0, 2}, newTargets)
	})

	t.Run("does not grow past the active revision phase", func(t *testing.T) {
		drain := RoleReplicaState{1, 0}
		newTargets := RoleReplicaState{0, 1}
		snapshot := rolloutSnapshot{
			{InitialOldReplicas: 1, OldSpecReplicas: 1, OldReadyReplicas: 1, NewTargetReplicas: 1, Config: RollingUpdateConfig{MaxUnavailable: 1}},
			{InitialOldReplicas: 3, OldSpecReplicas: 1, OldReadyReplicas: 1, NewSpecReplicas: 1, NewReadyReplicas: 1, NewTargetReplicas: 3, Config: RollingUpdateConfig{MaxUnavailable: 1}},
		}

		blocked := coordinateRevisionDrain(testRoleNames(), roles(1, 1), drain, newTargets, RoleReplicaState{0, 2}, snapshot)

		assert.True(t, blocked)
		assert.Equal(t, RoleReplicaState{0, 0}, drain)
		assert.Equal(t, RoleReplicaState{0, 1}, newTargets)
	})

	t.Run("waits when no partial drain or additional growth is possible", func(t *testing.T) {
		drain := RoleReplicaState{1, 0}
		newTargets := RoleReplicaState{0, 2}
		snapshot := rolloutSnapshot{
			{InitialOldReplicas: 1, OldSpecReplicas: 1, OldReadyReplicas: 1, NewTargetReplicas: 1, Config: RollingUpdateConfig{MaxUnavailable: 1}},
			{InitialOldReplicas: 3, OldSpecReplicas: 1, OldReadyReplicas: 1, NewSpecReplicas: 2, NewReadyReplicas: 1, NewTargetReplicas: 3, Config: RollingUpdateConfig{MaxUnavailable: 1}},
		}

		blocked := coordinateRevisionDrain(testRoleNames(), roles(1, 1), drain, newTargets, nil, snapshot)

		assert.True(t, blocked)
		assert.Equal(t, RoleReplicaState{0, 0}, drain)
		assert.Equal(t, RoleReplicaState{0, 2}, newTargets)
	})
}

func TestReconcileExistingRolloutCoordinatesRetirementWhileRoleReadinessIsSlow(t *testing.T) {
	createdAt := time.Now()
	initialOne := map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "1"}
	initialTwo := map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "2"}
	oldPrefill := buildTestLWS("test-0-oldhash-prefill", testNamespace, testRolePrefill, "oldhash").
		Replica(1).StatusReplicas(1).ReadyReplicas(1).CreationTimestamp(createdAt).Annotation(initialOne).Obj()
	oldDecode := buildTestLWS("test-0-oldhash-decode", testNamespace, testRoleDecode, "oldhash").
		Replica(1).StatusReplicas(1).ReadyReplicas(1).CreationTimestamp(createdAt).Annotation(initialTwo).Obj()
	newPrefill := buildTestLWS("test-0-newhash-prefill", testNamespace, testRolePrefill, "newhash").
		Replica(1).StatusReplicas(1).ReadyReplicas(1).CreationTimestamp(createdAt.Add(time.Second)).Obj()
	newDecode := buildTestLWS("test-0-newhash-decode", testNamespace, testRoleDecode, "newhash").
		Replica(2).StatusReplicas(2).ReadyReplicas(1).CreationTimestamp(createdAt.Add(time.Second)).Obj()
	fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
		WithObjects(oldPrefill, oldDecode, newPrefill, newDecode).
		WithStatusSubresource(&leaderworkersetv1.LeaderWorkerSet{}).Build()
	executor := newTestExecutor(fakeClient)
	one, zero := intstr.FromInt(1), intstr.FromInt(0)
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "uid"},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{
			makeRoleSpec(testRolePrefill, 1, corev1.PodSpec{}, one, zero),
			makeRoleSpec(testRoleDecode, 2, corev1.PodSpec{}, one, zero),
		}},
	}
	oldRevision := disaggregatedsetutils.RevisionRoles{Revision: "oldhash", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: oldPrefill,
		testRoleDecode:  oldDecode,
	}}
	newRevision := disaggregatedsetutils.RevisionRoles{Revision: "newhash", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: newPrefill,
		testRoleDecode:  newDecode,
	}}
	reconcile := func() {
		_, _, err := executor.reconcileExistingRollout(context.TODO(), ds,
			disaggregatedsetutils.RevisionRolesList{oldRevision}, newRevision, resolveDesiredReplicasByRole(ds, nil))
		require.NoError(t, err)
	}

	// Decode still has a pending replacement, so both old roles remain until
	// they can retire together.
	reconcile()
	reconcile()
	assert.Equal(t, int32(1), getTestLWSReplicas(fakeClient, testNamespace, oldPrefill.Name))
	assert.Equal(t, int32(1), getTestLWSReplicas(fakeClient, testNamespace, oldDecode.Name))

	newDecode.Status.ReadyReplicas = 2
	reconcile()
	assert.Zero(t, getTestLWSReplicas(fakeClient, testNamespace, oldPrefill.Name))
	assert.Zero(t, getTestLWSReplicas(fakeClient, testNamespace, oldDecode.Name))
}

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
			// Retiring P would leave the old revision without one of its roles.
			// D cannot retire, so P keeps one replica instead.
			name:            "full drain leaves one replica when the revision cannot retire",
			prefillBudget:   4,
			decodeBudget:    0,
			encodeBudget:    0,
			expectedPrefill: 1,
			expectedDecode:  4,
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
						"prefill": makeLWS(withName("test-0-oldhash-prefill"), withReplicas(4), withReadyReplicas(4), withCreationTimestamp(baseTime)),
						"decode":  makeLWS(withName("test-0-oldhash-decode"), withReplicas(4), withReadyReplicas(4), withCreationTimestamp(baseTime)),
						// Note: encode is NOT in this map
					},
				},
			}

			fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
				WithObjects(objects...).WithStatusSubresource(&leaderworkersetv1.LeaderWorkerSet{}).Build()
			executor := newTestExecutor(fakeClient)
			ds := &disaggregatedsetv1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "uid"}}

			// Convert budget to current/target format for 3 roles
			current := RoleReplicaState{
				grouped.GetTotalReplicasPerRole("prefill"),
				grouped.GetTotalReplicasPerRole("decode"),
				grouped.GetTotalReplicasPerRole("encode"),
			}
			target := RoleReplicaState{
				current[0] - tc.prefillBudget,
				current[1] - tc.decodeBudget,
				current[2] - tc.encodeBudget,
			}
			state := rolloutSnapshot{
				{
					InitialOldReplicas: current[0], OldSpecReplicas: current[0], OldReadyReplicas: current[0], NewTargetReplicas: current[0],
					Config: RollingUpdateConfig{MaxUnavailable: tc.prefillBudget},
				},
				{
					InitialOldReplicas: current[1], OldSpecReplicas: current[1], OldReadyReplicas: current[1], NewTargetReplicas: current[1],
					Config: RollingUpdateConfig{MaxUnavailable: tc.decodeBudget},
				},
				{
					InitialOldReplicas: current[2], OldSpecReplicas: current[2], OldReadyReplicas: current[2], NewTargetReplicas: current[2],
					Config: RollingUpdateConfig{MaxUnavailable: tc.encodeBudget},
				},
			}
			err := executor.scaleDownOld(context.TODO(), ds, grouped, threeRoleNames,
				state, target, make(RoleReplicaState, len(threeRoleNames)), nil)
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
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: namespace, UID: "uid"},
			}

			newRevision := disaggregatedsetutils.RevisionRoles{
				Revision: "newhash",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testRolePrefill: makeLWS(withName("test-0-newhash-prefill"), withReplicas(tc.workloadPrefill)),
					testRoleDecode:  makeLWS(withName("test-0-newhash-decode"), withReplicas(tc.workloadDecode)),
				},
			}

			target := RoleReplicaState{tc.targetPrefill, tc.targetDecode}
			err := executor.scaleUpNew(context.TODO(), ds, newRevision, roleNames, target)
			require.NoError(t, err)

			assert.Equal(t, tc.expectedPrefill, getTestLWSReplicas(fakeClient, namespace, "test-0-newhash-prefill"))
			assert.Equal(t, tc.expectedDecode, getTestLWSReplicas(fakeClient, namespace, "test-0-newhash-decode"))
		})
	}
}

func TestOldInitialReplicasAreBackfilledOnlyOnce(t *testing.T) {
	ctx := context.Background()
	ds := &disaggregatedsetv1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{
		Name: "test", Namespace: testNamespace, UID: "uid",
	}}
	lws := buildTestLWS("test-0-hashA-prefill", testNamespace, testRolePrefill, "hashA").Replica(5).Obj()
	fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).WithObjects(lws).Build()
	executor := newTestExecutor(fakeClient)
	recorder := events.NewFakeRecorder(10)
	executor.Record = recorder
	old := disaggregatedsetutils.RevisionRolesList{{Revision: "hashA", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: lws,
	}}}

	require.NoError(t, executor.ensureOldInitialReplicas(ctx, ds, old))
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, corev1.EventTypeWarning)
		assert.Contains(t, event, EventReasonInitialReplicasMissing)
		assert.Contains(t, event, lws.Name)
	default:
		t.Fatal("expected a warning event for the missing initial-replicas annotation")
	}
	stored, err := executor.LWSManager.Get(ctx, ds, lws.Name)
	require.NoError(t, err)
	require.NotNil(t, stored)
	initial, ok := disaggregatedsetutils.GetInitialReplicas(stored)
	require.True(t, ok)
	assert.EqualValues(t, 5, initial)

	// Simulate a later drain. The old revision's target must remain 5 rather
	// than following its now-partial Spec down to 2.
	stored.Spec.Replicas = ptr.To(int32(2))
	require.NoError(t, fakeClient.Update(ctx, stored))
	old[0].Roles[testRolePrefill] = stored
	require.NoError(t, executor.ensureOldInitialReplicas(ctx, ds, old))
	stored, err = executor.LWSManager.Get(ctx, ds, lws.Name)
	require.NoError(t, err)
	initial, ok = disaggregatedsetutils.GetInitialReplicas(stored)
	require.True(t, ok)
	assert.EqualValues(t, 5, initial)
	select {
	case event := <-recorder.Events:
		t.Fatalf("valid initial-replicas should not emit another warning: %s", event)
	default:
	}
}

func TestExternalTargetUpdatesCurrentRevisionInitialReplicas(t *testing.T) {
	ctx := context.Background()
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "uid"},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{{
			Name:    testRolePrefill,
			Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal},
		}}},
	}
	lws := buildTestLWS("test-0-hashB-prefill", testNamespace, testRolePrefill, "hashB").
		Replica(2).
		Annotation(map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "2"}).
		Obj()
	fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).WithObjects(lws).Build()
	executor := newTestExecutor(fakeClient)
	current := disaggregatedsetutils.RevisionRoles{Revision: "hashB", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: lws,
	}}
	desiredReplicasByRole := map[string]int{testRolePrefill: 6}

	require.NoError(t, executor.syncTargetInitialReplicas(ctx, ds, []string{testRolePrefill}, current, desiredReplicasByRole))
	stored, err := executor.LWSManager.Get(ctx, ds, lws.Name)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.EqualValues(t, 2, *stored.Spec.Replicas, "recording the completed target must not jump the in-progress Spec")
	initial, ok := disaggregatedsetutils.GetInitialReplicas(stored)
	require.True(t, ok)
	assert.EqualValues(t, 6, initial)
}

// =============================================================================
// Integration Tests for Multiple Old Workloads (A→B→C rollout scenario)
// =============================================================================

func TestReconcileRevisionTransitionRepairsPartialTargetRevision(t *testing.T) {
	ctx := context.Background()
	one, zero := intstr.FromInt(1), intstr.FromInt(0)
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "uid"},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{
			makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, one, zero),
			makeRoleSpec(testRoleDecode, 4, corev1.PodSpec{}, one, zero),
		}},
	}
	targetRevision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
	initialFour := map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "4"}
	oldPrefill := buildTestLWS("test-0-oldhash-prefill", testNamespace, testRolePrefill, "oldhash").
		Replica(4).ReadyReplicas(4).Annotation(initialFour).Obj()
	oldDecode := buildTestLWS("test-0-oldhash-decode", testNamespace, testRoleDecode, "oldhash").
		Replica(4).ReadyReplicas(4).Annotation(initialFour).Obj()
	newPrefillName := disaggregatedsetutils.GenerateName(ds.Name, 0, targetRevision, testRolePrefill)
	newPrefill := buildTestLWS(newPrefillName, testNamespace, testRolePrefill, targetRevision).
		Replica(0).ReadyReplicas(0).Annotation(initialFour).Obj()
	fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
		WithObjects(oldPrefill, oldDecode, newPrefill).WithStatusSubresource(&leaderworkersetv1.LeaderWorkerSet{}).Build()
	executor := newTestExecutor(fakeClient)

	result, complete, err := executor.ReconcileRevisionTransition(ctx, ds, 0, targetRevision, map[string]int{
		testRolePrefill: 4,
		testRoleDecode:  4,
	})

	require.NoError(t, err)
	assert.False(t, complete)
	assert.NotZero(t, result.RequeueAfter)
	newDecodeName := disaggregatedsetutils.GenerateName(ds.Name, 0, targetRevision, testRoleDecode)
	newDecode, err := executor.LWSManager.Get(ctx, ds, newDecodeName)
	require.NoError(t, err)
	require.NotNil(t, newDecode, "the missing role must be recreated before rollout planning")
	assert.Zero(t, getLWSReplicas(newDecode))
	initial, ok := disaggregatedsetutils.GetInitialReplicas(newDecode)
	require.True(t, ok)
	assert.EqualValues(t, 4, initial)
	assert.Equal(t, int32(4), getTestLWSReplicas(fakeClient, testNamespace, oldPrefill.Name))
	assert.Equal(t, int32(4), getTestLWSReplicas(fakeClient, testNamespace, oldDecode.Name))
}

func TestInterruptedRolloutKeepsInitialBaseline(t *testing.T) {
	ctx := context.Background()
	roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
		{Name: testRolePrefill, LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(int32(6))}}},
		{Name: testRoleDecode, LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(int32(3))}}},
	}
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "uid"},
		Spec:       disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
	}
	initialP := map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "6"}
	initialD := map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "3"}
	objects := []client.Object{ds,
		buildTestLWS("test-0-hashA-prefill", testNamespace, testRolePrefill, "hashA").Replica(5).ReadyReplicas(5).Annotation(initialP).Obj(),
		buildTestLWS("test-0-hashA-decode", testNamespace, testRoleDecode, "hashA").Replica(2).ReadyReplicas(2).Annotation(initialD).Obj(),
		buildTestLWS("test-0-hashB-prefill", testNamespace, testRolePrefill, "hashB").Replica(3).ReadyReplicas(3).Annotation(initialP).Obj(),
		buildTestLWS("test-0-hashB-decode", testNamespace, testRoleDecode, "hashB").Replica(2).ReadyReplicas(2).Annotation(initialD).Obj(),
	}
	fakeClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
		WithObjects(objects...).
		WithStatusSubresource(statusSubresourceObjects()...).
		Build()
	executor := newTestExecutor(fakeClient)

	desiredReplicasByRole := resolveDesiredReplicasByRole(ds, nil)
	targetRevision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
	result, _, err := executor.ReconcileRevisionTransition(ctx, ds, 0, targetRevision, desiredReplicasByRole)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	old, current, err := executor.LWSManager.GetRevisionRolesList(ctx, ds, 0, targetRevision)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Equal(t, 6, old.GetMaxInitialReplicasPerRole(testRolePrefill))
	assert.Equal(t, 3, old.GetMaxInitialReplicasPerRole(testRoleDecode))
	assert.Equal(t, 8, old.GetTotalReplicasPerRole(testRolePrefill), "physical occupancy still sums A and B")
	assert.Equal(t, 4, old.GetTotalReplicasPerRole(testRoleDecode))

	for role, want := range map[string]int32{testRolePrefill: 6, testRoleDecode: 3} {
		lws := current.Roles[role]
		require.NotNil(t, lws)
		assert.Zero(t, getLWSReplicas(lws), "C starts at zero")
		got, ok := disaggregatedsetutils.GetInitialReplicas(lws)
		require.True(t, ok)
		assert.Equal(t, want, got, "C records the target it should eventually reach")
	}

	for _, role := range testRoleNames() {
		var b leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{
			Namespace: testNamespace,
			Name:      fmt.Sprintf("test-0-hashB-%s", role),
		}, &b))
		require.NoError(t, fakeClient.Delete(ctx, &b))
	}

	old, _, err = executor.LWSManager.GetRevisionRolesList(ctx, ds, 0, targetRevision)
	require.NoError(t, err)
	assert.Equal(t, 6, old.GetMaxInitialReplicasPerRole(testRolePrefill), "removing partial B must not reduce the baseline to A's current 5")
	assert.Equal(t, 3, old.GetMaxInitialReplicasPerRole(testRoleDecode), "removing partial B must not reduce the baseline to A's current 2")
	assert.Equal(t, 5, old.GetTotalReplicasPerRole(testRolePrefill))
	assert.Equal(t, 2, old.GetTotalReplicasPerRole(testRoleDecode))
}

// abcExecutorScenario defines inputs for reconcileExistingRollout executor tests.
type abcExecutorScenario struct {
	name                            string
	targetPrefill, targetDecode     int32
	aPrefill, aDecode               int32
	bPrefill, bDecode               int32
	cPrefill, cDecode               int32
	aUnready                        bool
	expectedA, expectedB, expectedC [2]int32 // [prefill, decode]
}

func TestReconcileExistingRolloutABCScenario(t *testing.T) {
	baseTime := time.Now()

	testCases := []abcExecutorScenario{
		{
			// surge=1 unavail=0 (defaults). total=4=floor, specDrainHeadroom=0 so no drain
			// this reconcile. The planner scales up C using only the surge headroom.
			name: "first step scales up C using surge (no drain budget)", targetPrefill: 4, targetDecode: 4,
			aPrefill: 2, aDecode: 2, bPrefill: 2, bDecode: 2, cPrefill: 0, cDecode: 0,
			expectedA: [2]int32{2, 2}, expectedB: [2]int32{2, 2}, expectedC: [2]int32{1, 1},
		},
		{
			// curOld=2 curNew=2 total=4, ceiling=5, specDrainHeadroom=0. Planner scales C
			// further (using surge). Drain of B happens on a later reconcile, after
			// the surge step opens drain budget.
			name: "C scales up using surge (drain deferred to next reconcile)", targetPrefill: 4, targetDecode: 4,
			aPrefill: 0, aDecode: 0, bPrefill: 2, bDecode: 2, cPrefill: 2, cDecode: 2,
			expectedA: [2]int32{0, 0}, expectedB: [2]int32{2, 2}, expectedC: [2]int32{3, 3},
		},
		{
			// total=6 already exceeds ceiling=5, addBudget=0. The fractional
			// planner caps drain at the next old-side target rather than using
			// the full drain budget. Drain 1 from newest old (B); subsequent
			// reconciles continue one fractional step at a time.
			name: "above ceiling: drains newest old workload to recover", targetPrefill: 4, targetDecode: 4,
			aPrefill: 2, aDecode: 2, bPrefill: 2, bDecode: 2, cPrefill: 2, cDecode: 2,
			expectedA: [2]int32{2, 2}, expectedB: [2]int32{1, 1}, expectedC: [2]int32{2, 2},
		},
		{
			name: "parks A while draining asymmetric B", targetPrefill: 6, targetDecode: 2,
			aPrefill: 1, aDecode: 1, bPrefill: 2, bDecode: 1, cPrefill: 4, cDecode: 1,
			expectedA: [2]int32{1, 1}, expectedB: [2]int32{1, 1}, expectedC: [2]int32{4, 1},
		},
		{
			name: "caps C at the residual target while A is parked", targetPrefill: 6, targetDecode: 2,
			aPrefill: 1, aDecode: 1, bPrefill: 1, bDecode: 1, cPrefill: 4, cDecode: 1,
			expectedA: [2]int32{1, 1}, expectedB: [2]int32{1, 1}, expectedC: [2]int32{5, 1},
		},
		{
			name: "drains an unready revision before the newest", targetPrefill: 4, targetDecode: 4,
			aPrefill: 1, aDecode: 1, bPrefill: 1, bDecode: 1, cPrefill: 2, cDecode: 2, aUnready: true,
			expectedA: [2]int32{0, 0}, expectedB: [2]int32{1, 1}, expectedC: [2]int32{2, 2},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			initialPrefill := map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: fmt.Sprint(tc.targetPrefill)}
			initialDecode := map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: fmt.Sprint(tc.targetDecode)}
			aReadyPrefill, aReadyDecode := tc.aPrefill, tc.aDecode
			if tc.aUnready {
				aReadyPrefill, aReadyDecode = 0, 0
			}
			var objects []client.Object
			if tc.aPrefill > 0 || tc.aDecode > 0 {
				objects = append(objects,
					buildTestLWS("test-0-hashA-prefill", testNamespace, testRolePrefill, "hashA").
						Replica(int(tc.aPrefill)).StatusReplicas(tc.aPrefill).ReadyReplicas(aReadyPrefill).CreationTimestamp(baseTime).Annotation(initialPrefill).Obj(),
					buildTestLWS("test-0-hashA-decode", testNamespace, testRoleDecode, "hashA").
						Replica(int(tc.aDecode)).StatusReplicas(tc.aDecode).ReadyReplicas(aReadyDecode).CreationTimestamp(baseTime).Annotation(initialDecode).Obj())
			}
			bTime := baseTime.Add(1 * time.Hour)
			cTime := baseTime.Add(2 * time.Hour)
			objects = append(objects,
				buildTestLWS("test-0-hashB-prefill", testNamespace, testRolePrefill, "hashB").
					Replica(int(tc.bPrefill)).StatusReplicas(tc.bPrefill).ReadyReplicas(tc.bPrefill).CreationTimestamp(bTime).Annotation(initialPrefill).Obj(),
				buildTestLWS("test-0-hashB-decode", testNamespace, testRoleDecode, "hashB").
					Replica(int(tc.bDecode)).StatusReplicas(tc.bDecode).ReadyReplicas(tc.bDecode).CreationTimestamp(bTime).Annotation(initialDecode).Obj(),
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
				{Name: testRolePrefill, LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(tc.targetPrefill)}}},
				{Name: testRoleDecode, LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(tc.targetDecode)}}},
			}
			deployment := &disaggregatedsetv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "uid"},
				Spec:       disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
			}

			oldRevisions := disaggregatedsetutils.RevisionRolesList{
				{Revision: "hashA", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testRolePrefill: makeLWS(withName("test-0-hashA-prefill"), withReplicas(int(tc.aPrefill)), withReadyReplicas(int(aReadyPrefill)),
						withInitialReplicasAnnotation(int(tc.targetPrefill)), withCreationTimestamp(baseTime)),
					testRoleDecode: makeLWS(withName("test-0-hashA-decode"), withReplicas(int(tc.aDecode)), withReadyReplicas(int(aReadyDecode)),
						withInitialReplicasAnnotation(int(tc.targetDecode)), withCreationTimestamp(baseTime))}},
				{Revision: "hashB", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testRolePrefill: makeLWS(withName("test-0-hashB-prefill"), withReplicas(int(tc.bPrefill)), withReadyReplicas(int(tc.bPrefill)),
						withInitialReplicasAnnotation(int(tc.targetPrefill)), withCreationTimestamp(bTime)),
					testRoleDecode: makeLWS(withName("test-0-hashB-decode"), withReplicas(int(tc.bDecode)), withReadyReplicas(int(tc.bDecode)),
						withInitialReplicasAnnotation(int(tc.targetDecode)), withCreationTimestamp(bTime))}},
			}
			newRevision := disaggregatedsetutils.RevisionRoles{Revision: "hashC", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
				testRolePrefill: makeLWS(withName("test-0-hashC-prefill"), withReplicas(int(tc.cPrefill)), withReadyReplicas(int(tc.cPrefill))),
				testRoleDecode:  makeLWS(withName("test-0-hashC-decode"), withReplicas(int(tc.cDecode)), withReadyReplicas(int(tc.cDecode)))}}

			desiredReplicasByRole := resolveDesiredReplicasByRole(deployment, nil)
			_, _, err := executor.reconcileExistingRollout(context.TODO(), deployment, oldRevisions, newRevision, desiredReplicasByRole)
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
