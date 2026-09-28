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
		Client:        fakeClient,
		Scheme:        scheme,
		LWSManager:    NewLeaderWorkerSetManager(fakeClient),
		ScalerManager: NewScalerManager(fakeClient, recorder),
		Record:        recorder,
	}
}

// newTestExecutor creates a RollingUpdateExecutor with a FakeRecorder for testing.
func newTestExecutor(fakeClient client.Client) *RollingUpdateExecutor {
	return &RollingUpdateExecutor{
		LWSManager: NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}
}

func newTestClient(objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
		WithObjects(objects...).WithStatusSubresource(statusSubresourceObjects()...).Build()
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

func revisionLWS(revision, role string, replicas, ready int32, createdAt time.Time, initial ...int32) *leaderworkersetv1.LeaderWorkerSet {
	lws := buildTestLWS(fmt.Sprintf("test-0-%s-%s", revision, role), testNamespace, role, revision).
		Replica(int(replicas)).StatusReplicas(replicas).ReadyReplicas(ready).CreationTimestamp(createdAt).Obj()
	if len(initial) > 0 {
		setInitialReplicasAnnotation(lws, int(initial[0]))
	}
	return lws
}

func revisionLWSObjects(revision string, replicas, ready, initial [2]int32, createdAt time.Time) []client.Object {
	objects := make([]client.Object, 0, len(testRoleNames()))
	for i, role := range testRoleNames() {
		objects = append(objects, revisionLWS(revision, role, replicas[i], ready[i], createdAt, initial[i]))
	}
	return objects
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

func newTwoRoleTestDisaggregatedSet(
	replicas [2]int32,
	surge, unavailable [2]int,
) *disaggregatedsetv1.DisaggregatedSet {
	roles := make([]disaggregatedsetv1.DisaggregatedRoleSpec, 2)
	for i, name := range testRoleNames() {
		roles[i] = makeRoleSpec(name, replicas[i], corev1.PodSpec{},
			intstr.FromInt(surge[i]), intstr.FromInt(unavailable[i]))
	}
	return newTestDisaggregatedSet(roles...)
}

func newTestDisaggregatedSet(roles ...disaggregatedsetv1.DisaggregatedRoleSpec) *disaggregatedsetv1.DisaggregatedSet {
	return &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: testNamespace, UID: "uid"},
		Spec:       disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
	}
}

func reconcileExistingForTest(
	t *testing.T,
	executor *RollingUpdateExecutor,
	ds *disaggregatedsetv1.DisaggregatedSet,
	targetRevision string,
) (ctrl.Result, bool) {
	t.Helper()
	old, target, err := executor.LWSManager.GetRevisionRolesList(context.Background(), ds, 0, targetRevision)
	require.NoError(t, err)
	require.NotNil(t, target)
	result, complete, err := executor.reconcileExistingRollout(
		context.Background(), ds, old, *target, resolveDesiredReplicasByRole(ds, nil))
	require.NoError(t, err)
	return result, complete
}

