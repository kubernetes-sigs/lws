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
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
	scheme := wrappers.DisaggregatedSetTestScheme()
	utilruntime.Must(appsv1.AddToScheme(scheme))
	return scheme
}

func newTestReconciler(fakeClient client.Client) *DisaggregatedSetReconciler {
	scheme := testSchemeForUnit()
	recorder := events.NewFakeRecorder(100)
	return &DisaggregatedSetReconciler{
		Client:        fakeClient,
		Scheme:        scheme,
		LWSManager:    newTestLWSManager(fakeClient),
		ScalerManager: NewScalerManager(fakeClient, recorder),
		Record:        recorder,
	}
}

// newTestExecutor creates a RollingUpdateExecutor with a FakeRecorder for testing.
func newTestExecutor(fakeClient client.Client) *RollingUpdateExecutor {
	return &RollingUpdateExecutor{
		LWSManager: newTestLWSManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}
}

// Ordinary executor fixtures model settled observations. Pending-work tests
// supply explicit raw/committed counts; adapter tests use the real observer.
func newTestLWSManager(c client.Client) *LeaderWorkerSetManager {
	manager := NewLeaderWorkerSetManager(c)
	manager.observeReadiness = func(_ context.Context, lws *leaderworkersetv1.LeaderWorkerSet) (replicaReadiness, error) {
		return replicaReadiness{raw: int(lws.Status.ReadyReplicas), committed: int(min(getLWSReplicas(lws), lws.Status.ReadyReplicas))}, nil
	}
	return manager
}

func newTestClient(objects ...client.Object) client.WithWatch {
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
	lws.UID = types.UID(lws.Name)
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

func podWithSchedulingCondition(
	phase corev1.PodPhase,
	status corev1.ConditionStatus,
	reason string,
	transition metav1.Time,
) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{Phase: phase, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: status, Reason: reason, LastTransitionTime: transition,
	}}}}
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

func newOccupiedSurgeRollout() (
	client.Client, *RollingUpdateExecutor, *disaggregatedsetv1.DisaggregatedSet, *events.FakeRecorder,
) {
	createdAt := time.Now()
	initial := [2]int32{1, 4}
	objects := revisionLWSObjects("hashA", [2]int32{1, 3}, [2]int32{1, 3}, initial, createdAt)
	objects = append(objects, revisionLWSObjects(
		"hashB", [2]int32{1, 1}, [2]int32{1, 1}, initial, createdAt.Add(time.Hour))...)
	objects = append(objects, revisionLWSObjects(
		"hashC", [2]int32{0, 1}, [2]int32{0, 1}, initial, createdAt.Add(2*time.Hour))...)
	fakeClient := newTestClient(objects...)
	recorder := events.NewFakeRecorder(10)
	executor := &RollingUpdateExecutor{LWSManager: newTestLWSManager(fakeClient), Record: recorder}
	return fakeClient, executor, newTwoRoleTestDisaggregatedSet(initial, [2]int{1, 1}, [2]int{}), recorder
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "default", UID: "uid",
			Annotations: map[string]string{
				disaggregatedsetv1.RevisionHashVersionAnnotationKey: disaggregatedsetv1.RevisionHashVersion,
			},
		},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: rolesC},
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
				ObjectMeta: metav1.ObjectMeta{
					Name: tc.deployName, Namespace: "default", UID: "uid",
					Annotations: map[string]string{
						disaggregatedsetv1.RevisionHashVersionAnnotationKey: disaggregatedsetv1.RevisionHashVersion,
					},
				},
				Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
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