func assertRevisionReplicas(t *testing.T, fakeClient client.Client, revision string, want [2]int32) {
	t.Helper()
	for i, role := range testRoleNames() {
		assert.EqualValues(t, want[i], getTestLWSReplicas(fakeClient, testNamespace,
			fmt.Sprintf("test-0-%s-%s", revision, role)), "%s %s", revision, role)
	}
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

	objects := []client.Object{deployment}
	createdAt := time.Now()
	if aPrefill > 0 || aDecode > 0 {
		objects = append(objects, revisionLWSObjects(revisionA,
			[2]int32{aPrefill, aDecode}, [2]int32{aPrefill, aDecode}, [2]int32{targetPrefill, targetDecode}, createdAt)...)
	}
	createdAt = createdAt.Add(time.Second)
	if bPrefill > 0 || bDecode > 0 {
		objects = append(objects, revisionLWSObjects(revisionB,
			[2]int32{bPrefill, bDecode}, [2]int32{bPrefill, bDecode}, [2]int32{targetPrefill, targetDecode}, createdAt)...)
	}

	fakeClient := newTestClient(objects...)
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
			fakeClient := newTestClient()

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
	role := makeRoleSpec(testRolePrefill, 6, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(1))
	ds := newTestDisaggregatedSet(role)
	newRevision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
	const oldRevision = "old"
	oldName := fmt.Sprintf("%s-0-%s-%s", ds.Name, oldRevision, testRolePrefill)
	createdAt := time.Now()
	oldLWS := revisionLWS(oldRevision, testRolePrefill, 4, 3, createdAt, 6)
	newLWS := revisionLWS(newRevision, testRolePrefill, 3, 3, createdAt.Add(time.Second), 6)
	fakeClient := newTestClient(oldLWS, newLWS)
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

func TestReconcileRevisionTransitionDoesNotUseTerminatingTargetReadiness(t *testing.T) {
	ctx := context.Background()
	ds := newTwoRoleTestDisaggregatedSet([2]int32{1, 1}, [2]int{1, 1}, [2]int{})
	targetRevision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
	createdAt := time.Now()
	objects := revisionLWSObjects("old", [2]int32{1, 1}, [2]int32{1, 1}, [2]int32{1, 1}, createdAt)
	terminatingTargets := revisionLWSObjects(targetRevision, [2]int32{1, 1}, [2]int32{1, 1}, [2]int32{1, 1}, createdAt.Add(time.Hour))
	for _, object := range terminatingTargets {
		lws := object.(*leaderworkersetv1.LeaderWorkerSet)
		now := metav1.Now()
		lws.DeletionTimestamp = &now
		lws.Finalizers = []string{"foregroundDeletion"}
	}
	objects = append(objects, terminatingTargets...)
	fakeClient := newTestClient(objects...)
	executor := newTestExecutor(fakeClient)

	_, complete, err := executor.ReconcileRevisionTransition(ctx, ds, 0, targetRevision, resolveDesiredReplicasByRole(ds, nil))

	require.NoError(t, err)
	assert.False(t, complete)
	assertRevisionReplicas(t, fakeClient, "old", [2]int32{1, 1})
}

func TestOrderedRevisionCandidatesPreferUnreadyThenNewest(t *testing.T) {
	createdAt := time.Now()
	oldestUnready := disaggregatedsetutils.RevisionRoles{Revision: "A", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: makeLWS(withReplicas(1), withCreationTimestamp(createdAt)),
	}}
	newestReady := disaggregatedsetutils.RevisionRoles{Revision: "B", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: makeLWS(withReplicas(1), withReadyReplicas(1), withCreationTimestamp(createdAt.Add(time.Second))),
	}}

	candidates := orderedRevisionCandidates(disaggregatedsetutils.RevisionRolesList{oldestUnready, newestReady})
	require.Len(t, candidates, 2)
	assert.Equal(t, []string{"A", "B"}, []string{candidates[0].Revision, candidates[1].Revision})

	oldestUnready.Roles[testRolePrefill].Status.ReadyReplicas = 1
	candidates = orderedRevisionCandidates(disaggregatedsetutils.RevisionRolesList{oldestUnready, newestReady})
	require.Len(t, candidates, 2)
	assert.Equal(t, []string{"B", "A"}, []string{candidates[0].Revision, candidates[1].Revision})

	// A pending drain can reserve all readiness from another drain without
	// making the revision actually unready. It must not enter the fast path
	// that discards a fully unready revision.
	oldestUnready.Roles[testRolePrefill].Status.Replicas = 2
	assert.Zero(t, committedReadyReplicas(oldestUnready.Roles[testRolePrefill]))
	candidates = orderedRevisionCandidates(disaggregatedsetutils.RevisionRolesList{oldestUnready, newestReady})
	require.Len(t, candidates, 2)
	assert.Equal(t, []string{"B", "A"}, []string{candidates[0].Revision, candidates[1].Revision})
}

func TestReconcileExistingRolloutPrefersOrdinaryProgressOverBootstrapSurge(t *testing.T) {
	createdAt := time.Now()
	objects := revisionLWSObjects("hashA", [2]int32{1, 1}, [2]int32{1, 0}, [2]int32{1, 1}, createdAt)
	objects = append(objects, revisionLWSObjects(
		"hashB", [2]int32{1, 1}, [2]int32{1, 1}, [2]int32{1, 1}, createdAt.Add(time.Hour),
	)...)
	objects = append(objects, revisionLWSObjects(
		"hashC", [2]int32{0, 0}, [2]int32{0, 0}, [2]int32{1, 1}, createdAt.Add(2*time.Hour),
	)...)
	fakeClient := newTestClient(objects...)
	executor := newTestExecutor(fakeClient)
	ds := newTwoRoleTestDisaggregatedSet([2]int32{1, 1}, [2]int{1, 1}, [2]int{})
	reconcileExistingForTest(t, executor, ds, "hashC")

	// hashB could bootstrap C, but retiring unusable hashA is ordinary progress
	// and must take precedence over exceeding a configured surge ceiling.
	assertRevisionReplicas(t, fakeClient, "hashA", [2]int32{})
	assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{1, 1})
}

func TestReconcileExistingRolloutBootstrapsThenWaitsForReadiness(t *testing.T) {
	createdAt := time.Now()
	objects := revisionLWSObjects(
		"hashB", [2]int32{1, 4}, [2]int32{1, 4}, [2]int32{1, 5}, createdAt,
	)
	objects = append(objects, revisionLWSObjects(
		"hashC", [2]int32{0, 1}, [2]int32{0, 1}, [2]int32{1, 5}, createdAt.Add(time.Hour),
	)...)
	fakeClient := newTestClient(objects...)
	recorder := events.NewFakeRecorder(10)
	executor := &RollingUpdateExecutor{LWSManager: NewLeaderWorkerSetManager(fakeClient), Record: recorder}
	ds := newTwoRoleTestDisaggregatedSet([2]int32{1, 5}, [2]int{}, [2]int{1, 1})
	result, complete := reconcileExistingForTest(t, executor, ds, "hashC")
	assert.False(t, complete)
	assert.NotZero(t, result.RequeueAfter)
	assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{1, 4})
	assertRevisionReplicas(t, fakeClient, "hashC", [2]int32{1, 1})
	require.Len(t, recorder.Events, 2)
	eventsSeen := (<-recorder.Events) + (<-recorder.Events)
	assert.Contains(t, eventsSeen, EventReasonScalingUp)
	assert.Contains(t, eventsSeen, EventReasonBootstrapSurge)
	assert.Contains(t, eventsSeen, testRolePrefill)
	assert.NotContains(t, eventsSeen, EventReasonRevisionDrainBlocked)

	// The emergency replica now exists but is not Ready. A second reconcile
	// waits instead of spending another bootstrap replica.
	result, complete = reconcileExistingForTest(t, executor, ds, "hashC")
	assert.False(t, complete)
	assert.NotZero(t, result.RequeueAfter)
	assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{1, 4})
	assertRevisionReplicas(t, fakeClient, "hashC", [2]int32{1, 1})
	require.Len(t, recorder.Events, 1)
	assert.Contains(t, <-recorder.Events, EventReasonRevisionDrainBlocked)
}