// Only adapter tests build native objects. Ordinary executor tests supply
// observations; native ownership and victim accounting are tested in pkg/replicagroups.
func replicaGroupObjects(lws *leaderworkersetv1.LeaderWorkerSet, actual, ready int32) []client.Object {
	meta := metav1.ObjectMeta{Name: lws.Name, Namespace: lws.Namespace, UID: types.UID(lws.Name + "-native"), Generation: 1,
		Labels:          map[string]string{leaderworkersetv1.SetNameLabelKey: lws.Name},
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkersetv1.GroupVersion.WithKind("LeaderWorkerSet"))}}
	objects := []client.Object{lws}
	var owner client.Object
	kind := "StatefulSet"
	if lws.Spec.GroupIdentity == leaderworkersetv1.GroupIdentityHash {
		deployment := &appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Replicas: lws.Spec.Replicas},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 1}}
		meta.Name, meta.UID = lws.Name+"-rs", types.UID(lws.Name+"-rs")
		meta.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))}
		owner = &appsv1.ReplicaSet{ObjectMeta: meta, Spec: appsv1.ReplicaSetSpec{Replicas: lws.Spec.Replicas},
			Status: appsv1.ReplicaSetStatus{ObservedGeneration: 1}}
		objects, kind = append(objects, deployment), "ReplicaSet"
	} else {
		owner = &appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Replicas: lws.Spec.Replicas},
			Status: appsv1.StatefulSetStatus{ObservedGeneration: 1}}
	}
	objects = append(objects, owner)
	for i := int32(0); i < actual; i++ {
		status := corev1.ConditionFalse
		if i < ready {
			status = corev1.ConditionTrue
		}
		name := fmt.Sprintf("%s-%d", lws.Name, i)
		objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: lws.Namespace, UID: types.UID(name),
			Annotations:     map[string]string{leaderworkersetv1.SizeAnnotationKey: "1"},
			Labels:          map[string]string{leaderworkersetv1.SetNameLabelKey: lws.Name, leaderworkersetv1.WorkerIndexLabelKey: "0"},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, appsv1.SchemeGroupVersion.WithKind(kind))}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}}})
	}
	return objects
}

func TestRolloutRejectsInvalidReplicaGroupObservation(t *testing.T) {
	for _, change := range []string{"missing", "replacement", "generation changed", "deleting", "read error"} {
		t.Run(change, func(t *testing.T) {
			ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(0)))
			old := revisionLWS("A", testRolePrefill, 4, 4, time.Unix(1000, 0), 4)
			target := revisionLWS("B", testRolePrefill, 1, 1, time.Unix(2000, 0), 4)
			old.Spec.LeaderWorkerTemplate.Size, target.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1), ptr.To[int32](1)
			c := newTestClient(append(replicaGroupObjects(old, 4, 4), replicaGroupObjects(target, 1, 1)...)...)
			manager := NewLeaderWorkerSetManager(c)
			failure, failRead := errors.New("read unavailable"), true
			manager.apiReader = interceptor.NewClient(c, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					lws, ok := object.(*leaderworkersetv1.LeaderWorkerSet)
					if !ok || key.Name != old.Name {
						return nil
					}
					switch change {
					case "missing":
						return apierrors.NewNotFound(leaderworkersetv1.GroupVersion.WithResource("leaderworkersets").GroupResource(), key.Name)
					case "replacement":
						lws.UID = "replacement"
					case "generation changed":
						lws.Generation++
					case "deleting":
						lws.DeletionTimestamp = ptr.To(metav1.Now())
					case "read error":
						if failRead {
							failRead = false
							return failure
						}
					}
					return nil
				},
			})
			wantError := errReplicaGroupsPending
			if change == "read error" {
				wantError = failure
			}
			_, err := manager.observeReadiness(t.Context(), old)
			require.ErrorIs(t, err, wantError)
			failRead = true // The executor must also propagate a one-shot failure.
			executor := newTestExecutor(c)
			executor.LWSManager = manager
			result, complete, err := executor.ReconcileRevisionTransition(t.Context(), ds, 0, "B", resolveDesiredReplicasByRole(ds, nil))
			if change == "read error" {
				require.ErrorIs(t, err, failure)
			} else {
				require.NoError(t, err)
				assert.Equal(t, time.Second, result.RequeueAfter)
			}
			assert.False(t, complete)
			assert.EqualValues(t, 4, getTestLWSReplicas(c, testNamespace, old.Name))
			assert.EqualValues(t, 1, getTestLWSReplicas(c, testNamespace, target.Name))
		})
	}
}