func TestReconcileExistingRolloutDrainsUnreadySpecWithoutSpendingReadyAgain(t *testing.T) {
	createdAt := time.Now()
	objects := revisionLWSObjects("hashA", [2]int32{1, 1}, [2]int32{1, 0}, [2]int32{1, 1}, createdAt)
	objects = append(objects, revisionLWSObjects(
		"hashB", [2]int32{2, 1}, [2]int32{0, 1}, [2]int32{2, 2}, createdAt.Add(time.Hour),
	)...)
	objects = append(objects, revisionLWSObjects(
		"hashC", [2]int32{1, 1}, [2]int32{1, 1}, [2]int32{3, 2}, createdAt.Add(2*time.Hour),
	)...)
	fakeClient := newTestClient(objects...)
	executor := newTestExecutor(fakeClient)
	ds := newTwoRoleTestDisaggregatedSet([2]int32{3, 2}, [2]int{1, 1}, [2]int{})
	reconcileExistingForTest(t, executor, ds, "hashC")

	assertRevisionReplicas(t, fakeClient, "hashA", [2]int32{1, 1})
	assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{})
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

func TestApplyOldTargetsUsesPlannerTargetsVerbatim(t *testing.T) {
	ctx := context.Background()
	oldPrefill := buildTestLWS("old-prefill", testNamespace, testRolePrefill, "old").
		Replica(2).StatusReplicas(2).ReadyReplicas(1).Obj()
	oldDecode := buildTestLWS("old-decode", testNamespace, testRoleDecode, "old").
		Replica(1).StatusReplicas(1).ReadyReplicas(1).Obj()
	fakeClient := newTestClient(oldPrefill, oldDecode)
	executor := newTestExecutor(fakeClient)
	ds := newTestDisaggregatedSet()
	active := disaggregatedsetutils.RevisionRoles{
		Revision: "old",
		Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			testRolePrefill: oldPrefill,
			testRoleDecode:  oldDecode,
		},
	}
	state := rolloutState(
		RoleReplicaState{2, 1}, RoleReplicaState{2, 1}, RoleReplicaState{1, 1}, nil, nil,
		RoleReplicaState{2, 1}, RoleReplicaState{2, 1}, RoleReplicaState{2, 1},
		configs([]int{1, 1}, []int{0, 0}),
	)
	step := &UpdateStep{Past: RoleReplicaState{1, 1}, New: RoleReplicaState{2, 1}}

	require.NoError(t, executor.applyOldTargets(ctx, ds, active, testRoleNames(), state, step))
	assert.EqualValues(t, 1, getTestLWSReplicas(fakeClient, testNamespace, oldPrefill.Name))
	assert.EqualValues(t, 1, getTestLWSReplicas(fakeClient, testNamespace, oldDecode.Name))
}

func TestReconcileExistingRolloutWaitsForACompleteReadyTargetRevision(t *testing.T) {
	createdAt := time.Now()
	oldPrefill := revisionLWS("oldhash", testRolePrefill, 1, 1, createdAt, 1)
	oldDecode := revisionLWS("oldhash", testRoleDecode, 1, 1, createdAt, 2)
	newPrefill := revisionLWS("newhash", testRolePrefill, 0, 0, createdAt.Add(time.Second))
	newDecode := revisionLWS("newhash", testRoleDecode, 2, 2, createdAt.Add(time.Second))
	fakeClient := newTestClient(oldPrefill, oldDecode, newPrefill, newDecode)
	executor := newTestExecutor(fakeClient)
	ds := newTwoRoleTestDisaggregatedSet([2]int32{1, 2}, [2]int{1, 1}, [2]int{})
	reconcile := func() { reconcileExistingForTest(t, executor, ds, "newhash") }

	// Decode alone does not make the target revision usable. The planner keeps
	// the complete old revision and grows the missing target Prefill.
	reconcile()
	assertRevisionReplicas(t, fakeClient, "oldhash", [2]int32{1, 1})
	assert.Equal(t, int32(1), getTestLWSReplicas(fakeClient, testNamespace, newPrefill.Name))

	require.NoError(t, fakeClient.Get(context.TODO(), client.ObjectKeyFromObject(newPrefill), newPrefill))
	newPrefill.Status.Replicas = 1
	newPrefill.Status.ReadyReplicas = 1
	require.NoError(t, fakeClient.Status().Update(context.TODO(), newPrefill))
	reconcile()
	assertRevisionReplicas(t, fakeClient, "oldhash", [2]int32{})
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
			fakeClient := newTestClient(
				buildTestLWS("test-0-newhash-prefill", namespace, testRolePrefill, "newhash").
					Replica(int(tc.initPrefill)).StatusReplicas(tc.initPrefill).ReadyReplicas(tc.initPrefill).CreationTimestamp(baseTime).Obj(),
				buildTestLWS("test-0-newhash-decode", namespace, testRoleDecode, "newhash").
					Replica(int(tc.initDecode)).StatusReplicas(tc.initDecode).ReadyReplicas(tc.initDecode).CreationTimestamp(baseTime).Obj(),
			)

			executor := newTestExecutor(fakeClient)

			ds := newTestDisaggregatedSet()

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
	ds := newTestDisaggregatedSet()
	lws := buildTestLWS("test-0-hashA-prefill", testNamespace, testRolePrefill, "hashA").Replica(5).Obj()
	fakeClient := newTestClient(lws)
	executor := newTestExecutor(fakeClient)
	recorder := events.NewFakeRecorder(10)
	executor.Record = recorder
	old := disaggregatedsetutils.RevisionRolesList{{Revision: "hashA", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: lws,
	}}}

	require.NoError(t, executor.ensureOldInitialReplicas(ctx, ds, old))
	require.Len(t, recorder.Events, 1)
	event := <-recorder.Events
	assert.Contains(t, event, corev1.EventTypeWarning)
	assert.Contains(t, event, EventReasonInitialReplicasMissing)
	assert.Contains(t, event, lws.Name)
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
	assert.Empty(t, recorder.Events, "valid initial-replicas should not emit another warning")
}

func TestOldInitialReplicasPreservesExplicitZero(t *testing.T) {
	ctx := context.Background()
	ds := newTestDisaggregatedSet()
	lws := buildTestLWS("test-0-hashA-prefill", testNamespace, testRolePrefill, "hashA").
		Replica(0).
		Annotation(map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "0"}).
		Obj()
	fakeClient := newTestClient(lws)
	recorder := events.NewFakeRecorder(10)
	executor := newTestExecutor(fakeClient)
	executor.Record = recorder
	old := disaggregatedsetutils.RevisionRolesList{{Revision: "hashA", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: lws,
	}}}

	require.NoError(t, executor.ensureOldInitialReplicas(ctx, ds, old))
	stored, err := executor.LWSManager.Get(ctx, ds, lws.Name)
	require.NoError(t, err)
	require.NotNil(t, stored)
	initial, ok := disaggregatedsetutils.GetInitialReplicas(stored)
	require.True(t, ok)
	assert.Zero(t, initial)
	assert.Empty(t, recorder.Events, "a valid zero baseline must not be repaired or warn")
}

func TestExternalTargetUpdatesCurrentRevisionInitialReplicas(t *testing.T) {
	ctx := context.Background()
	ds := newTestDisaggregatedSet(disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: testRolePrefill, Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal},
	})
	lws := buildTestLWS("test-0-hashB-prefill", testNamespace, testRolePrefill, "hashB").
		Replica(2).
		Annotation(map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "2"}).
		Obj()
	fakeClient := newTestClient(lws)
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

	stored.Status.ReadyReplicas = 1
	targets := rolloutTargetReplicas(ds, []string{testRolePrefill}, sets.New(testRolePrefill),
		disaggregatedsetutils.RevisionRolesList{{Revision: "old"}},
		disaggregatedsetutils.RevisionRoles{Revision: "hashB", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{testRolePrefill: stored}},
		map[string]int{testRolePrefill: 1})
	assert.Equal(t, RoleReplicaState{1}, targets, "a drained old object must not keep the external target high")

	old := makeLWS(withReplicas(1), withReadyReplicas(1))
	targets = rolloutTargetReplicas(ds, []string{testRolePrefill}, sets.New(testRolePrefill),
		disaggregatedsetutils.RevisionRolesList{{Revision: "old", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{testRolePrefill: old}}},
		disaggregatedsetutils.RevisionRoles{Revision: "hashB", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{testRolePrefill: stored}},
		map[string]int{testRolePrefill: 1})
	assert.Equal(t, RoleReplicaState{2}, targets, "non-zero old capacity for the same role keeps the in-flight target from shrinking")
}