func TestRollbackUsesLiveRetainedReadiness(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, issued := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deletions-issued=%t", identity, issued), func(t *testing.T) {
				ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(0)))
				a := revisionLWS("A", testRolePrefill, 2, 4, time.Unix(1000, 0), 4)
				b := revisionLWS("B", testRolePrefill, 2, 2, time.Unix(2000, 0), 4)
				a.Spec.GroupIdentity, b.Spec.GroupIdentity = identity, identity
				a.Spec.LeaderWorkerTemplate.Size, b.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1), ptr.To[int32](1)
				c := newTestClient(a, b)
				for _, phase := range []struct {
					name                                          string
					pending, cancel, replacement, unready, finish bool
					counters, target                              int32
					raw, retained, old                            int
					complete                                      bool
				}{
					{name: "grow despite outstanding removals", raw: 4, retained: 2, old: 2, target: 3},
					{name: "growth awaits acknowledgement", pending: true, raw: 4, old: 2},
					{name: "counters alone cannot restore credit", pending: true, counters: 2, raw: 4, old: 2},
					{name: "acknowledged reversal", cancel: true, raw: 4, retained: 2, old: 2},
					{name: "replacement starts unready", replacement: true, unready: true, raw: 3, retained: 2, old: 2},
					{name: "replacement ready; other victim still terminating", replacement: true, raw: 4, retained: 3, old: 1},
					{name: "finish growth", finish: true, raw: 4, retained: 3, old: 1},
					{name: "finish draining", finish: true, raw: 4, retained: 4},
					{name: "complete", finish: true, raw: 4, retained: 4, complete: true},
				} {
					if !issued && phase.replacement {
						continue
					}
					t.Run(phase.name, func(t *testing.T) {
						require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(a), a))
						require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(b), b))
						live := a.DeepCopy()
						// These counters deliberately disagree with four live Ready Pods.
						if phase.counters != 0 {
							live.Status.Replicas, live.Status.ReadyReplicas = phase.counters, phase.counters
						}
						objects := replicaGroupObjects(live, 4, 4)
						for _, object := range objects[1:] {
							if pod, ok := object.(*corev1.Pod); ok {
								if issued && !phase.finish && (pod.Name == a.Name+"-3" || !phase.replacement && pod.Name == a.Name+"-2") {
									pod.DeletionTimestamp, pod.Finalizers = ptr.To(metav1.Now()), []string{"test/hold"}
								}
								if issued && phase.finish && pod.Name == a.Name+"-3" {
									pod.UID = types.UID(pod.Name + "-replacement")
								}
								if issued && (phase.replacement || phase.finish) && pod.Name == a.Name+"-2" {
									pod.UID = types.UID(pod.Name + "-replacement")
									if phase.unready {
										pod.Status.Conditions[0].Status = corev1.ConditionFalse
									}
								}
							} else if phase.pending {
								object.SetGeneration(2)
							}
						}
						manager := NewLeaderWorkerSetManager(c)
						manager.apiReader = newTestClient(append(objects, replicaGroupObjects(b.DeepCopy(), 2, 2)...)...)
						observed, err := manager.observeReadiness(t.Context(), a)
						require.NoError(t, err)
						if !issued && phase.cancel {
							phase.retained, phase.old = 3, 1 // Unissued removals can be cancelled.
						}
						assert.Equal(t, replicaReadiness{raw: phase.raw, committed: phase.retained}, observed)
						assert.EqualValues(t, 4, a.Status.ReadyReplicas, "observation must not mutate discovered inputs")
						executor := newTestExecutor(c)
						executor.LWSManager = manager // Real observer, reconstructed on every reconcile.
						_, complete, err := executor.ReconcileRevisionTransition(t.Context(), ds, 0, "A", resolveDesiredReplicasByRole(ds, nil))
						require.NoError(t, err)
						assert.Equal(t, phase.complete, complete)
						assert.EqualValues(t, phase.old, getTestLWSReplicas(c, testNamespace, b.Name))
						if phase.target != 0 {
							assert.EqualValues(t, phase.target, getTestLWSReplicas(c, testNamespace, a.Name))
						}
					})
				}
				// Discovery must use live Spec too, not just live readiness.
				a.Spec.Replicas = ptr.To[int32](3)
				manager := NewLeaderWorkerSetManager(c)
				manager.apiReader = newTestClient(a)
				_, target, err := manager.GetRevisionRolesList(t.Context(), ds, 0, "A")
				require.NoError(t, err)
				require.NotNil(t, target)
				assert.EqualValues(t, 3, getLWSReplicas(target.Roles[testRolePrefill]))
			})
		}
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
	oldReady := replicaReadiness{raw: 3, committed: 3}
	executor.LWSManager.observeReadiness = func(_ context.Context, lws *leaderworkersetv1.LeaderWorkerSet) (replicaReadiness, error) {
		if lws.Name == oldLWS.Name {
			return oldReady, nil
		}
		return replicaReadiness{raw: 3, committed: 3}, nil
	}

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

	// The observer still sees three Ready groups, but only two are retained.
	// Stale LWS counters must not override that observation.
	oldReady.committed = 2
	var observedOld leaderworkersetv1.LeaderWorkerSet
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: oldName}, &observedOld))
	assert.EqualValues(t, 4, observedOld.Status.Replicas)
	assert.EqualValues(t, 3, observedOld.Status.ReadyReplicas)
	reconcile()
	assert.EqualValues(t, 3, getTestLWSReplicas(fakeClient, testNamespace, oldName),
		"a pending deletion must reserve the Ready replica it may remove")

	// The observer confirms three survivors; the rollout may continue.
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: oldName}, &observedOld))
	observedOld.Status.Replicas = 3
	observedOld.Status.ReadyReplicas = 3
	require.NoError(t, fakeClient.Status().Update(ctx, &observedOld))
	oldReady.committed = 3
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
		testRolePrefill: makeLWS(withName("A"), withReplicas(1), withCreationTimestamp(createdAt)),
	}}
	newestReady := disaggregatedsetutils.RevisionRoles{Revision: "B", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: makeLWS(withName("B"), withReplicas(1), withReadyReplicas(1), withCreationTimestamp(createdAt.Add(time.Second))),
	}}

	readiness := rolloutReadiness{"B": {raw: 1, committed: 1}}
	candidates := orderedRevisionCandidates(disaggregatedsetutils.RevisionRolesList{oldestUnready, newestReady}, readiness)
	require.Len(t, candidates, 2)
	assert.Equal(t, []string{"A", "B"}, []string{candidates[0].Revision, candidates[1].Revision})

	oldestUnready.Roles[testRolePrefill].Status.ReadyReplicas = 1
	readiness["A"] = replicaReadiness{raw: 1, committed: 1}
	candidates = orderedRevisionCandidates(disaggregatedsetutils.RevisionRolesList{oldestUnready, newestReady}, readiness)
	require.Len(t, candidates, 2)
	assert.Equal(t, []string{"B", "A"}, []string{candidates[0].Revision, candidates[1].Revision})

	// A pending drain can reserve all readiness from another drain without
	// making the revision actually unready. It must not enter the fast path
	// that discards a fully unready revision.
	oldestUnready.Roles[testRolePrefill].Status.Replicas = 2
	readiness["A"] = replicaReadiness{raw: 1, committed: 0}
	candidates = orderedRevisionCandidates(disaggregatedsetutils.RevisionRolesList{oldestUnready, newestReady}, readiness)
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
	executor := &RollingUpdateExecutor{LWSManager: newTestLWSManager(fakeClient), Record: recorder}
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