func TestExternalTargetShrinksAfterRoleOldSpecReachesZero(t *testing.T) {
	ctx := context.Background()
	one, zero := intstr.FromInt(1), intstr.FromInt(0)
	role := makeRoleSpec(testRolePrefill, 10, corev1.PodSpec{}, one, zero)
	role.Scaling = &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}
	ds := newTestDisaggregatedSet(role)
	targetRevision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
	old := buildTestLWS("test-0-old-prefill", testNamespace, testRolePrefill, "old").
		Replica(0).StatusReplicas(0).ReadyReplicas(0).
		Annotation(map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "10"}).Obj()
	target := buildTestLWS(
		disaggregatedsetutils.GenerateName(ds.Name, 0, targetRevision, testRolePrefill),
		testNamespace, testRolePrefill, targetRevision,
	).Replica(10).StatusReplicas(10).ReadyReplicas(5).
		Annotation(map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "10"}).Obj()
	fakeClient := newTestClient(old, target)
	reconciler := newTestReconciler(fakeClient)
	executor := reconciler.createRollingUpdateExecutor()
	desired := map[string]int{testRolePrefill: 5}

	_, err := reconciler.reconcileSlice(ctx, executor, ds, 0, targetRevision, desired)
	require.NoError(t, err)
	_, err = reconciler.reconcileSlice(ctx, executor, ds, 0, targetRevision, desired)
	require.NoError(t, err)

	assert.EqualValues(t, 5, getTestLWSReplicas(fakeClient, testNamespace, target.Name))
}

func TestReconcileRevisionTransitionRepairsPartialTargetRevision(t *testing.T) {
	ctx := context.Background()
	ds := newTwoRoleTestDisaggregatedSet([2]int32{4, 4}, [2]int{1, 1}, [2]int{})
	targetRevision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
	createdAt := time.Now()
	objects := revisionLWSObjects("oldhash", [2]int32{4, 4}, [2]int32{4, 4}, [2]int32{4, 4}, createdAt)
	newPrefill := revisionLWS(targetRevision, testRolePrefill, 0, 0, createdAt.Add(time.Hour), 4)
	objects = append(objects, newPrefill)
	fakeClient := newTestClient(objects...)
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
	assertRevisionReplicas(t, fakeClient, "oldhash", [2]int32{4, 4})
}

func TestInterruptedRolloutKeepsInitialBaseline(t *testing.T) {
	ctx := context.Background()
	ds := newTwoRoleTestDisaggregatedSet([2]int32{6, 3}, [2]int{1, 1}, [2]int{})
	createdAt := time.Now()
	objects := []client.Object{ds}
	objects = append(objects, revisionLWSObjects(
		"hashA", [2]int32{5, 2}, [2]int32{5, 2}, [2]int32{6, 3}, createdAt,
	)...)
	objects = append(objects, revisionLWSObjects(
		"hashB", [2]int32{3, 2}, [2]int32{3, 2}, [2]int32{6, 3}, createdAt.Add(time.Hour),
	)...)
	fakeClient := newTestClient(objects...)
	executor := newTestExecutor(fakeClient)

	desiredReplicasByRole := resolveDesiredReplicasByRole(ds, nil)
	targetRevision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
	result, _, err := executor.ReconcileRevisionTransition(ctx, ds, 0, targetRevision, desiredReplicasByRole)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	old, current, err := executor.LWSManager.GetRevisionRolesList(ctx, ds, 0, targetRevision)
	require.NoError(t, err)
	require.NotNil(t, current)
	require.Len(t, old, 2)
	for _, revision := range old {
		assert.Equal(t, 6, revision.GetInitialReplicasPerRole(testRolePrefill))
		assert.Equal(t, 3, revision.GetInitialReplicasPerRole(testRoleDecode))
	}
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
	require.Len(t, old, 1)
	assert.Equal(t, 6, old[0].GetInitialReplicasPerRole(testRolePrefill), "A preserves its intended baseline despite its current Spec of 5")
	assert.Equal(t, 3, old[0].GetInitialReplicasPerRole(testRoleDecode), "A preserves its intended baseline despite its current Spec of 2")
	assert.Equal(t, 5, old.GetTotalReplicasPerRole(testRolePrefill))
	assert.Equal(t, 2, old.GetTotalReplicasPerRole(testRoleDecode))
}