func TestReconcileExistingRolloutBootstrapsPastOccupiedSurge(t *testing.T) {
	fakeClient, executor, ds, recorder := newOccupiedSurgeRollout()

	result, complete := reconcileExistingForTest(t, executor, ds, "hashC")
	assert.False(t, complete)
	assert.NotZero(t, result.RequeueAfter)
	assertRevisionReplicas(t, fakeClient, "hashA", [2]int32{1, 3})
	assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{1, 1})
	assertRevisionReplicas(t, fakeClient, "hashC", [2]int32{1, 1})
	require.Len(t, recorder.Events, 2)
	eventsSeen := (<-recorder.Events) + (<-recorder.Events)
	assert.Contains(t, eventsSeen, EventReasonBootstrapSurge)
	assert.Contains(t, eventsSeen, testRolePrefill)
}

func TestReconcileExistingRolloutReleasesCapacityForUnschedulableBootstrap(t *testing.T) {
	fakeClient, executor, ds, recorder := newOccupiedSurgeRollout()

	// No ordinary step exists, so the first reconcile creates C's bootstrap
	// Prefill. B remains complete while that Pod gets a chance to schedule.
	reconcileExistingForTest(t, executor, ds, "hashC")
	assertRevisionReplicas(t, fakeClient, "hashA", [2]int32{1, 3})
	assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{1, 1})
	assertRevisionReplicas(t, fakeClient, "hashC", [2]int32{1, 1})
	for len(recorder.Events) > 0 {
		<-recorder.Events
	}

	pod := podWithSchedulingCondition(corev1.PodPending, corev1.ConditionFalse,
		corev1.PodReasonUnschedulable, metav1.NewTime(time.Now()))
	pod.Name = "hash-c-prefill-0"
	pod.Namespace = testNamespace
	pod.Labels = map[string]string{leaderworkersetv1.SetNameLabelKey: "test-0-hashC-prefill"}
	require.NoError(t, fakeClient.Create(context.Background(), pod))

	// A recent scheduling failure is not enough evidence to reduce
	// availability. Give the scheduler a grace period first.
	reconcileExistingForTest(t, executor, ds, "hashC")
	assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{1, 1})
	for len(recorder.Events) > 0 {
		<-recorder.Events
	}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}, pod))
	pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(
		time.Now().Add(-unschedulablePodGracePeriod - time.Second),
	)
	require.NoError(t, fakeClient.Status().Update(context.Background(), pod))

	// The scheduler has confirmed that C's Prefill cannot fit. Retiring B is
	// now allowed to release B's Prefill capacity. A remains complete, and the
	// temporary availability deficit is limited to one Decode replica.
	reconcileExistingForTest(t, executor, ds, "hashC")
	assertRevisionReplicas(t, fakeClient, "hashA", [2]int32{1, 3})
	assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{})
	assertRevisionReplicas(t, fakeClient, "hashC", [2]int32{1, 1})
	eventsSeen := ""
	for len(recorder.Events) > 0 {
		eventsSeen += <-recorder.Events
	}
	assert.Contains(t, eventsSeen, EventReasonAvailabilityFallback)
	assert.Contains(t, eventsSeen, testRolePrefill)
}

func TestPodIsPersistentlyUnschedulable(t *testing.T) {
	now := time.Now()
	oldTransition := metav1.NewTime(now.Add(-unschedulablePodGracePeriod - time.Second))
	recentTransition := metav1.NewTime(now.Add(-unschedulablePodGracePeriod + time.Second))
	deleting := metav1.NewTime(now)
	deletingPod := podWithSchedulingCondition(corev1.PodPending, corev1.ConditionFalse,
		corev1.PodReasonUnschedulable, oldTransition)
	deletingPod.DeletionTimestamp = &deleting

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"old scheduler rejection", podWithSchedulingCondition(corev1.PodPending, corev1.ConditionFalse,
			corev1.PodReasonUnschedulable, oldTransition), true},
		{"recent scheduler rejection", podWithSchedulingCondition(corev1.PodPending, corev1.ConditionFalse,
			corev1.PodReasonUnschedulable, recentTransition), false},
		{"pending without scheduler rejection", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}, false},
		{"different pending reason", podWithSchedulingCondition(corev1.PodPending, corev1.ConditionFalse,
			"ImagePullBackOff", oldTransition), false},
		{"scheduled but unready", podWithSchedulingCondition(corev1.PodRunning, corev1.ConditionTrue,
			"", oldTransition), false},
		{"deleting unschedulable Pod", deletingPod, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, podIsPersistentlyUnschedulable(tc.pod, now))
		})
	}
}

func TestTargetUnschedulableRolesSkipsReadyRoles(t *testing.T) {
	now := time.Now()
	ready := revisionLWS("target", testRolePrefill, 1, 1, now)
	pending := revisionLWS("target", testRoleDecode, 1, 0, now)
	pod := podWithSchedulingCondition(corev1.PodPending, corev1.ConditionFalse,
		corev1.PodReasonUnschedulable, metav1.NewTime(now.Add(-unschedulablePodGracePeriod-time.Second)))
	pod.Name = "target-decode-0"
	pod.Namespace = testNamespace
	pod.Labels = map[string]string{leaderworkersetv1.SetNameLabelKey: pending.Name}

	podListCalls := 0
	baseClient := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).WithObjects(pod).Build()
	countingClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				podListCalls++
			}
			return c.List(ctx, list, opts...)
		},
	})
	executor := newTestExecutor(countingClient)
	target := disaggregatedsetutils.RevisionRoles{Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: ready,
		testRoleDecode:  pending,
	}}

	roles, err := executor.targetUnschedulableRoles(context.Background(), target, testRoleNames(), rolloutReadiness{
		ready.Name: {raw: 1, committed: 1},
	})
	require.NoError(t, err)
	assert.Equal(t, []bool{false, true}, roles)
	assert.Equal(t, 1, podListCalls)
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

	// B's extra unready Prefill can drain, but its Ready Decode remains needed
	// until A or C has replacement Decode readiness.
	assertRevisionReplicas(t, fakeClient, "hashA", [2]int32{1, 1})
	assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{1, 1})
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