func TestDrainedRevisionDoesNotInflateBaselineOrThrottleColdStart(t *testing.T) {
	ctx := context.Background()
	ds := newTwoRoleTestDisaggregatedSet([2]int32{8, 4}, [2]int{1, 1}, [2]int{})
	createdAt := time.Now()
	objects := []client.Object{ds}
	objects = append(objects,
		revisionLWSObjects("hashA", [2]int32{2, 2}, [2]int32{2, 2}, [2]int32{2, 2}, createdAt)...)
	objects = append(objects,
		revisionLWSObjects("hashB", [2]int32{0, 0}, [2]int32{0, 0}, [2]int32{10, 10}, createdAt.Add(time.Hour))...)
	objects = append(objects,
		revisionLWSObjects("hashC", [2]int32{0, 0}, [2]int32{0, 0}, [2]int32{8, 4}, createdAt.Add(2*time.Hour))...)

	fakeClient := newTestClient(objects...)
	executor := newTestExecutor(fakeClient)
	desiredReplicasByRole := resolveDesiredReplicasByRole(ds, nil)
	oldRevisions, targetRevision, err := executor.LWSManager.GetRevisionRolesList(ctx, ds, 0, "hashC")
	require.NoError(t, err)
	require.NotNil(t, targetRevision)

	candidates := orderedRevisionCandidates(oldRevisions)
	require.NotEmpty(t, candidates)
	activeRevision := candidates[0]
	assert.Equal(t, "hashA", activeRevision.Revision, "the drained hashB revision must not become the planning baseline")
	roleNames := testRoleNames()
	config := extractRollingUpdateConfig(ds, roleNames, desiredReplicasByRole)
	targets := rolloutTargetReplicas(ds, roleNames, sets.New(roleNames...), oldRevisions, *targetRevision, desiredReplicasByRole)
	state := rolloutStateForRevision(roleNames, oldRevisions, activeRevision, *targetRevision, targets, config)
	assert.Equal(t, RoleReplicaState{2, 2}, state.ActiveOld.InitialReplicas)
	assert.Equal(t, RoleReplicaState{2, 2}, state.ActiveOld.SpecReplicas)
	require.Len(t, state.ParkedOld, 1)
	assert.Equal(t, RoleReplicaState{0, 0}, state.ParkedOld[0].SpecReplicas)
	assert.Equal(t, RoleReplicaState{0, 0}, state.ParkedOld[0].ReadyReplicas)

	for _, roleName := range roleNames {
		require.NoError(t, executor.LWSManager.Scale(ctx, ds, activeRevision.Roles[roleName].Name, 0))
	}
	result, complete := reconcileExistingForTest(t, executor, ds, "hashC")
	assert.False(t, complete)
	assert.NotZero(t, result.RequeueAfter)
	assertRevisionReplicas(t, fakeClient, "hashC", [2]int32{8, 4})
}

type abcExecutorScenario struct {
	name                            string
	target, a, b, c                 [2]int32 // [prefill, decode]
	aUnready, bPrefillUnready       bool
	expectedA, expectedB, expectedC [2]int32
}