func TestScaleRevisionDownUsesPlannerTargetsVerbatim(t *testing.T) {
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
	require.NoError(t, executor.scaleRevision(
		ctx, ds, active, testRoleNames(), RoleReplicaState{1, 1}, scaleDown,
	))
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
// Unit Tests for scaleRevision
// =============================================================================

func TestScaleRevisionUp(t *testing.T) {
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

			targetRevision := disaggregatedsetutils.RevisionRoles{
				Revision: "newhash",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testRolePrefill: makeLWS(withName("test-0-newhash-prefill"), withReplicas(tc.workloadPrefill)),
					testRoleDecode:  makeLWS(withName("test-0-newhash-decode"), withReplicas(tc.workloadDecode)),
				},
			}

			targets := RoleReplicaState{tc.targetPrefill, tc.targetDecode}
			err := executor.scaleRevision(context.TODO(), ds, targetRevision, roleNames, targets, scaleUp)
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

func TestTargetInitialReplicasFollowResolvedRolloutTarget(t *testing.T) {
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

	require.NoError(t, executor.syncTargetInitialReplicas(
		ctx, ds, []string{testRolePrefill}, current, RoleReplicaState{6},
	))
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

	require.NoError(t, executor.syncTargetInitialReplicas(
		ctx, ds, []string{testRolePrefill}, current, targets,
	))
	stored, err = executor.LWSManager.Get(ctx, ds, lws.Name)
	require.NoError(t, err)
	initial, ok = disaggregatedsetutils.GetInitialReplicas(stored)
	require.True(t, ok)
	assert.EqualValues(t, 2, initial,
		"the annotation must follow the clamped rollout target, not the raw external target of one")
}

func TestExternalScaleDownPersistsClampedRolloutTarget(t *testing.T) {
	ctx := context.Background()
	one, zero := intstr.FromInt(1), intstr.FromInt(0)
	role := makeRoleSpec(testRolePrefill, 3, corev1.PodSpec{}, one, zero)
	role.Scaling = &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}
	ds := newTestDisaggregatedSet(role)
	createdAt := time.Now()
	oldLWS := revisionLWS("hashA", testRolePrefill, 1, 1, createdAt, 1)
	targetLWS := revisionLWS("hashB", testRolePrefill, 3, 3, createdAt.Add(time.Hour), 6)
	fakeClient := newTestClient(oldLWS, targetLWS)
	executor := newTestExecutor(fakeClient)
	old := disaggregatedsetutils.RevisionRolesList{{
		Revision: "hashA",
		Roles:    map[string]*leaderworkersetv1.LeaderWorkerSet{testRolePrefill: oldLWS},
	}}
	target := disaggregatedsetutils.RevisionRoles{
		Revision: "hashB",
		Roles:    map[string]*leaderworkersetv1.LeaderWorkerSet{testRolePrefill: targetLWS},
	}

	_, _, err := executor.reconcileExistingRollout(
		ctx, ds, old, target, map[string]int{testRolePrefill: 1},
	)
	require.NoError(t, err)

	stored, err := executor.LWSManager.Get(ctx, ds, targetLWS.Name)
	require.NoError(t, err)
	initial, ok := disaggregatedsetutils.GetInitialReplicas(stored)
	require.True(t, ok)
	assert.EqualValues(t, 3, initial,
		"while old capacity remains, the current Spec—not the lower raw scaler value—is the rollout target")
}

func TestObserveOldRevisionNeverUsesBaselineBelowSpec(t *testing.T) {
	for _, annotation := range []string{"1", "0", "-1", "not-a-number"} {
		t.Run(annotation, func(t *testing.T) {
			lws := revisionLWS("hashB", testRolePrefill, 3, 3, time.Now())
			lws.Annotations = map[string]string{
				disaggregatedsetv1.InitialReplicasAnnotationKey: annotation,
			}
			revision := disaggregatedsetutils.RevisionRoles{
				Revision: "hashB",
				Roles:    map[string]*leaderworkersetv1.LeaderWorkerSet{testRolePrefill: lws},
			}

			initial, observed := observeOldRevision(revision, []string{testRolePrefill}, nil)

			assert.Equal(t, RoleReplicaState{3}, initial)
			assert.Equal(t, RoleReplicaState{3}, observed.SpecReplicas)
			assert.Equal(t, []bool{true}, observed.RequiredRoles)
		})
	}
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

func TestRolloutAvailabilityBaselineFollowsNonDrainedRevisions(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanup=%t", cleanup), func(t *testing.T) {
			ctx := context.Background()
			createdAt := time.Unix(1000, 0)
			objects := revisionLWSObjects("A", [2]int32{1, 1}, [2]int32{1, 1}, [2]int32{1, 1}, createdAt)
			objects = append(objects, revisionLWSObjects("B", [2]int32{1, 1}, [2]int32{1, 1}, [2]int32{2, 2}, createdAt.Add(time.Hour))...)
			objects = append(objects, revisionLWSObjects("C", [2]int32{1, 1}, [2]int32{1, 1}, [2]int32{2, 2}, createdAt.Add(2*time.Hour))...)
			fakeClient := newTestClient(objects...)
			ds := newTwoRoleTestDisaggregatedSet([2]int32{2, 2}, [2]int{1, 1}, [2]int{})
			desired := resolveDesiredReplicasByRole(ds, nil)
			reconcile := func() bool {
				// Reconstruct the executor each time; there is no carried baseline.
				_, complete, err := newTestExecutor(fakeClient).ReconcileRevisionTransition(ctx, ds, 0, "C", desired)
				require.NoError(t, err)
				return complete
			}

			// B's baseline is two. Retiring B leaves A+C at two Ready per role.
			require.False(t, reconcile())
			assertRevisionReplicas(t, fakeClient, "A", [2]int32{1, 1})
			assertRevisionReplicas(t, fakeClient, "B", [2]int32{})
			assertRevisionReplicas(t, fakeClient, "C", [2]int32{1, 1})
			if cleanup {
				require.NoError(t, newTestReconciler(fakeClient).cleanupDrainedLWS(ctx, ds, 0, "C", false))
			}

			// B's zero Spec, not object cleanup, starts the next phase. A's
			// baseline is one, so it may retire before C's growth becomes Ready.
			require.False(t, reconcile())
			assertRevisionReplicas(t, fakeClient, "A", [2]int32{})
			assertRevisionReplicas(t, fakeClient, "C", [2]int32{2, 2})
			for _, roleName := range testRoleNames() {
				target, err := newTestExecutor(fakeClient).LWSManager.GetForRole(ctx, ds, 0, "C", roleName)
				require.NoError(t, err)
				require.NotNil(t, target)
				assert.EqualValues(t, 1, target.Status.ReadyReplicas)
			}

			// Completion still requires the full target to become Ready.
			simulateAllReady(fakeClient)
			require.True(t, reconcile())
		})
	}
}

func TestTwoReadinessIncompleteOldRevisionsConverge(t *testing.T) {
	ctx := context.Background()
	createdAt := time.Now()
	objects := revisionLWSObjects("A", [2]int32{1, 2}, [2]int32{0, 2}, [2]int32{1, 2}, createdAt)
	objects = append(objects, revisionLWSObjects("B", [2]int32{1, 1}, [2]int32{0, 1}, [2]int32{1, 2}, createdAt.Add(time.Hour))...)
	objects = append(objects, revisionLWSObjects("C", [2]int32{}, [2]int32{}, [2]int32{1, 2}, createdAt.Add(2*time.Hour))...)
	fakeClient := newTestClient(objects...)
	ds := newTwoRoleTestDisaggregatedSet([2]int32{1, 2}, [2]int{1, 1}, [2]int{})
	desired := resolveDesiredReplicasByRole(ds, nil)

	complete := false
	for i := 0; i < 10 && !complete; i++ {
		var err error
		_, complete, err = newTestExecutor(fakeClient).ReconcileRevisionTransition(ctx, ds, 0, "C", desired)
		require.NoError(t, err)

		// Old Prefill remains broken. Target replicas become Ready and old
		// deletions settle before the next observation.
		var list leaderworkersetv1.LeaderWorkerSetList
		require.NoError(t, fakeClient.List(ctx, &list))
		for j := range list.Items {
			lws := &list.Items[j]
			lws.Status.Replicas = *lws.Spec.Replicas
			if lws.Labels[disaggregatedsetv1.RevisionLabelKey] == "C" {
				lws.Status.ReadyReplicas = *lws.Spec.Replicas
			} else {
				lws.Status.ReadyReplicas = min(lws.Status.ReadyReplicas, *lws.Spec.Replicas)
			}
			require.NoError(t, fakeClient.Status().Update(ctx, lws))
		}
	}

	require.True(t, complete, "a healthy target must not wait for broken old Prefill Pods to recover")
	assertRevisionReplicas(t, fakeClient, "C", [2]int32{1, 2})
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

	readiness, err := executor.LWSManager.observeRolloutReadiness(ctx, oldRevisions, *targetRevision)
	require.NoError(t, err)
	candidates := orderedRevisionCandidates(oldRevisions, readiness)
	require.NotEmpty(t, candidates)
	activeRevision := candidates[0]
	assert.Equal(t, "hashA", activeRevision.Revision, "the drained hashB revision must not become the planning baseline")
	roleNames := testRoleNames()
	config := extractRollingUpdateConfig(ds, roleNames, desiredReplicasByRole)
	targets := rolloutTargetReplicas(ds, roleNames, sets.New(roleNames...), oldRevisions, *targetRevision, desiredReplicasByRole)
	state := rolloutStateForRevision(roleNames, oldRevisions, activeRevision, *targetRevision, targets, config, readiness)
	assert.Equal(t, RoleReplicaState{2, 2}, state.ActiveOld.InitialReplicas)
	assert.Equal(t, RoleReplicaState{2, 2}, state.AvailabilityBaseline)
	assert.Equal(t, RoleReplicaState{2, 2}, state.ActiveOld.SpecReplicas)
	require.Len(t, state.ParkedOld, 1)
	assert.Equal(t, RoleReplicaState{0, 0}, state.ParkedOld[0].SpecReplicas)
	assert.Equal(t, RoleReplicaState{0, 0}, state.ParkedOld[0].ReadyReplicas)

	for _, roleName := range roleNames {
		require.NoError(t, executor.LWSManager.Scale(ctx, ds, activeRevision.Roles[roleName], 0))
	}
	result, complete := reconcileExistingForTest(t, executor, ds, "hashC")
	assert.False(t, complete)
	assert.NotZero(t, result.RequeueAfter)
	assertRevisionReplicas(t, fakeClient, "hashC", [2]int32{8, 4})
}