func TestReconcileExistingRolloutABCScenario(t *testing.T) {
	baseTime := time.Now()

	testCases := []abcExecutorScenario{
		{
			name: "first step scales up C using surge (no drain budget)", target: [2]int32{4, 4},
			a: [2]int32{2, 2}, b: [2]int32{2, 2}, c: [2]int32{},
			expectedA: [2]int32{2, 2}, expectedB: [2]int32{2, 2}, expectedC: [2]int32{1, 1},
		},
		{
			name: "C scales up using surge (drain deferred to next reconcile)", target: [2]int32{4, 4},
			a: [2]int32{}, b: [2]int32{2, 2}, c: [2]int32{2, 2},
			expectedA: [2]int32{0, 0}, expectedB: [2]int32{2, 2}, expectedC: [2]int32{3, 3},
		},
		{
			name: "above ceiling: drains newest old workload to recover", target: [2]int32{4, 4},
			a: [2]int32{2, 2}, b: [2]int32{2, 2}, c: [2]int32{2, 2},
			expectedA: [2]int32{2, 2}, expectedB: [2]int32{0, 0}, expectedC: [2]int32{2, 2},
		},
		{
			name: "parks A while draining asymmetric B", target: [2]int32{6, 2},
			a: [2]int32{1, 1}, b: [2]int32{2, 1}, c: [2]int32{4, 1},
			expectedA: [2]int32{1, 1}, expectedB: [2]int32{1, 1}, expectedC: [2]int32{4, 1},
		},
		{
			name: "caps C at the residual target while A is parked", target: [2]int32{6, 2},
			a: [2]int32{1, 1}, b: [2]int32{1, 1}, c: [2]int32{4, 1},
			expectedA: [2]int32{1, 1}, expectedB: [2]int32{1, 1}, expectedC: [2]int32{5, 1},
		},
		{
			name: "drains an unready revision before the newest", target: [2]int32{4, 4},
			a: [2]int32{1, 1}, b: [2]int32{1, 1}, c: [2]int32{2, 2}, aUnready: true,
			expectedA: [2]int32{0, 0}, expectedB: [2]int32{1, 1}, expectedC: [2]int32{3, 3},
		},
		{
			name: "retires mixed-ready B to release its occupied surge slot", target: [2]int32{4, 4},
			a: [2]int32{3, 3}, b: [2]int32{1, 1}, c: [2]int32{1, 1}, bPrefillUnready: true,
			expectedA: [2]int32{3, 3}, expectedB: [2]int32{0, 0}, expectedC: [2]int32{1, 1},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			readyA := tc.a
			if tc.aUnready {
				readyA = [2]int32{}
			}
			var objects []client.Object
			if tc.a != [2]int32{} {
				objects = append(objects, revisionLWSObjects("hashA", tc.a, readyA, tc.target, baseTime)...)
			}
			bReady := tc.b
			if tc.bPrefillUnready {
				bReady[0] = 0
			}
			objects = append(objects, revisionLWSObjects("hashB", tc.b, bReady, tc.target, baseTime.Add(time.Hour))...)
			objects = append(objects, revisionLWSObjects("hashC", tc.c, tc.c, tc.target, baseTime.Add(2*time.Hour))...)

			fakeClient := newTestClient(objects...)
			executor := newTestExecutor(fakeClient)
			deployment := newTwoRoleTestDisaggregatedSet(tc.target, [2]int{1, 1}, [2]int{})
			reconcileExistingForTest(t, executor, deployment, "hashC")

			if tc.a != [2]int32{} {
				assertRevisionReplicas(t, fakeClient, "hashA", tc.expectedA)
			}
			assertRevisionReplicas(t, fakeClient, "hashB", tc.expectedB)
			assertRevisionReplicas(t, fakeClient, "hashC", tc.expectedC)
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