func TestRolloutStateSeparatesRawAndCommittedReadiness(t *testing.T) {
	createdAt := time.Now()
	active := disaggregatedsetutils.RevisionRoles{
		Revision: "B",
		Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			testRolePrefill: revisionLWS("B", testRolePrefill, 7, 10, createdAt, 50),
			testRoleDecode:  revisionLWS("B", testRoleDecode, 3, 5, createdAt, 25),
		},
	}
	active.Roles[testRolePrefill].Status.Replicas = 16
	active.Roles[testRoleDecode].Status.Replicas = 8

	parked := disaggregatedsetutils.RevisionRoles{
		Revision: "A",
		Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			testRolePrefill: revisionLWS("A", testRolePrefill, 37, 37, createdAt.Add(-time.Hour), 60),
			testRoleDecode:  revisionLWS("A", testRoleDecode, 18, 19, createdAt.Add(-time.Hour), 30),
		},
	}
	parked.Roles[testRolePrefill].Status.Replicas = 38
	parked.Roles[testRoleDecode].Status.Replicas = 19

	target := disaggregatedsetutils.RevisionRoles{
		Revision: "C",
		Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			testRolePrefill: revisionLWS("C", testRolePrefill, 11, 7, createdAt.Add(time.Hour), 50),
			testRoleDecode:  revisionLWS("C", testRoleDecode, 7, 5, createdAt.Add(time.Hour), 25),
		},
	}

	state := rolloutStateForRevision(
		testRoleNames(),
		disaggregatedsetutils.RevisionRolesList{parked, active},
		active,
		target,
		RoleReplicaState{50, 25},
		configs([]int{5, 5}, []int{5, 5}),
		rolloutReadiness{
			active.Roles[testRolePrefill].Name: {raw: 10, committed: 1},
			active.Roles[testRoleDecode].Name:  {raw: 5, committed: 0},
			parked.Roles[testRolePrefill].Name: {raw: 37, committed: 36},
			parked.Roles[testRoleDecode].Name:  {raw: 19, committed: 18},
			target.Roles[testRolePrefill].Name: {raw: 7, committed: 7},
			target.Roles[testRoleDecode].Name:  {raw: 5, committed: 5},
		},
	)

	assert.Equal(t, RoleReplicaState{10, 5}, state.ActiveOld.RawReadyReplicas)
	assert.Equal(t, RoleReplicaState{50, 25}, state.ActiveOld.InitialReplicas)
	assert.Equal(t, RoleReplicaState{60, 30}, state.AvailabilityBaseline, "a smaller drain candidate must retain the parked revision's baseline")
	assert.Equal(t, RoleReplicaState{1, 0}, state.ActiveOld.ReadyReplicas)
	require.Len(t, state.ParkedOld, 1)
	assert.Equal(t, RoleReplicaState{37, 19}, state.ParkedOld[0].RawReadyReplicas)
	assert.Equal(t, RoleReplicaState{36, 18}, state.ParkedOld[0].ReadyReplicas)
	assert.Equal(t, RoleReplicaState{7, 5}, state.Target.RawReadyReplicas)
	assert.Equal(t, RoleReplicaState{7, 5}, state.Target.ReadyReplicas)
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
