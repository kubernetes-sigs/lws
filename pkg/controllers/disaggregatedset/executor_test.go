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
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/managedfields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clientgoapplyconfigurations "k8s.io/client-go/applyconfigurations"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	"sigs.k8s.io/lws/pkg/replicagroups"
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

// reconcileTestTransition supplies the live revision list normally read by the controller.
func reconcileTestTransition(ctx context.Context, executor *RollingUpdateExecutor, ds *disaggregatedsetv1.DisaggregatedSet, revision string, desired map[string]int) (ctrl.Result, bool, error) {
	old, target, err := executor.LWSManager.GetRevisionRolesList(ctx, ds, 0, revision)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	return executor.ReconcileRevisionTransition(ctx, ds, 0, revision, old, target, desired)
}

// Ordinary executor fixtures model settled observations. Pending-work tests
// supply explicit raw/committed counts; adapter tests use the real observer.
func newTestLWSManager(c client.Client) *LeaderWorkerSetManager {
	manager := NewLeaderWorkerSetManager(c)
	manager.observeReadiness = func(_ context.Context, sets []*leaderworkersetv1.LeaderWorkerSet) (rolloutReadiness, error) {
		result := make(rolloutReadiness, len(sets))
		for _, lws := range sets {
			result[lws.Name] = replicaReadiness{raw: int(lws.Status.ReadyReplicas), committed: int(min(getLWSReplicas(lws), lws.Status.ReadyReplicas))}
		}
		return result, nil
	}
	return manager
}

func newTestClient(objects ...client.Object) client.WithWatch {
	// Reuse the built-in type registry instead of rebuilding it on every
	// simulated rollout tick. Keep the default converters and a fresh tracker.
	return fake.NewClientBuilder().WithScheme(testSchemeForUnit()).
		WithTypeConverters(clientgoapplyconfigurations.NewTypeConverter(clientgoscheme.Scheme), managedfields.NewDeducedTypeConverter()).
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
	return []client.Object{&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}, &disaggregatedsetv1.DisaggregatedSetRoleScaler{}}
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
	expectRequeue            time.Duration
	expectOldDeleted         bool
	expectOldRetained        bool
	expectOldScaledDown      bool
	expectNewCreated         bool
}

func TestValidateDisjointReplacementStep(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		ready, keepOld, invalidOld, invalidNew int
		errorContains                          string
	}{
		{"half-ready replacement cannot retire all old capacity", 1, 2, 0, 1, "usable readiness"},
		{"unready replacement cannot issue the full target", 0, 4, 4, 2, "outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Dimensions A, B, C: A4 is replaced by B2/C2, with one surge each.
			initial, ready := RoleReplicaState{4, 0, 0}, RoleReplicaState{0, tc.ready, tc.ready}
			state := rolloutState(initial, initial, initial, nil, nil, ready, ready,
				RoleReplicaState{0, 2, 2}, configs([]int{1, 1, 1}, []int{0, 0, 0}))
			require.NoError(t, validateUpdateStep(state, &UpdateStep{
				Past: RoleReplicaState{tc.keepOld, 0, 0}, New: RoleReplicaState{0, 1, 1},
			}))
			require.ErrorContains(t, validateUpdateStep(state, &UpdateStep{
				Past: RoleReplicaState{tc.invalidOld, 0, 0}, New: RoleReplicaState{0, tc.invalidNew, tc.invalidNew},
			}), tc.errorContains)
		})
	}
}

func TestValidateDisjointSurplusStep(t *testing.T) {
	// P has three Ready groups for desired two, while D has none. Only P's
	// true excess may shrink; W3 keeps its historical floor of four minus one.
	state := rolloutState([]int{0, 0, 4}, []int{0, 0, 3}, []int{0, 0, 3}, nil, nil,
		[]int{3, 1, 0}, []int{3, 0, 0}, []int{2, 2, 0}, configs([]int{0, 0, 0}, []int{1, 1, 1}))
	valid := &UpdateStep{Past: []int{0, 0, 3}, New: []int{2, 1, 0}, Kind: TargetRebalanceStep}
	require.Equal(t, valid, ComputeNextStep(state))
	require.NoError(t, validateUpdateStep(state, valid))
	for _, tc := range []struct {
		name string
		edit func(*UpdateStep)
	}{
		{"old drain", func(step *UpdateStep) { step.Past[2]-- }},
		{"last Ready group", func(step *UpdateStep) { step.New[0] = 0 }},
		{"below desired", func(step *UpdateStep) { step.New[0] = 1 }},
		{"target growth", func(step *UpdateStep) { step.New[1]++ }},
		{"unmarked shrink", func(step *UpdateStep) { step.Kind = OrdinaryStep }},
		{"bootstrap flag", func(step *UpdateStep) { step.Kind = BootstrapSurgeStep }},
		{"unavailable flag", func(step *UpdateStep) { step.Kind = UnavailableFallbackStep }},
		{"unknown kind", func(step *UpdateStep) { step.Kind = StepKind(255) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := &UpdateStep{Past: slicesClone(valid.Past), New: slicesClone(valid.New), Kind: TargetRebalanceStep}
			tc.edit(step)
			require.Error(t, validateUpdateStep(state, step))
		})
	}
	state.Target.ReadyReplicas[1] = 1
	require.Error(t, validateUpdateStep(state, valid), "a complete target cannot be rebalanced")
	state.Target.ReadyReplicas[1] = 0
	state.AvailabilityBaseline[0] = 2
	require.Error(t, validateUpdateStep(state, valid), "shared old roles may already rely on target readiness")
}

func TestDisjointSurplusReconcileUsesGuardedTargetOnlyWrites(t *testing.T) {
	for _, concurrentWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrentWrite=%t", concurrentWrite), func(t *testing.T) {
			ds := newTwoRoleTestDisaggregatedSet([2]int32{2, 2}, [2]int{}, [2]int{1, 1})
			ds.Annotations = map[string]string{disaggregatedsetv1.MaxUnavailableAnnotationKey: `{"worker":1}`}
			createdAt := time.Unix(1000, 0)
			old := revisionLWS("old", "worker", 3, 3, createdAt, 4)
			prefill := revisionLWS("new", testRolePrefill, 3, 3, createdAt, 2)
			decode := revisionLWS("new", testRoleDecode, 1, 0, createdAt, 2)
			base := newTestClient(ds, old, prefill, decode)
			patches, race := 0, concurrentWrite
			c := interceptor.NewClient(base, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					require.Equal(t, prefill.Name, obj.GetName(), "only the surplus target may change")
					data, err := patch.Data(obj)
					require.NoError(t, err)
					assert.Contains(t, string(data), "resourceVersion", "scale writes must retain optimistic locking")
					if race {
						race = false
						latest := &leaderworkersetv1.LeaderWorkerSet{}
						require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(prefill), latest))
						latest.Annotations["example.com/concurrent"] = "keep"
						require.NoError(t, c.Update(ctx, latest))
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			for attempt := range 4 {
				// Reconstruct the executor on every attempt; no recovery annotation
				// or in-memory hold is needed to keep the target from growing back.
				executor := newTestExecutor(c)
				oldRevisions, target, err := executor.LWSManager.GetRevisionRolesList(t.Context(), ds, 0, "new")
				require.NoError(t, err)
				require.NotNil(t, target)
				_, complete, err := executor.reconcileExistingRollout(t.Context(), ds, oldRevisions, *target,
					map[string]int{testRolePrefill: 2, testRoleDecode: 2})
				if concurrentWrite && attempt == 0 {
					require.True(t, apierrors.IsConflict(err), "the raced scale must fail without changing replicas")
					assert.EqualValues(t, 3, getTestLWSReplicas(c, testNamespace, prefill.Name))
					continue
				}
				require.NoError(t, err)
				assert.False(t, complete)
				assert.EqualValues(t, 2, getTestLWSReplicas(c, testNamespace, prefill.Name))
				assert.EqualValues(t, 1, getTestLWSReplicas(c, testNamespace, decode.Name))
				assert.EqualValues(t, 3, getTestLWSReplicas(c, testNamespace, old.Name))
			}
			wantPatches := 1
			if concurrentWrite {
				wantPatches++
			}
			assert.Equal(t, wantPatches, patches, "only the initial trim and optional failed race require writes")
			stored := &leaderworkersetv1.LeaderWorkerSet{}
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(prefill), stored))
			assert.NotContains(t, stored.Annotations, "disaggregatedset.x-k8s.io/scheduling-recovery")
			if concurrentWrite {
				assert.Equal(t, "keep", stored.Annotations["example.com/concurrent"])
			}
		})
	}
}

func TestReconcilerIntegration(t *testing.T) {
	testCases := []reconcilerTestCase{
		{
			name: "completes and cleans up old workloads", deployName: "test-complete",
			targetReplicas: 2, oldSpec: 0, oldReady: 0, newSpec: 2, newReady: 2,
			expectOldDeleted: true,
		},
		{
			name: "keeps drained old workloads until the target is ready", deployName: "test-wait-ready",
			targetReplicas: 2, oldSpec: 0, oldReady: 0, newSpec: 2, newReady: 1,
			expectRequeue: time.Second, expectOldRetained: true,
		},
		{
			name: "advances through rolling update", deployName: "test-advance",
			targetReplicas: 2, oldSpec: 2, oldReady: 2, newSpec: -1, newReady: -1,
			expectRequeue: time.Second, expectNewCreated: true,
		},
		{
			name: "no scale down until new ready", deployName: "test-maxunavail",
			targetReplicas: 4, oldSpec: 4, oldReady: 4, newSpec: 2, newReady: 0,
			maxSurge: ptr.To(2), maxUnavailable: ptr.To(0),
			expectRequeue: time.Second, expectOldScaledDown: false,
		},
		{
			name: "scales down when new ready", deployName: "test-partial",
			targetReplicas: 4, oldSpec: 4, oldReady: 4, newSpec: 4, newReady: 4,
			maxSurge: ptr.To(2), maxUnavailable: ptr.To(0),
			expectRequeue: time.Second, expectOldScaledDown: true,
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
			assert.Equal(t, tc.expectRequeue, result.RequeueAfter)
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
		status, nodeName := corev1.ConditionFalse, ""
		if i < ready {
			status, nodeName = corev1.ConditionTrue, "node"
		}
		name := fmt.Sprintf("%s-%d", lws.Name, i)
		objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: lws.Namespace, UID: types.UID(name),
			Annotations:     map[string]string{leaderworkersetv1.SizeAnnotationKey: "1"},
			Labels:          map[string]string{leaderworkersetv1.SetNameLabelKey: lws.Name, leaderworkersetv1.WorkerIndexLabelKey: "0"},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, appsv1.SchemeGroupVersion.WithKind(kind))}},
			Spec:   corev1.PodSpec{NodeName: nodeName},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}}})
	}
	return objects
}

func TestRolloutGuardsChangedInformerInputs(t *testing.T) {
	for _, change := range []string{"missing", "replacement", "generation changed", "deleting", "terminating input", "read error"} {
		t.Run(change, func(t *testing.T) {
			ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(0)))
			old := revisionLWS("A", testRolePrefill, 4, 4, time.Unix(1000, 0), 4)
			target := revisionLWS("B", testRolePrefill, 1, 1, time.Unix(2000, 0), 4)
			old.Spec.LeaderWorkerTemplate.Size, target.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1), ptr.To[int32](1)
			c := newTestClient(append(replicaGroupObjects(old, 4, 4), replicaGroupObjects(target, 1, 1)...)...)
			manager := NewLeaderWorkerSetManager(c)
			oldRevisions, targetRevision, err := manager.GetRevisionRolesList(t.Context(), ds, 0, "B")
			require.NoError(t, err)
			if change == "terminating input" {
				oldRevisions[0].Roles[testRolePrefill].DeletionTimestamp = ptr.To(metav1.Now())
			}
			failure := errors.New("read unavailable")
			manager.reader = interceptor.NewClient(c, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					// LWS are read by key; only descendant lists follow.
					assert.IsNotType(t, &leaderworkersetv1.LeaderWorkerSetList{}, list)
					if change == "read error" {
						return failure
					}
					return c.List(ctx, list, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					lws, isLWS := obj.(*leaderworkersetv1.LeaderWorkerSet)
					if key.Name != old.Name || !isLWS {
						return nil
					}
					// A change since discovery must defer the old plan.
					switch change {
					case "missing":
						return apierrors.NewNotFound(leaderworkersetv1.GroupVersion.WithResource("leaderworkersets").GroupResource(), key.Name)
					case "replacement":
						lws.UID = "replacement"
					case "generation changed":
						lws.Generation++
					case "deleting":
						lws.DeletionTimestamp = ptr.To(metav1.Now())
					}
					return nil
				},
			})
			executor := newTestExecutor(c)
			executor.LWSManager = manager
			result, complete, err := executor.ReconcileRevisionTransition(t.Context(), ds, 0, "B", oldRevisions, targetRevision, resolveDesiredReplicasByRole(ds, nil))
			switch change {
			case "read error":
				require.ErrorIs(t, err, failure)
			default:
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
						manager.reader = newTestClient(append(objects, replicaGroupObjects(b.DeepCopy(), 2, 2)...)...)
						observed, err := manager.observeReadiness(t.Context(), []*leaderworkersetv1.LeaderWorkerSet{a})
						require.NoError(t, err)
						if !issued && phase.cancel {
							phase.retained, phase.old = 3, 1 // Unissued removals can be cancelled.
						}
						assert.Equal(t, replicaReadiness{raw: phase.raw, committed: phase.retained}, observed[a.Name])
						assert.EqualValues(t, 4, a.Status.ReadyReplicas, "observation must not mutate discovered inputs")
						executor := newTestExecutor(c)
						executor.LWSManager = manager // Real observer, reconstructed on every reconcile.
						_, complete, err := reconcileTestTransition(t.Context(), executor, ds, "A", resolveDesiredReplicasByRole(ds, nil))
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
				manager.reader = newTestClient(a)
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
	executor.LWSManager.observeReadiness = func(_ context.Context, _ []*leaderworkersetv1.LeaderWorkerSet) (rolloutReadiness, error) {
		return rolloutReadiness{oldLWS.Name: oldReady, newLWS.Name: {raw: 3, committed: 3}}, nil
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

	_, complete, err := reconcileTestTransition(ctx, executor, ds, targetRevision, resolveDesiredReplicasByRole(ds, nil))

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

func TestReconcileZeroFloorRolloutWithoutSpareCapacity(t *testing.T) {
	for _, unavailable := range []intstr.IntOrString{intstr.FromInt(1), intstr.FromString("100%")} {
		t.Run(unavailable.String(), func(t *testing.T) {
			createdAt := time.Now()
			objects := revisionLWSObjects("old", [2]int32{1, 2}, [2]int32{1, 2}, [2]int32{1, 2}, createdAt)
			objects = append(objects, revisionLWSObjects("new", [2]int32{}, [2]int32{}, [2]int32{1, 2}, createdAt.Add(time.Hour))...)
			fakeClient := newTestClient(objects...)
			executor := newTestExecutor(fakeClient)
			ds := newTwoRoleTestDisaggregatedSet([2]int32{1, 2}, [2]int{}, [2]int{1, 1})
			ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = unavailable
			complete := false
			for tick := range 12 {
				_, complete = reconcileExistingForTest(t, executor, ds, "new")
				if tick == 0 {
					assertRevisionReplicas(t, fakeClient, "old", [2]int32{0, 1})
					assertRevisionReplicas(t, fakeClient, "new", [2]int32{})
				}
				var sets leaderworkersetv1.LeaderWorkerSetList
				require.NoError(t, fakeClient.List(t.Context(), &sets))
				requested := int32(0)
				for _, set := range sets.Items {
					requested += *set.Spec.Replicas
				}
				require.LessOrEqual(t, requested, int32(3), "no extra group may be requested")
				if complete {
					break
				}
				simulateAllReady(fakeClient)
			}
			require.True(t, complete)
			assertRevisionReplicas(t, fakeClient, "old", [2]int32{})
			assertRevisionReplicas(t, fakeClient, "new", [2]int32{1, 2})
		})
	}
}

func TestReconcileExistingRolloutBootstrapsThenWaitsForReadiness(t *testing.T) {
	createdAt := time.Now()
	objects := revisionLWSObjects(
		"hashB", [2]int32{2, 4}, [2]int32{1, 4}, [2]int32{2, 5}, createdAt,
	)
	objects = append(objects, revisionLWSObjects(
		"hashC", [2]int32{0, 1}, [2]int32{0, 1}, [2]int32{2, 5}, createdAt.Add(time.Hour),
	)...)
	fakeClient := newTestClient(objects...)
	recorder := events.NewFakeRecorder(10)
	executor := &RollingUpdateExecutor{LWSManager: newTestLWSManager(fakeClient), Record: recorder}
	// Both roles retain positive floors. An unready old Prefill occupies
	// the slot that would otherwise let its target start without bootstrap.
	ds := newTwoRoleTestDisaggregatedSet([2]int32{2, 5}, [2]int{}, [2]int{1, 1})
	result, complete := reconcileExistingForTest(t, executor, ds, "hashC")
	assert.False(t, complete)
	assert.Equal(t, time.Second, result.RequeueAfter)
	assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{2, 4})
	assertRevisionReplicas(t, fakeClient, "hashC", [2]int32{1, 1})
	require.Len(t, recorder.Events, 2)
	eventsSeen := (<-recorder.Events) + (<-recorder.Events)
	assert.Contains(t, eventsSeen, EventReasonScalingUp)
	assert.Contains(t, eventsSeen, EventReasonBootstrapSurge)
	assert.Contains(t, eventsSeen, testRolePrefill)
	assert.NotContains(t, eventsSeen, EventReasonRevisionDrainBlocked)

	// The emergency replica exists but is not Ready. Even immediate repeated
	// retries must wait instead of spending another bootstrap replica.
	for range 5 {
		result, complete = reconcileExistingForTest(t, executor, ds, "hashC")
		assert.False(t, complete)
		assert.Equal(t, time.Second, result.RequeueAfter)
		assertRevisionReplicas(t, fakeClient, "hashB", [2]int32{2, 4})
		assertRevisionReplicas(t, fakeClient, "hashC", [2]int32{1, 1})
		require.Len(t, recorder.Events, 1)
		assert.Contains(t, <-recorder.Events, EventReasonRevisionDrainBlocked)
	}
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
		{"defaults when nil", nil, nil, nil, nil, 0, 1, 0, 1},
		{"custom prefill only", ptr.To(3), ptr.To(1), nil, nil, 3, 1, 0, 1},
		{"custom decode only", nil, nil, ptr.To(2), ptr.To(0), 0, 1, 2, 0},
		{"partial prefill (surge only)", ptr.To(5), ptr.To(0), nil, nil, 5, 0, 0, 1},
		{"both custom", ptr.To(2), ptr.To(1), ptr.To(3), ptr.To(2), 2, 1, 3, 2},
		{"surge=0 with unavail allows zero surge", ptr.To(0), ptr.To(4), ptr.To(0), ptr.To(2), 0, 4, 0, 2},
		{"surge=0 and unavail=0 falls back to the default", ptr.To(0), ptr.To(0), ptr.To(0), ptr.To(0), 0, 1, 0, 1},
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
		{
			name:                   "percentages that round to 0 on a small role fall back to the default",
			prefillReplicas:        1,
			decodeReplicas:         4,
			prefillSurge:           "0%",
			prefillUnavail:         "25%",
			decodeSurge:            "0%",
			decodeUnavail:          "25%",
			expectedPrefillSurge:   0,
			expectedPrefillUnavail: 1,
			expectedDecodeSurge:    0,
			expectedDecodeUnavail:  1,
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

func TestRememberedMaxUnavailableRejectsInvalidHistory(t *testing.T) {
	for _, tc := range []struct {
		annotation string
		want       int
	}{
		{"", 0}, {"null", 0}, {"{", 0}, {`{"prefill":-1}`, 0},
		{`{"prefill":0}`, 0}, {`{"prefill":1}`, 1},
		{`{"prefill":2147483648}`, 0}, {`{"prefill":9223372036854775808}`, 0},
		{`{"prefill":2,"decode":-1}`, 2},
	} {
		t.Run(tc.annotation, func(t *testing.T) {
			ds := newTestDisaggregatedSet()
			ds.Annotations = map[string]string{disaggregatedsetv1.MaxUnavailableAnnotationKey: tc.annotation}
			config := extractRollingUpdateConfig(ds, testRoleNames(), nil)
			assert.Equal(t, tc.want, rememberedMaxUnavailable(ds)[testRolePrefill])
			assert.Equal(t, max(1, tc.want), config[0].MaxUnavailable, "removed roles always permit one unavailable group")
			assert.Equal(t, 1, config[1].MaxUnavailable)
		})
	}
}

func TestRemovedRoleMinimumDoesNotOverrideLivePolicy(t *testing.T) {
	ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(0)))
	ds.Annotations = map[string]string{disaggregatedsetv1.MaxUnavailableAnnotationKey: `{"prefill":3,"decode":0,"removed":4}`}
	roleNames := []string{testRolePrefill, testRoleDecode, "removed"}
	assert.Equal(t, configs([]int{1, 0, 0}, []int{0, 1, 4}),
		extractRollingUpdateConfig(ds, roleNames, resolveDesiredReplicasByRole(ds, nil)),
		"the live zero wins; removed zero becomes one and larger saved budgets survive")
	ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = nil
	assert.Equal(t, configs([]int{0, 0, 0}, []int{1, 1, 4}),
		extractRollingUpdateConfig(ds, roleNames, resolveDesiredReplicasByRole(ds, nil)),
		"a nil live policy is the default, not the remembered budget")
}

func TestReconcileRemembersBudgetBeforeRoleRemoval(t *testing.T) {
	ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(0)))
	ds.Annotations = map[string]string{
		disaggregatedsetv1.RevisionHashVersionAnnotationKey: disaggregatedsetv1.RevisionHashVersion,
		"example.com/keep": "yes",
	}
	old := revisionLWS(disaggregatedsetutils.ComputeRevision(ds.Spec.Roles), testRolePrefill, 4, 4, time.Now(), 4)
	old.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
	base := newTestClient(append(replicaGroupObjects(old, 4, 4), ds)...)
	failPersistence := false
	writeFailure := errors.New("budget write failed")
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetUID(types.UID(obj.GetName())) // The real observer requires persisted LWS UIDs.
			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isDS := obj.(*disaggregatedsetv1.DisaggregatedSet); isDS && failPersistence {
				return writeFailure
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	r := newTestReconciler(c)
	r.LWSManager = NewLeaderWorkerSetManager(c) // Use native observations, not the executor test stub.
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)}
	reconcile := func() {
		_, err := r.Reconcile(t.Context(), request)
		require.NoError(t, err)
		require.NoError(t, c.Get(t.Context(), request.NamespacedName, ds))
	}
	require.NoError(t, c.Get(t.Context(), request.NamespacedName, ds))
	for _, tc := range []struct {
		policy *intstr.IntOrString
		want   int
	}{
		{ptr.To(intstr.FromString("50%")), 2}, {nil, 1},
		{ptr.To(intstr.FromInt(1)), 1}, {ptr.To(intstr.FromString("50%")), 2},
	} {
		ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = nil
		if tc.policy != nil {
			ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{
				MaxSurge: intstr.FromInt(1), MaxUnavailable: *tc.policy,
			}
		}
		require.NoError(t, c.Update(t.Context(), ds))
		// A nil live policy must override an earlier remembered nonzero budget too.
		assert.Equal(t, tc.want, extractRollingUpdateConfig(ds, []string{testRolePrefill}, resolveDesiredReplicasByRole(ds, nil))[0].MaxUnavailable)
		reconcile()
		assert.Equal(t, tc.want, rememberedMaxUnavailable(ds)[testRolePrefill])
	}
	version := ds.ResourceVersion
	reconcile()
	assert.Equal(t, version, ds.ResourceVersion, "unchanged budgets need no write")
	assert.Equal(t, "yes", ds.Annotations["example.com/keep"])
	ds.Spec.Roles[0].Name = "new"
	ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt(0)
	require.NoError(t, c.Update(t.Context(), ds))
	failPersistence = true
	_, err := r.Reconcile(t.Context(), request)
	require.ErrorIs(t, err, writeFailure)
	var workloads leaderworkersetv1.LeaderWorkerSetList
	require.NoError(t, c.List(t.Context(), &workloads))
	require.Len(t, workloads.Items, 1, "policy-only edits and failed persistence must not create revisions")
	assert.Equal(t, old.Name, workloads.Items[0].Name)
	assert.EqualValues(t, 4, *workloads.Items[0].Spec.Replicas)
	failPersistence = false
	r = newTestReconciler(c) // A restarted controller must recover the remembered policy.
	r.LWSManager = NewLeaderWorkerSetManager(c)
	reconcile() // Save both policies and create the zero-replica target.
	reconcile() // With no Ready target, only the remembered allowance permits this drain.
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(old), old))
	assert.EqualValues(t, 2, *old.Spec.Replicas)
	reconcile()
	assert.Equal(t, 2, rememberedMaxUnavailable(ds)[testRolePrefill], "freeze 50%% of four, not the shrinking old Spec")
	assert.Zero(t, rememberedMaxUnavailable(ds)["new"])
}

func TestMaxUnavailablePrunesUsingLiveOwnedObjects(t *testing.T) {
	ds := newTestDisaggregatedSet()
	ds.Annotations = map[string]string{disaggregatedsetv1.MaxUnavailableAnnotationKey: `{"prefill":3,"foreign":2}`}
	first := revisionLWS("A", testRolePrefill, 4, 4, time.Now(), 8)
	last := revisionLWS("B", testRolePrefill, 0, 0, time.Now(), 8)
	foreign := revisionLWS("A", "foreign", 4, 4, time.Now(), 4)
	foreign.OwnerReferences[0].UID = "another-set"
	cache := newTestClient(ds) // The informer has not observed any LWS yet.
	live := newTestClient(ds, first, last, foreign)
	require.NoError(t, cache.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
	r := newTestReconciler(cache)
	r.LWSManager.reader = live
	require.NoError(t, r.syncMaxUnavailable(t.Context(), ds, nil))
	assert.NotContains(t, rememberedMaxUnavailable(ds), "foreign")
	for _, old := range []*leaderworkersetv1.LeaderWorkerSet{first, last} {
		// Revisions share one budget; even the final Spec=0 object retains it.
		assert.Equal(t, 3, rememberedMaxUnavailable(ds)[testRolePrefill])
		require.NoError(t, live.Delete(t.Context(), old))
		require.NoError(t, r.syncMaxUnavailable(t.Context(), ds, nil))
	}
	assert.NotContains(t, rememberedMaxUnavailable(ds), testRolePrefill)
}

func TestMaxUnavailableRejectsConcurrentPolicyChange(t *testing.T) {
	ds := newTwoRoleTestDisaggregatedSet([2]int32{8, 3}, [2]int{1, 1}, [2]int{2, 0})
	base := newTestClient(ds)
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			latest := &disaggregatedsetv1.DisaggregatedSet{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), latest))
			latest.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt(0)
			latest.Annotations = map[string]string{disaggregatedsetv1.MaxUnavailableAnnotationKey: `{"prefill":0,"decode":0}`}
			require.NoError(t, c.Update(ctx, latest))
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	err := newTestReconciler(c).syncMaxUnavailable(t.Context(), ds, map[string]int{testRolePrefill: 8, testRoleDecode: 3})
	require.True(t, apierrors.IsConflict(err), "stale budget persistence must conflict: %v", err)
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
	assert.Zero(t, rememberedMaxUnavailable(ds)[testRolePrefill], "the newer lower budget must survive")
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
		map[string]int{testRolePrefill: 1}, nil)
	assert.Equal(t, RoleReplicaState{1}, targets, "a drained old object must not keep the external target high")

	old := makeLWS(withReplicas(1), withReadyReplicas(1))
	targets = rolloutTargetReplicas(ds, []string{testRolePrefill}, sets.New(testRolePrefill),
		disaggregatedsetutils.RevisionRolesList{{Revision: "old", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{testRolePrefill: old}}},
		disaggregatedsetutils.RevisionRoles{Revision: "hashB", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{testRolePrefill: stored}},
		map[string]int{testRolePrefill: 1}, nil)
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

// Active reconciliation includes budget history plus three Ordinal or five Hash
// lists per slice. An already-settled workload needs no live reads.
func TestReconcileInformerObservationsAreSliceLocal(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, tc := range []struct {
			name, failure            string
			slices, roles, revisions int
			oldReplicas              int32
			budgetMissing            bool
		}{
			{name: "steady", slices: 4, roles: 2, revisions: 1},
			{name: "steady needs budget sync", slices: 4, roles: 2, revisions: 1, budgetMissing: true},
			{name: "one slice", slices: 1, roles: 2, revisions: 2, oldReplicas: 4},
			{name: "more roles and revisions", slices: 1, roles: 8, revisions: 3, oldReplicas: 4},
			{name: "four slices", slices: 4, roles: 2, revisions: 3, oldReplicas: 2},
			{name: "discovery failure", slices: 1, roles: 2, revisions: 2, oldReplicas: 4, failure: "read"},
			{name: "broken sibling", slices: 2, roles: 1, revisions: 2, oldReplicas: 4, failure: "metadata"},
		} {
			t.Run(fmt.Sprintf("%s/%s", identity, tc.name), func(t *testing.T) {
				ds := newTestDisaggregatedSet()
				ds.Spec.Slices = ptr.To(int32(tc.slices))
				ds.Annotations = map[string]string{disaggregatedsetv1.RevisionHashVersionAnnotationKey: disaggregatedsetv1.RevisionHashVersion}
				for i := range tc.roles {
					role := makeRoleSpec(fmt.Sprintf("role-%d", i), 4, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(0))
					role.Spec.GroupIdentity = identity
					ds.Spec.Roles = append(ds.Spec.Roles, role)
				}
				if !tc.budgetMissing {
					encoded, err := maxUnavailableAnnotation(ds, disaggregatedsetutils.GetRoleNames(ds), resolveDesiredReplicasByRole(ds, nil))
					require.NoError(t, err)
					ds.Annotations[disaggregatedsetv1.MaxUnavailableAnnotationKey] = encoded
				}
				target := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
				revisions := map[string]int32{target: 4}
				for i := 1; i < tc.revisions; i++ {
					revisions[target], revisions[fmt.Sprintf("old-%d", i)] = 1, tc.oldReplicas
				}
				objects := []client.Object{ds}
				for slice := range tc.slices {
					for revision, replicas := range revisions {
						for _, role := range ds.Spec.Roles {
							lws := revisionLWS(revision, role.Name, replicas, replicas, time.Now(), 4)
							lws.Name = disaggregatedsetutils.GenerateName(ds.Name, slice, revision, role.Name)
							lws.UID = types.UID(lws.Name)
							lws.Labels[disaggregatedsetv1.SliceLabelKey] = fmt.Sprint(slice)
							lws.Spec.GroupIdentity = identity
							lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
							groups := replicaGroupObjects(lws, replicas, replicas)
							if tc.failure == "metadata" && slice == 0 && revision == "old-1" {
								groups[len(groups)-1].SetAnnotations(map[string]string{leaderworkersetv1.SizeAnnotationKey: "invalid"})
							}
							objects = append(objects, groups...)
						}
					}
				}
				c := newTestClient(objects...)
				r := newTestReconciler(c)
				r.LWSManager = NewLeaderWorkerSetManager(c)
				lwsLists, failure := 0, errors.New("informer list failed")
				r.LWSManager.reader = interceptor.NewClient(c, interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if _, ok := list.(*leaderworkersetv1.LeaderWorkerSetList); ok {
							lwsLists++
							if tc.failure == "read" && lwsLists == 2 { // Fail slice discovery, after budget history.
								return failure
							}
						}
						return c.List(ctx, list, opts...)
					},
				})
				o := testPendingWrites(r.LWSManager.reader)
				r.LWSManager.reader, r.LWSManager.pending = o, o
				result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)})
				if err == nil {
					// The first pass only writes takeover receipts. This fully
					// current reader observes them before the second pass plans.
					result, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)})
				}
				switch tc.failure {
				case "read":
					require.ErrorIs(t, err, failure)
				case "metadata":
					require.ErrorContains(t, err, "slice 0")
					require.ErrorContains(t, err, "invalid group size")
					for slice, replicas := range []int{4, 3} {
						name := disaggregatedsetutils.GenerateName(ds.Name, slice, "old-1", "role-0")
						assert.EqualValues(t, replicas, getTestLWSReplicas(c, testNamespace, name), "only the healthy slice may drain")
					}
				default:
					require.NoError(t, err)
					if tc.revisions > 1 {
						assert.Equal(t, time.Second, result.RequeueAfter)
					}
				}
			})
		}
	}
}

func TestInformerReceiptPreventsBypassingHiddenRollout(t *testing.T) {
	ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(0)))
	ds.Spec.Roles[0].Scaling = &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}
	ds.Annotations = map[string]string{
		disaggregatedsetv1.RevisionHashVersionAnnotationKey: disaggregatedsetv1.RevisionHashVersion,
		disaggregatedsetv1.MaxUnavailableAnnotationKey:      `{"prefill":0}`,
	}
	targetRevision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
	old := revisionLWS("old", testRolePrefill, 4, 4, time.Unix(1000, 0), 4)
	target := revisionLWS(targetRevision, testRolePrefill, 1, 1, time.Unix(2000, 0), 4)
	old.Spec.LeaderWorkerTemplate.Size, target.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1), ptr.To[int32](1)
	old.Annotations[observationReceiptAnnotation] = "created"
	objects := append(replicaGroupObjects(old, 4, 4), replicaGroupObjects(target, 1, 1)...)
	base := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).WithObjects(append(objects, ds)...).
		WithStatusSubresource(append(statusSubresourceObjects(), &disaggregatedsetv1.DisaggregatedSetRoleScaler{})...).Build()
	cachedTarget := target.DeepCopy()
	cachedTarget.Spec.Replicas, cachedTarget.Status.Replicas, cachedTarget.Status.ReadyReplicas = ptr.To[int32](4), 4, 2
	patches, caughtUp := 0, false
	cached := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if sets, ok := list.(*leaderworkersetv1.LeaderWorkerSetList); ok && !caughtUp {
				sets.Items = []leaderworkersetv1.LeaderWorkerSet{*cachedTarget.DeepCopy()} // The cache also hides the old revision.
				return nil
			}
			return c.List(ctx, list, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	r := newTestReconciler(cached)
	r.LWSManager = NewLeaderWorkerSetManager(cached)
	_, err := r.ScalerManager.Reconcile(t.Context(), ds, func(string) (int32, error) { return 4, nil })
	require.NoError(t, err)
	o := testPendingWrites(cached)
	o.writes[client.ObjectKeyFromObject(old)] = &pendingWrite{owner: ds.UID, slice: "0", uid: old.UID, receipt: "created"}
	// Hide the create from both Get and List until the watch catches up.
	o.Reader = interceptor.NewClient(cached, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if !caughtUp && key == client.ObjectKeyFromObject(old) {
				return apierrors.NewNotFound(leaderworkersetv1.GroupVersion.WithResource("leaderworkersets").GroupResource(), key.Name)
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	r.LWSManager.reader, r.LWSManager.pending = o, o
	for _, advance := range []bool{false, true} {
		caughtUp = advance
		if caughtUp {
			cachedTarget = target.DeepCopy()
			cachedTarget.Status.ReadyReplicas += old.Status.ReadyReplicas
			cachedTarget.Status.Replicas += old.Status.Replicas
		}
		result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)})
		require.NoError(t, err)
		if caughtUp {
			// Discovery also takes over the newly visible target before planning.
			result, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)})
			require.NoError(t, err)
		}
		stored := &disaggregatedsetv1.DisaggregatedSet{}
		require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(ds), stored))
		require.Len(t, stored.Status.RoleStatuses, 1)
		assert.Equal(t, cachedTarget.Status.ReadyReplicas, stored.Status.RoleStatuses[0].ReadyReplicas, "status still follows informer updates")
		scaler := &disaggregatedsetv1.DisaggregatedSetRoleScaler{}
		require.NoError(t, base.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: ScalerName(ds.Name, testRolePrefill)}, scaler))
		assert.Equal(t, cachedTarget.Status.Replicas, scaler.Status.Replicas, "scaler status must also refresh on the no-op path")
		if !caughtUp {
			assert.Equal(t, time.Second, result.RequeueAfter, "unseen writes wait for informer events")
			assert.Zero(t, patches, "a stale no-op must not change workloads or budget history")
			assert.EqualValues(t, 4, getTestLWSReplicas(base, testNamespace, old.Name))
		} else {
			assert.EqualValues(t, 3, getTestLWSReplicas(base, testNamespace, old.Name), "fresh discovery finds the hidden old revision and uses the rollout planner")
		}
		assert.EqualValues(t, 1, getTestLWSReplicas(base, testNamespace, target.Name), "must not bypass maxSurge=1 by growing the target directly to four")
	}
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

	result, complete, err := reconcileTestTransition(ctx, executor, ds, targetRevision, map[string]int{
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
	result, _, err := reconcileTestTransition(ctx, executor, ds, targetRevision, desiredReplicasByRole)
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

func TestRolloutAvailabilityDoesNotDropWithOldHistory(t *testing.T) {
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
				_, complete, err := reconcileTestTransition(ctx, newTestExecutor(fakeClient), ds, "C", desired)
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

			// Retiring B must not lower the floor to A's smaller history.
			// Keep A+C at two Ready while requesting C's second group.
			require.False(t, reconcile())
			assertRevisionReplicas(t, fakeClient, "A", [2]int32{1, 1})
			assertRevisionReplicas(t, fakeClient, "C", [2]int32{2, 2})
			for _, roleName := range testRoleNames() {
				target, err := newTestExecutor(fakeClient).LWSManager.GetForRole(ctx, ds, 0, "C", roleName)
				require.NoError(t, err)
				require.NotNil(t, target)
				assert.EqualValues(t, 1, target.Status.ReadyReplicas)
			}

			// Once C is fully Ready, A may retire without crossing the floor.
			simulateAllReady(fakeClient)
			require.False(t, reconcile())
			assertRevisionReplicas(t, fakeClient, "A", [2]int32{})
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
		_, complete, err = reconcileTestTransition(ctx, newTestExecutor(fakeClient), ds, "C", desired)
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

	readiness, err := executor.LWSManager.collectRolloutObservations(ctx, ds, oldRevisions, *targetRevision, desiredReplicasByRole)
	require.NoError(t, err)
	candidates := orderedRevisionCandidates(oldRevisions, readiness)
	require.NotEmpty(t, candidates)
	activeRevision := candidates[0]
	assert.Equal(t, "hashA", activeRevision.Revision, "the drained hashB revision must not become the planning baseline")
	roleNames := testRoleNames()
	config := extractRollingUpdateConfig(ds, roleNames, desiredReplicasByRole)
	targets := rolloutTargetReplicas(ds, roleNames, sets.New(roleNames...), oldRevisions, *targetRevision, desiredReplicasByRole, readiness)
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

func TestDisjointAvailabilityBaselineExcludesOverlappingHistory(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		initial, current, targetReady int32
		overlapSpec                   [2]int32
		wantOld                       int
	}{
		{"smaller disjoint revision can drain", 2, 2, 2, [2]int32{0, 1}, 1},
		{"original singleton still rounds credit down", 1, 1, 3, [2]int32{0, 1}, 1},
		{"drain does not reset frozen baseline", 4, 2, 2, [2]int32{0, 1}, 2},
		{"overlap uses initial roles not surviving spec", 2, 2, 2, [2]int32{1, 0}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			createdAt := time.Unix(1000, 0)
			makeRevision := func(name string, lwss ...*leaderworkersetv1.LeaderWorkerSet) disaggregatedsetutils.RevisionRoles {
				revision := disaggregatedsetutils.RevisionRoles{Revision: name, Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{}}
				for _, lws := range lwss {
					revision.Roles[lws.Labels[disaggregatedsetv1.RoleLabelKey]] = lws
				}
				return revision
			}
			// A once had many X replicas but overlaps target C through Y. B's
			// smaller X baseline must govern its disjoint replacement instead.
			a := makeRevision("A",
				revisionLWS("A", "X", tc.overlapSpec[0], tc.overlapSpec[0], createdAt, 8),
				revisionLWS("A", "Y", tc.overlapSpec[1], tc.overlapSpec[1], createdAt, 4))
			b := makeRevision("B", revisionLWS("B", "X", tc.current, tc.current, createdAt, tc.initial))
			c := makeRevision("C",
				revisionLWS("C", "Y", tc.targetReady, tc.targetReady, createdAt, 4),
				revisionLWS("C", "Z", tc.targetReady, tc.targetReady, createdAt, 4))
			// Neither drained disjoint nor drained overlapping history can inflate
			// the baseline, even before their LWS objects have been cleaned up.
			d := makeRevision("D", revisionLWS("D", "X", 0, 0, createdAt, 20))
			e := makeRevision("E", revisionLWS("E", "Y", 0, 0, createdAt, 20))
			old := disaggregatedsetutils.RevisionRolesList{a, b, d, e}
			readiness := rolloutReadiness{}
			for _, revision := range append(slices.Clone(old), c) {
				for _, lws := range revision.Roles {
					ready := int(lws.Status.ReadyReplicas)
					readiness[lws.Name] = replicaReadiness{raw: ready, committed: ready}
				}
			}
			for range 2 {
				for _, active := range old {
					state := rolloutStateForRevision([]string{"X", "Y", "Z"}, old, active, c,
						[]int{0, 4, 4}, configs([]int{0, 1, 1}, []int{0, 0, 0}), readiness)
					assert.Equal(t, RoleReplicaState{int(tc.initial), 4, 0}, state.AvailabilityBaseline,
						"the same frozen baselines apply regardless of candidate or ordering")
					if active.Revision == "B" {
						past := state.ActiveOld.SpecReplicas
						if step := ComputeNextStep(state); step != nil {
							past = step.Past
							require.NoError(t, validateUpdateStep(state, step))
						}
						assert.Equal(t, tc.wantOld, past[0])
					}
				}
				slices.Reverse(old)
			}
		})
	}
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

func (f *subRoleFixture) revision(name string) disaggregatedsetutils.RevisionRoles {
	return disaggregatedsetutils.RevisionRoles{Revision: name, Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{"model": f.observe().LWS}}
}

func TestSubRoleProjectionAndBudgets(t *testing.T) {
	f := newSubRoleFixture(t, 0, "b", "a", "b", "a")
	s := f.observe()
	s.LWS.Spec.Replicas, s.LeaderStatefulSet.Spec.Replicas = ptr.To[int32](3), ptr.To[int32](3)
	observed := rolloutReadiness{"router": {raw: 2, committed: 1}}
	observeSubRoleReadiness(&subRoleState{snapshot: s, issued: map[string]int{"a": 1, "b": 2}, initial: map[string]int{"a": 7, "b": 8, "gone": 2}}, observed)
	old := disaggregatedsetutils.RevisionRoles{Revision: "old", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		"model": s.LWS, "router": makeLWS(withName("router"), withReplicas(3)),
	}}
	targetFixture := f.withRevision("target", "target-only")
	target := targetFixture.revision("target")
	observeSubRoleReadiness(&subRoleState{snapshot: targetFixture.observe(), issued: map[string]int{"target-only": 1}}, observed)
	for _, immediate := range []bool{false, true} {
		complete, ready := rolloutCompletionStatus(nil, target, []string{"model/target-only"}, RoleReplicaState{0}, observed, immediate)
		require.Equal(t, !immediate, complete, "Immediate requires exact child targets before completing")
		require.Equal(t, !immediate, ready)
	}
	for _, order := range [][]string{{"a", "b"}, {"b", "a"}, nil} {
		for _, budget := range []intstr.IntOrString{intstr.FromInt(2), intstr.FromString("50%")} {
			f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{Name: "router"}}
			if order != nil {
				f.ds.Spec.Roles = append(f.ds.Spec.Roles, disaggregatedsetv1.DisaggregatedRoleSpec{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: order[0]}, {Name: order[1]}}})
				f.ds.Spec.Roles[1].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: budget, MaxUnavailable: budget}
			}
			desired := map[string]int{"router": 3, "model/a": 3, "model/b": 7}
			inputs := buildRolloutInputs(f.ds, disaggregatedsetutils.RevisionRolesList{old}, target, desired, observed)
			require.ElementsMatch(t, []string{"router", "model/a", "model/b", "model/gone", "model/target-only"}, inputs.allRoleNames)
			for name, want := range map[string]replicaReadiness{
				"model/a": {spec: 1, initial: 7, raw: 2, committed: 1}, "model/b": {spec: 2, initial: 8, raw: 2, committed: 2},
				"model/gone": {initial: 2}, "router": {spec: 3, initial: 3, raw: 2, committed: 1},
			} {
				require.Equal(t, want, observed.role(old, name), name)
			}
			state := rolloutStateForRevision(inputs.allRoleNames, disaggregatedsetutils.RevisionRolesList{old}, old, target, inputs.targetReplicas, inputs.config, observed)
			for i, name := range inputs.allRoleNames {
				want, targetCount := RollingUpdateConfig{MaxSurge: 0, MaxUnavailable: 1}, 0
				if name == "router" {
					targetCount = 3
				} else if slices.Contains(order, name[len("model/"):]) {
					targetCount = desired[name]
					want.MaxSurge, _ = intstr.GetScaledValueFromIntOrPercent(&budget, targetCount, true)
					want.MaxUnavailable, _ = intstr.GetScaledValueFromIntOrPercent(&budget, targetCount, false)
				}
				require.Equal(t, want, inputs.config[i], name)
				require.Equal(t, targetCount, inputs.targetReplicas[i], name)
				require.Equal(t, observed.role(old, name).initial > 0, state.ActiveOld.RequiredRoles[i], name)
			}
		}
	}
}

func TestSubRoleBudgetOverrides(t *testing.T) {
	ds := newTestDisaggregatedSet(makeRoleSpec("model", 10, corev1.PodSpec{}, intstr.FromInt(3), intstr.FromString("50%")))
	ds.Spec.Roles[0].SubRoles = []disaggregatedsetv1.DisaggregatedSubRoleSpec{
		{Name: "a", Replicas: ptr.To[int32](3), MaxSurge: ptr.To(intstr.FromInt(0))},
		{Name: "b", Replicas: ptr.To[int32](7), MaxSurge: ptr.To(intstr.FromString("50%")), MaxUnavailable: ptr.To(intstr.FromInt(0))},
	}
	names := []string{"model/a", "model/b"}
	assert.Equal(t, []RollingUpdateConfig{{MaxUnavailable: 1}, {MaxSurge: 4}},
		extractRollingUpdateConfig(ds, names, resolveDesiredReplicasByRole(ds, nil)))
	// External scaling resolves percentages against each child's actual target,
	// not its initial spec count or the total physical LWS size.
	for i := range ds.Spec.Roles[0].SubRoles {
		ds.Spec.Roles[0].SubRoles[i].Scaling = &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}
		ds.Spec.Roles[0].SubRoles[i].Replicas = nil
	}
	scalers := map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler{
		"model/a": {Spec: disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: 5}},
		"model/b": {Spec: disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: 9}},
	}
	assert.Equal(t, []RollingUpdateConfig{{MaxUnavailable: 2}, {MaxSurge: 5}},
		extractRollingUpdateConfig(ds, names, resolveDesiredReplicasByRole(ds, scalers)))
	scalers["model/a"].Spec.Replicas = 1
	assert.Equal(t, []RollingUpdateConfig{{MaxUnavailable: 1}, {MaxSurge: 5}},
		extractRollingUpdateConfig(ds, names, resolveDesiredReplicasByRole(ds, scalers)), "fall back to the default when scaler changes make both budgets zero")
}

func TestSubRoleBudgetsSurviveRemovalAndPolicyReset(t *testing.T) {
	f := newSubRoleFixture(t, 0, "a", "a", "b", "b")
	require.NoError(t, f.manager.client.Get(t.Context(), client.ObjectKeyFromObject(f.ds), f.ds))
	f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{
		{Name: "a", Replicas: ptr.To[int32](4), MaxUnavailable: ptr.To(intstr.FromString("75%"))},
		{Name: "b", Replicas: ptr.To[int32](6), MaxUnavailable: ptr.To(intstr.FromInt(1))},
	}}}
	f.ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{
		MaxSurge: intstr.FromInt(1), MaxUnavailable: intstr.FromString("50%"),
	}
	require.NoError(t, f.manager.client.Update(t.Context(), f.ds))
	r := newTestReconciler(f.manager.client)
	require.NoError(t, r.syncMaxUnavailable(t.Context(), f.ds, resolveDesiredReplicasByRole(f.ds, nil)))
	assert.Equal(t, map[string]int{"model/a": 3, "model/b": 1}, rememberedMaxUnavailable(f.ds))

	// The old child exists only in frozen history. The current children use
	// their live policy; removing child overrides restores the parent's budget.
	setSubRoleJSON(f.lws, subRoleReplicasAnnotation, map[string]int{"b": 4})
	require.NoError(t, f.manager.client.Update(t.Context(), f.lws))
	f.ds.Spec.Roles[0].SubRoles[0].Name = "c"
	for i := range f.ds.Spec.Roles[0].SubRoles {
		f.ds.Spec.Roles[0].SubRoles[i].MaxUnavailable = nil
	}
	require.NoError(t, f.manager.client.Update(t.Context(), f.ds))
	require.NoError(t, r.syncMaxUnavailable(t.Context(), f.ds, resolveDesiredReplicasByRole(f.ds, nil)))
	assert.Equal(t, map[string]int{"model/a": 3, "model/b": 3, "model/c": 2}, rememberedMaxUnavailable(f.ds))

	// Removing the parent policy then restores the default, without losing history.
	f.ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = nil
	require.NoError(t, f.manager.client.Update(t.Context(), f.ds))
	require.NoError(t, r.syncMaxUnavailable(t.Context(), f.ds, resolveDesiredReplicasByRole(f.ds, nil)))
	assert.Equal(t, map[string]int{"model/a": 3, "model/b": 1, "model/c": 1}, rememberedMaxUnavailable(f.ds))
	config := extractRollingUpdateConfig(f.ds, []string{"model/a", "model/b", "model/c"}, resolveDesiredReplicasByRole(f.ds, nil))
	assert.Equal(t, []RollingUpdateConfig{{MaxUnavailable: 3}, {MaxUnavailable: 1}, {MaxUnavailable: 1}}, config)

	require.NoError(t, f.manager.client.Delete(t.Context(), f.lws))
	require.NoError(t, r.syncMaxUnavailable(t.Context(), f.ds, resolveDesiredReplicasByRole(f.ds, nil)))
	assert.NotContains(t, rememberedMaxUnavailable(f.ds), "model/a")
}

func TestSubRoleMixedHistoricalLayouts(t *testing.T) {
	partitioned := newSubRoleFixture(t, 0, "a", "b")
	ordinary := partitioned.withRevision("ordinary", "a", "b")
	delete(ordinary.lws.Annotations, subRoleReplicasAnnotation)
	delete(ordinary.lws.Annotations, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey)
	require.NoError(t, ordinary.manager.client.Update(t.Context(), ordinary.lws))
	pod := partitioned.observe().Groups[1].Leader
	pod.Status = podWithSchedulingCondition(corev1.PodPending, corev1.ConditionFalse, corev1.PodReasonUnschedulable, metav1.NewTime(time.Now().Add(-2*time.Minute))).Status
	require.NoError(t, partitioned.manager.client.Status().Update(t.Context(), pod))
	old := disaggregatedsetutils.RevisionRolesList{ordinary.revision("A"), partitioned.revision("B")}
	observed, err := partitioned.manager.collectRolloutObservations(t.Context(), partitioned.ds, old, disaggregatedsetutils.RevisionRoles{}, nil)
	require.NoError(t, err)
	inputs := buildRolloutInputs(partitioned.ds, old, disaggregatedsetutils.RevisionRoles{}, nil, observed)
	require.Equal(t, []string{"model", "model/a", "model/b"}, inputs.allRoleNames, "a removed parent retains both historical layouts")
	assert.Equal(t, replicaReadiness{}, observed.role(old[0], "model/a"))
	assert.Equal(t, replicaReadiness{}, observed.role(old[0], "model/b"))
	assert.Equal(t, replicaReadiness{}, observed.role(old[1], "model"))
	flags, err := newTestExecutor(partitioned.manager.client).targetUnschedulableRoles(t.Context(), old[1], []string{"model/a", "model/b"}, observed)
	require.NoError(t, err)
	require.Equal(t, []bool{false, true}, flags)
	writes := 0
	partitioned.manager.client = interceptor.NewClient(partitioned.manager.client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		writes++
		return c.Patch(ctx, obj, patch, opts...)
	}})
	for _, tc := range []struct {
		fixture           *subRoleFixture
		name, absent      string
		replicas, initial int
		writeError        error
	}{
		{ordinary, "model", "model/a", 3, 7, nil},
		{partitioned, "model/a", "model", 5, 13, errReplicaGroupsPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fixture
			executor := newTestExecutor(f.manager.client)
			before := f.observe().LWS
			require.NoError(t, executor.scaleRevision(t.Context(), f.ds, f.revision("old"), []string{tc.absent}, RoleReplicaState{99}, scaleUp))
			require.NoError(t, executor.syncTargetInitialReplicas(t.Context(), f.ds, []string{tc.absent}, f.revision("old"), RoleReplicaState{99}))
			require.Equal(t, before, f.observe().LWS, "an absent logical role must not write this physical LWS")
			writes = 0
			require.NoError(t, executor.scaleRevision(t.Context(), f.ds, f.revision("old"), inputs.allRoleNames, RoleReplicaState{3, 2, 3}, scaleUp))
			require.Equal(t, 1, writes, "one physical write for the whole role vector")
			require.ErrorIs(t, executor.syncTargetInitialReplicas(t.Context(), f.ds, inputs.allRoleNames, f.revision("old"), RoleReplicaState{7, 9, 4}), tc.writeError)
			s := f.observe()
			require.EqualValues(t, tc.replicas, getLWSReplicas(s.LWS))
			require.Equal(t, fmt.Sprint(tc.initial), s.LWS.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])
			if f == partitioned {
				require.JSONEq(t, `{"a":2,"b":3}`, s.LWS.Annotations[subRoleReplicasAnnotation])
				require.JSONEq(t, `{"a":9,"b":4}`, s.LWS.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey])
			} else {
				require.Empty(t, s.LWS.Annotations[subRoleReplicasAnnotation])
				require.Empty(t, s.LWS.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey])
			}
		})
	}
}

func TestSubRoleMigrationFromOrdinaryToOrdinal(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityHash, leaderworkersetv1.GroupIdentityOrdinal} {
		t.Run(string(identity), func(t *testing.T) {
			f := newSubRoleFixture(t, 0, "a", "b")
			f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "b"}}}}
			f.ds.Spec.Roles[0].Spec.GroupIdentity = leaderworkersetv1.GroupIdentityOrdinal
			// The floors below assume a surge-first policy.
			f.ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(1), MaxUnavailable: intstr.FromInt(0)}
			ordinary := f.lws.DeepCopy()
			ordinary.Name, ordinary.UID, ordinary.ResourceVersion = "old-ordinary", "old-ordinary", ""
			ordinary.Labels[disaggregatedsetv1.RevisionLabelKey] = "old"
			ordinary.Spec.GroupIdentity, ordinary.Spec.LeaderWorkerTemplate.Size = identity, ptr.To[int32](1)
			ordinary.Annotations = map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "2"}
			// Build the identity's real native ownership chain.
			for _, object := range replicaGroupObjects(ordinary, 2, 2) {
				require.NoError(t, f.manager.client.Create(t.Context(), object))
			}
			before, err := replicagroups.Observe(t.Context(), f.manager.reader, ordinary)
			require.NoError(t, err)
			for _, group := range f.observe().Groups {
				group.Leader.Status.Conditions = nil
				require.NoError(t, f.manager.client.Status().Update(t.Context(), group.Leader))
			}
			oldRevision := func() disaggregatedsetutils.RevisionRoles {
				require.NoError(t, f.manager.client.Get(t.Context(), client.ObjectKeyFromObject(ordinary), ordinary))
				return disaggregatedsetutils.RevisionRoles{Revision: "old", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{"model": ordinary.DeepCopy()}}
			}
			old, target := oldRevision(), f.revision("target")
			desired := resolveDesiredReplicasByRole(f.ds, nil)
			observed, err := f.manager.collectRolloutObservations(t.Context(), f.ds, disaggregatedsetutils.RevisionRolesList{old}, target, desired)
			old = oldRevision()
			assert.Equal(t, before.LWS.Annotations, ordinary.Annotations, "the old ordinary revision must never acquire child metadata")
			require.NoError(t, err)
			require.Equal(t, replicaReadiness{spec: 2, initial: 2, raw: 2, committed: 2}, observed.role(old, "model"))
			for _, child := range []string{"model/a", "model/b"} {
				require.Equal(t, replicaReadiness{}, observed.role(old, child))
				require.Equal(t, replicaReadiness{spec: 1, initial: 1}, observed.role(target, child))
			}
			inputs := buildRolloutInputs(f.ds, disaggregatedsetutils.RevisionRolesList{old}, target, desired, observed)
			state := rolloutStateForRevision(inputs.allRoleNames, disaggregatedsetutils.RevisionRolesList{old}, old, target, inputs.targetReplicas, inputs.config, observed)
			require.Equal(t, []string{"model/a", "model/b", "model"}, inputs.allRoleNames)
			require.Equal(t, RoleReplicaState{0, 0, 2}, state.AvailabilityBaseline)
			// Each child protects its desired count without inheriting ordinary history;
			// disjoint replacement separately preserves the removed model's floor.
			snapshot := snapshotForRolloutState(state)
			require.Equal(t, 1, snapshot.availabilityFloor(0))
			require.Equal(t, 1, snapshot.availabilityFloor(1))
			require.Equal(t, 1, snapshot.availabilityFloor(2), "removed ordinary roles can release one group")
			executor := newTestExecutor(f.manager.client)
			executor.LWSManager = f.manager
			for range 2 {
				result, complete, err := executor.reconcileExistingRollout(t.Context(), f.ds, disaggregatedsetutils.RevisionRolesList{oldRevision()}, f.revision("target"), desired)
				require.NoError(t, err)
				require.False(t, complete, "an unready target must not complete the rollout")
				require.Positive(t, result.RequeueAfter)
			}
			require.EqualValues(t, 1, getLWSReplicas(oldRevision().Roles["model"]), "the old pool keeps its availability floor until the Ordinal children are Ready")
			for _, group := range f.observe().Groups {
				group.Leader.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				require.NoError(t, f.manager.client.Status().Update(t.Context(), group.Leader))
			}
			complete := false
			for range 2 {
				_, complete, err = executor.reconcileExistingRollout(t.Context(), f.ds, disaggregatedsetutils.RevisionRolesList{oldRevision()}, f.revision("target"), desired)
				require.NoError(t, err)
			}
			require.True(t, complete)
			require.Zero(t, getLWSReplicas(oldRevision().Roles["model"]))
			after, err := replicagroups.Observe(t.Context(), f.manager.reader, ordinary)
			require.NoError(t, err)
			require.Equal(t, before.LWS.Annotations, after.LWS.Annotations)
			require.Equal(t, before.Groups, after.Groups, "old ordinary Pods must not be relabeled")
			require.JSONEq(t, `{"a":1,"b":1}`, f.observe().LWS.Annotations[subRoleReplicasAnnotation])
		})
	}
}

func TestSubRoleMembershipChangesOnlyCurrentRevision(t *testing.T) {
	for _, children := range [][]string{nil, {"b", "c"}} {
		t.Run(fmt.Sprint(children), func(t *testing.T) {
			old := newSubRoleFixture(t, 0, "a", "b")
			target := old.withRevision("target", "a", "b")
			before := old.observe()
			setChildren := func(names []string) {
				old.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{Name: "model"}}
				for _, name := range names {
					old.ds.Spec.Roles[0].SubRoles = append(old.ds.Spec.Roles[0].SubRoles, disaggregatedsetv1.DisaggregatedSubRoleSpec{Name: name})
				}
			}
			collect := func() rolloutReadiness {
				for range 8 {
					observed, err := old.manager.collectRolloutObservations(t.Context(), old.ds,
						disaggregatedsetutils.RevisionRolesList{old.revision("old")}, target.revision("current"), resolveDesiredReplicasByRole(old.ds, nil))
					require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
					if err == nil {
						return observed
					}
				}
				t.Fatal("membership did not converge")
				return nil
			}
			setChildren(children)
			observed := collect()
			for _, child := range []string{"a", "b"} {
				require.Equal(t, replicaReadiness{spec: 1, initial: 1, raw: 1, committed: 1}, observed.role(old.revision("old"), "model/"+child))
			}
			if len(children) == 0 {
				s := target.observe()
				require.Empty(t, s.LWS.Annotations[subRoleReplicasAnnotation])
				for _, group := range s.Groups {
					child, coherent := groupSubRole(group)
					require.True(t, coherent)
					require.Empty(t, child, "only the current revision loses child routing")
				}
				setChildren([]string{"restored"})
				collect()
				require.JSONEq(t, `{"restored":2}`, target.observe().LWS.Annotations[subRoleReplicasAnnotation])
				for _, group := range target.observe().Groups {
					child, coherent := groupSubRole(group)
					require.True(t, coherent)
					require.Equal(t, "restored", child)
				}
			} else {
				require.JSONEq(t, `{"a":1,"b":1,"c":0}`, target.observe().LWS.Annotations[subRoleReplicasAnnotation])
			}
			after := old.observe()
			require.Equal(t, before.LWS.Annotations, after.LWS.Annotations, "old issued membership and frozen baseline stay unchanged")
			require.Equal(t, before.Groups, after.Groups, "current layout changes must not relabel old Pods")
			require.EqualValues(t, 2, getLWSReplicas(target.observe().LWS), "membership changes preserve current physical capacity")
		})
	}
}

func TestSubRoleScalingKeepsRevisionMembership(t *testing.T) {
	old := newSubRoleFixture(t, 0, "a", "b")
	target := old.withRevision("target", "c", "d")
	executor := &RollingUpdateExecutor{LWSManager: old.manager}
	names := []string{"model/c", "model/d", "model/a", "model/b"}
	require.NoError(t, executor.scaleRevision(t.Context(), old.ds, old.revision("old"), names, RoleReplicaState{0, 0, 0, 0}, scaleDown))
	old.sync()
	old.native()
	old.sync()
	s := old.observe()
	require.JSONEq(t, `{"a":0,"b":0}`, s.LWS.Annotations[subRoleReplicasAnnotation], "draining old counts must not enroll target-only child names")
	require.JSONEq(t, `{"a":1,"b":1}`, s.LWS.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey])
	require.NoError(t, executor.scaleRevision(t.Context(), old.ds, target.revision("current"), names, RoleReplicaState{2, 1, 0, 0}, scaleUp))
	require.JSONEq(t, `{"c":2,"d":1}`, target.observe().LWS.Annotations[subRoleReplicasAnnotation], "growing the target must not enroll old-only child names")
}

func TestSubRoleScalingWaitsUntilRequestedTargetsAreAccepted(t *testing.T) {
	for _, repair := range []string{"baseline", "labels", "stale parent"} {
		t.Run(repair, func(t *testing.T) {
			f := newSubRoleFixture(t, 0, "a", "b")
			s := f.observe()
			switch repair {
			case "baseline":
				delete(f.lws.Annotations, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey)
				require.NoError(t, f.manager.client.Update(t.Context(), f.lws))
			case "labels":
				worker := s.Groups[0].Pods[1]
				delete(worker.Labels, disaggregatedsetv1.SubRoleLabelKey)
				require.NoError(t, f.manager.client.Update(t.Context(), worker))
			case "stale parent":
				live := f.lws.DeepCopy()
				live.Annotations["concurrent-edit"] = "true"
				require.NoError(t, f.manager.client.Update(t.Context(), live))
			}
			executor := newTestExecutor(f.manager.client)
			revision := disaggregatedsetutils.RevisionRoles{Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{"model": f.lws}}
			err := executor.scaleRevision(t.Context(), f.ds, revision, []string{"model/a", "model/b"}, RoleReplicaState{2, 1}, scaleUp)
			require.ErrorIs(t, err, errReplicaGroupsPending)
			require.EqualValues(t, 2, getLWSReplicas(f.observe().LWS), "repairing metadata or labels is not an accepted scale-up")
			require.Empty(t, executor.Record.(*events.FakeRecorder).Events)
		})
	}
}

func TestSubRoleBaselineRepairAndLiveMembership(t *testing.T) {
	for _, parent := range []string{"bootstrap", "", "999"} {
		t.Run(parent, func(t *testing.T) {
			children := []string{"a", "b"}
			if parent == "bootstrap" {
				children = nil
			}
			f := newSubRoleFixture(t, 0, children...)
			initial := map[string]int{"a": 2, "b": 3}
			f.lws.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey] = parent
			if parent == "bootstrap" {
				f.lws.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey] = "5"
			} else {
				setSubRoleJSON(f.lws, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey, initial)
			}
			require.NoError(t, f.manager.client.Update(t.Context(), f.lws))
			executor := &RollingUpdateExecutor{LWSManager: f.manager}
			require.NoError(t, executor.ensureOldInitialReplicas(t.Context(), f.ds, disaggregatedsetutils.RevisionRolesList{f.revision("old")}))
			_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{membership: map[string]int{"a": 2, "b": 3, "new": 0}})
			require.ErrorIs(t, err, errReplicaGroupsPending)
			s := f.observe()
			require.Equal(t, "5", s.LWS.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])
			stored, err := subRoleCounts(s.LWS, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey)
			require.NoError(t, err)
			require.Zero(t, stored["new"])
			delete(stored, "new")
			require.Equal(t, initial, stored, "new membership must not rewrite frozen history")
			issued, err := subRoleCounts(s.LWS, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Zero(t, issued["new"])
			require.Equal(t, len(children), countSubRoles(issued))
			observed, err := f.manager.collectRolloutObservations(t.Context(), f.ds, disaggregatedsetutils.RevisionRolesList{f.revision("old")}, disaggregatedsetutils.RevisionRoles{}, nil)
			require.NoError(t, err)
			require.Equal(t, 3, observed.role(f.revision("old"), "model/b").initial, "removing the parent must preserve child history")
			if parent == "bootstrap" {
				return
			}
			leader := s.Groups[0].Leader
			leader.Finalizers = []string{"test/hold"}
			require.NoError(t, f.manager.client.Update(t.Context(), leader))
			if parent == "" {
				require.NoError(t, f.manager.client.Delete(t.Context(), leader))
			} else {
				leader.Status.Phase = corev1.PodFailed
				require.NoError(t, f.manager.client.Status().Update(t.Context(), leader))
			}
			status, _, err := f.manager.subRoleStatus(t.Context(), &disaggregatedsetv1.DisaggregatedRoleSpec{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "b"}}}, []*leaderworkersetv1.LeaderWorkerSet{s.LWS}, s.LWS.Name)
			require.NoError(t, err)
			require.Equal(t, []int32{1, 0, 1}, []int32{status.Replicas, status.SubRoleStatuses[0].Replicas, status.SubRoleStatuses[1].Replicas}, "failed and terminating leaders must not inflate status replicas")
			for _, membership := range []map[string]int{{}, {"re-enabled": 3}} {
				_, err = f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{membership: membership})
				require.ErrorIs(t, err, errReplicaGroupsPending)
			}
			require.Empty(t, f.observe().LWS.Annotations[subRoleReplicasAnnotation], "finish accepted cleanup before enabling subroles again")
			require.EqualValues(t, len(children), getLWSReplicas(f.observe().LWS), "unpartitioning retains physical replicas")
			for _, group := range f.observe().Groups {
				child, coherent := groupSubRole(group)
				require.True(t, coherent)
				require.Empty(t, child, "unpartitioning clears every group")
			}
			require.Equal(t, []string{"test/hold"}, f.observe().Groups[0].Leader.Finalizers)
		})
	}
}

func TestSubRoleExternalBaselineFreezesAfterInterruption(t *testing.T) {
	a := newSubRoleFixture(t, 0, "pool", "pool")
	b := a.withRevision("target", "pool", "pool", "pool", "pool", "pool", "pool")
	b.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "pool", Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}}}}}
	b.ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(1), MaxUnavailable: intstr.FromInt(0)}
	for _, fixture := range []*subRoleFixture{a, b} {
		_, err := fixture.manager.syncSubRoles(t.Context(), fixture.ds, fixture.observe().LWS, subRoleUpdate{initial: map[string]int{"pool": 8}})
		require.ErrorIs(t, err, errReplicaGroupsPending)
	}
	executor := newTestExecutor(b.manager.client)
	// A8 -> B8 reached A2/B6. A surge-first policy clamps desired 2 to issued 6.
	inputs, err := executor.prepareRolloutInputs(t.Context(), b.ds, disaggregatedsetutils.RevisionRolesList{a.revision("A")}, b.revision("B"), map[string]int{"model/pool": 2})
	require.ErrorIs(t, err, errReplicaGroupsPending)
	inputs, err = executor.prepareRolloutInputs(t.Context(), b.ds, disaggregatedsetutils.RevisionRolesList{a.revision("A")}, b.revision("B"), map[string]int{"model/pool": 2})
	require.NoError(t, err)
	baseline := 6
	require.Equal(t, RoleReplicaState{baseline}, inputs.targetReplicas)
	s := b.observe()
	initial, err := subRoleCounts(s.LWS, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"pool": baseline}, initial)
	total, ok := disaggregatedsetutils.GetInitialReplicas(s.LWS)
	require.True(t, ok)
	require.EqualValues(t, baseline, total)
	_, err = b.manager.syncSubRoles(t.Context(), b.ds, s.LWS, subRoleUpdate{target: map[string]int{"pool": 5}})
	require.ErrorIs(t, err, errReplicaGroupsPending)
	b.sync()
	b.native()
	b.sync()
	c := b.withRevision("next", "pool")
	old, target := b.revision("B"), c.revision("C")
	inputs, err = executor.prepareRolloutInputs(t.Context(), b.ds, disaggregatedsetutils.RevisionRolesList{old}, target, map[string]int{"model/pool": 6})
	require.ErrorIs(t, err, errReplicaGroupsPending)
	target = c.revision("C")
	inputs, err = executor.prepareRolloutInputs(t.Context(), b.ds, disaggregatedsetutils.RevisionRolesList{old}, target, map[string]int{"model/pool": 6})
	require.NoError(t, err)
	require.Equal(t, 6, inputs.readiness.role(old, "model/pool").initial, "B5 must retain its frozen baseline 6")
	state := rolloutStateForRevision(inputs.allRoleNames, disaggregatedsetutils.RevisionRolesList{old}, old, target, inputs.targetReplicas, inputs.config, inputs.readiness)
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	require.NoError(t, validateUpdateStep(state, step))
	require.Equal(t, RoleReplicaState{5}, step.Past, "grow C, do not spend a second B against a moving floor")
	require.Equal(t, RoleReplicaState{2}, step.New)
}

func TestSubRolePendingBarrierResumesEveryAcceptedAssignment(t *testing.T) {
	blocked := newSubRoleFixture(t, 0, "a", "a", "b")
	blocked.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{Name: "model"}}
	healthy, target := blocked.withRevision("healthy", "a", "b"), blocked.withRevision("target", "a")
	for _, f := range []*subRoleFixture{blocked, healthy} {
		_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{target: map[string]int{"a": 1, "b": 0}})
		require.ErrorIs(t, err, errReplicaGroupsPending)
	}
	leader := blocked.observe().Groups[0].Leader
	leader.Status.Conditions = nil
	require.NoError(t, blocked.manager.client.Status().Update(t.Context(), leader))
	executor := newTestExecutor(blocked.manager.client)
	for range 3 {
		result, complete, err := executor.reconcileExistingRollout(t.Context(), blocked.ds,
			disaggregatedsetutils.RevisionRolesList{blocked.revision("blocked"), healthy.revision("healthy")}, target.revision("target"), map[string]int{"model/a": 2, "model/b": 3})
		require.NoError(t, err)
		require.False(t, complete)
		require.Positive(t, result.RequeueAfter)
		require.NotEmpty(t, blocked.observe().LWS.Annotations[subRolePlanAnnotation])
		require.Empty(t, healthy.observe().LWS.Annotations[subRolePlanAnnotation])
		require.EqualValues(t, 1, getLWSReplicas(healthy.observe().LWS))
		require.JSONEq(t, `{"a":1,"b":0}`, healthy.observe().LWS.Annotations[subRoleReplicasAnnotation], "accepted old assignments finish under an ordinary current config")
		require.EqualValues(t, 1, getLWSReplicas(target.observe().LWS), "pending assignment blocks fresh planner decisions")
	}
}

type subRoleFixture struct {
	t            *testing.T
	manager      *LeaderWorkerSetManager
	nativeClient client.Client // Backing fake for native-controller writes, not DS observations.
	ds           *disaggregatedsetv1.DisaggregatedSet
	lws          *leaderworkersetv1.LeaderWorkerSet
	start        int
	nextHash     int
	identity     leaderworkersetv1.GroupIdentityType
}

func newSubRoleFixture(t *testing.T, start int, children ...string) *subRoleFixture {
	return newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityOrdinal, start, children...)
}

func newSubRoleFixtureForIdentity(t *testing.T, identity leaderworkersetv1.GroupIdentityType, start int, children ...string) *subRoleFixture {
	f := &subRoleFixture{t: t, start: start, identity: identity, ds: newDSWithRoles("ds")}
	f.manager = NewLeaderWorkerSetManager(newTestClient(f.ds))
	f.nativeClient = f.manager.client
	return f.withRevision("model", children...)
}

func (f *subRoleFixture) metadata(name string, owner client.Object, kind schema.GroupVersionKind) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: f.ds.Namespace, UID: types.UID(name + "-" + kind.Kind), Generation: 1,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, kind)}, Labels: map[string]string{
			leaderworkersetv1.SetNameLabelKey: f.lws.Name, disaggregatedsetv1.SetNameLabelKey: f.ds.Name, disaggregatedsetv1.RoleLabelKey: "model", disaggregatedsetv1.SliceLabelKey: "0", disaggregatedsetv1.RevisionLabelKey: f.lws.Name}}
}

func (f *subRoleFixture) withRevision(name string, children ...string) *subRoleFixture {
	f = &subRoleFixture{t: f.t, manager: f.manager, nativeClient: f.nativeClient, ds: f.ds, start: f.start, nextHash: f.start + len(children), identity: f.identity, lws: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: name}}}
	f.lws.ObjectMeta = f.metadata(name, f.ds, disaggregatedsetv1.GroupVersion.WithKind("DisaggregatedSet"))
	f.lws.Spec = leaderworkersetv1.LeaderWorkerSetSpec{GroupIdentity: f.identity, Replicas: ptr.To(int32(len(children))), LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{Size: ptr.To[int32](3)}}
	f.lws.Status.ObservedGeneration = 1
	counts := map[string]int{}
	for _, child := range children {
		counts[child]++
	}
	setSubRoleJSON(f.lws, subRoleReplicasAnnotation, counts)
	setSubRoleJSON(f.lws, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey, counts)
	setInitialReplicasAnnotation(f.lws, len(children))
	var owner client.Object = &appsv1.StatefulSet{ObjectMeta: f.metadata(name, f.lws, leaderworkersetv1.GroupVersion.WithKind("LeaderWorkerSet")), Spec: appsv1.StatefulSetSpec{Replicas: f.lws.Spec.Replicas, Ordinals: &appsv1.StatefulSetOrdinals{Start: int32(f.start)}}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: int32(len(children))}}
	require.NoError(f.t, f.manager.client.Create(f.t.Context(), f.lws))
	if f.identity == leaderworkersetv1.GroupIdentityHash {
		deployment := &appsv1.Deployment{ObjectMeta: f.metadata(name, f.lws, leaderworkersetv1.GroupVersion.WithKind("LeaderWorkerSet")), Spec: appsv1.DeploymentSpec{Replicas: f.lws.Spec.Replicas}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1}}
		require.NoError(f.t, f.manager.client.Create(f.t.Context(), deployment))
		owner = &appsv1.ReplicaSet{ObjectMeta: f.metadata(name+"-rs", deployment, appsv1.SchemeGroupVersion.WithKind("Deployment")), Spec: appsv1.ReplicaSetSpec{Replicas: f.lws.Spec.Replicas}, Status: appsv1.ReplicaSetStatus{ObservedGeneration: 1, Replicas: int32(len(children))}}
	}
	require.NoError(f.t, f.manager.client.Create(f.t.Context(), owner))
	for i, child := range children {
		f.createGroup(owner, f.start+i, child)
	}
	return f
}

// Initial construction and native growth use the same two-worker group shape.
func (f *subRoleFixture) createGroup(owner client.Object, ordinal int, child string) {
	ready := corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	if child == "" {
		ready.Conditions = nil // Native growth has not acquired application readiness yet.
	}
	kind := appsv1.SchemeGroupVersion.WithKind("StatefulSet")
	if _, hash := owner.(*appsv1.ReplicaSet); hash {
		kind = appsv1.SchemeGroupVersion.WithKind("ReplicaSet")
	}
	leader := &corev1.Pod{ObjectMeta: f.metadata(fmt.Sprintf("%s-%d", f.lws.Name, ordinal), owner, kind), Status: ready}
	leader.Annotations = map[string]string{leaderworkersetv1.SizeAnnotationKey: "3", "user": "keep"}
	workers := &appsv1.StatefulSet{ObjectMeta: f.metadata(leader.Name, leader, corev1.SchemeGroupVersion.WithKind("Pod")), Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](2), Ordinals: &appsv1.StatefulSetOrdinals{Start: 1}}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, AvailableReplicas: 2, CurrentRevision: "a", UpdateRevision: "a"}}
	require.NoError(f.t, f.manager.client.Create(f.t.Context(), workers))
	for i := range 3 {
		pod := leader
		if i != 0 {
			pod = &corev1.Pod{ObjectMeta: f.metadata(fmt.Sprintf("%s-%d", leader.Name, i), workers, appsv1.SchemeGroupVersion.WithKind("StatefulSet")), Status: ready}
		}
		pod.Labels[leaderworkersetv1.WorkerIndexLabelKey] = fmt.Sprint(i)
		if child != "" {
			pod.Labels[disaggregatedsetv1.SubRoleLabelKey] = child
			pod.Spec.NodeName = "node" // Ready groups have already been scheduled.
		}
		require.NoError(f.t, f.manager.client.Create(f.t.Context(), pod))
	}
}

func (f *subRoleFixture) observe() *replicagroups.Snapshot {
	f.t.Helper()
	s, err := replicagroups.Observe(f.t.Context(), f.manager.reader, f.lws)
	require.NoError(f.t, err)
	require.NotNil(f.t, s)
	f.lws = s.LWS
	return s
}

func (f *subRoleFixture) sync() {
	f.t.Helper()
	_, err := f.manager.syncSubRoles(f.t.Context(), f.ds, f.observe().LWS, subRoleUpdate{})
	require.True(f.t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
}

func (f *subRoleFixture) scale(target map[string]int) error {
	_, err := f.manager.syncSubRoles(f.t.Context(), f.ds, f.observe().LWS, subRoleUpdate{target: target})
	return err
}

func (f *subRoleFixture) native(wrongVictims ...types.UID) {
	s := f.observe()
	n := int(getLWSReplicas(s.LWS))
	if f.identity == leaderworkersetv1.GroupIdentityHash {
		// Simulate native writes through the backing fake, independently of any
		// DS write spy. Tests can deliberately override the deletion-cost choice.
		c := f.nativeClient
		s.LeaderDeployment.Spec.Replicas = ptr.To(int32(n))
		require.NoError(f.t, c.Update(f.t.Context(), s.LeaderDeployment))
		s.LeaderDeployment.Status.ObservedGeneration = s.LeaderDeployment.Generation
		require.NoError(f.t, c.Status().Update(f.t.Context(), s.LeaderDeployment))
		require.Len(f.t, s.ReplicaSets, 1, "the fixture simulates one active ReplicaSet")
		rs := s.ReplicaSets[0]
		rs.Spec.Replicas = ptr.To(int32(n))
		require.NoError(f.t, c.Update(f.t.Context(), rs))
		groups := activeSubRoleGroups(s)
		slices.SortStableFunc(groups, func(a, b replicagroups.Group) int {
			if wrong := cmp.Compare(slices.Index(wrongVictims, b.Leader.UID), slices.Index(wrongVictims, a.Leader.UID)); wrong != 0 {
				return wrong
			}
			aCost, _ := strconv.Atoi(a.Leader.Annotations[corev1.PodDeletionCost])
			bCost, _ := strconv.Atoi(b.Leader.Annotations[corev1.PodDeletionCost])
			return cmp.Compare(aCost, bCost)
		})
		for _, group := range groups[:max(0, len(groups)-n)] {
			for _, pod := range group.Pods {
				require.NoError(f.t, client.IgnoreNotFound(c.Delete(f.t.Context(), pod)))
			}
			if group.WorkerStatefulSet != nil {
				require.NoError(f.t, c.Delete(f.t.Context(), group.WorkerStatefulSet))
			}
		}
		for count := len(groups); count < n; count++ {
			f.createGroup(rs, f.nextHash, "")
			f.nextHash++
		}
		rs.Status.ObservedGeneration, rs.Status.Replicas = rs.Generation, int32(n)
		require.NoError(f.t, c.Status().Update(f.t.Context(), rs))
		return
	}
	s.LeaderStatefulSet.Spec.Replicas = ptr.To(int32(n))
	require.NoError(f.t, f.manager.client.Update(f.t.Context(), s.LeaderStatefulSet))
	for _, group := range s.Groups {
		if group.Ordinal >= f.start+n {
			require.NoError(f.t, f.manager.client.Delete(f.t.Context(), group.Leader))
		}
	}
	for ordinal := f.start; ordinal < f.start+n; ordinal++ {
		if !slices.ContainsFunc(s.Groups, func(g replicagroups.Group) bool { return g.Ordinal == ordinal }) {
			f.createGroup(s.LeaderStatefulSet, ordinal, "")
		}
	}
	s = f.observe()
	s.LeaderStatefulSet.Status.Replicas = int32(len(activeSubRoleGroups(s)))
	require.NoError(f.t, f.manager.client.Status().Update(f.t.Context(), s.LeaderStatefulSet))
}

func assertHashSubRoleCounts(t *testing.T, f *subRoleFixture, want map[string]int) {
	t.Helper()
	s := f.observe()
	require.Empty(t, s.LWS.Annotations[subRolePlanAnnotation])
	require.EqualValues(t, countSubRoles(want), getLWSReplicas(s.LWS))
	actual := maps.Clone(want)
	for name := range actual {
		actual[name] = 0
	}
	for _, group := range s.Groups {
		name, coherent := groupSubRole(group)
		require.True(t, coherent)
		actual[name]++
		require.NotContains(t, group.Leader.Annotations, subRoleOriginalDeletionCostAnnotation, "completed survivors must lose the DS cost override")
	}
	require.Equal(t, want, actual)
}

// Exercise the real API observation and reconciler for every initial health mask.
func TestSubRoleTransitions(t *testing.T) {
	for _, initial := range [][]string{{"a", "a", "b"}, {"a", "a"}, {"a", "b"}} {
		for _, target := range []map[string]int{{"a": 1, "b": 1}, {"a": 1, "b": 2}, {"a": 2, "b": 1}, {"a": 0, "b": 0}} {
			for health := 0; health < 1<<len(initial); health++ {
				t.Run(fmt.Sprint(initial, target, health), func(t *testing.T) {
					f := newSubRoleFixture(t, 7, initial...)
					issued, _ := subRoleCounts(f.lws, subRoleReplicasAnnotation)
					decrease := target["a"] < issued["a"] || target["b"] < issued["b"]
					_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{target: target})
					require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
					for i, group := range f.observe().Groups {
						if health&(1<<i) == 0 {
							group.Leader.Status.Conditions = nil
							require.NoError(t, f.manager.client.Status().Update(t.Context(), group.Leader))
						}
					}
					for range 6 {
						f.native()
						f.sync()
					}
					s := f.observe()
					mask := (1 << min(len(initial), countSubRoles(target))) - 1
					complete := !decrease || health&mask == mask
					counts, err := subRoleCounts(s.LWS, subRoleReplicasAnnotation)
					require.NoError(t, err)
					if !complete {
						require.Equal(t, issued, counts)
						require.NotEmpty(t, s.LWS.Annotations[subRolePlanAnnotation])
						return
					}
					require.Equal(t, target, counts)
					require.Empty(t, s.LWS.Annotations[subRolePlanAnnotation])
					actual := map[string]int{"a": 0, "b": 0}
					for _, group := range f.observe().Groups {
						child, coherent := groupSubRole(group)
						require.True(t, coherent)
						actual[child]++
					}
					require.Equal(t, target, actual)
				})
			}
		}
	}
}

func TestSubRoleLabelsBatchBeforePlanning(t *testing.T) {
	for _, tc := range []struct {
		identity leaderworkersetv1.GroupIdentityType
		newborn  bool
	}{{leaderworkersetv1.GroupIdentityOrdinal, true}, {leaderworkersetv1.GroupIdentityOrdinal, false}, {leaderworkersetv1.GroupIdentityHash, true}, {leaderworkersetv1.GroupIdentityHash, false}} {
		identity, newborn := tc.identity, tc.newborn
		t.Run(fmt.Sprintf("%s/newborn=%t", identity, newborn), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, identity, 0, "a", "a", "a", "a")
			for _, group := range f.observe().Groups {
				for _, pod := range group.Pods {
					if newborn {
						delete(pod.Labels, disaggregatedsetv1.SubRoleLabelKey)
					} else if pod != group.Leader {
						pod.Labels[disaggregatedsetv1.SubRoleLabelKey] = "b"
					}
					require.NoError(t, f.manager.client.Update(t.Context(), pod))
				}
			}
			lwsReads, podLists := 0, 0
			f.manager.reader = interceptor.NewClient(f.manager.client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*leaderworkersetv1.LeaderWorkerSet); ok {
						lwsReads++
					}
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.PodList); ok {
						podLists++
					}
					return c.List(ctx, list, opts...)
				},
			})
			_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.lws, subRoleUpdate{})
			require.ErrorIs(t, err, errReplicaGroupsPending, "observe all label writes before planning")
			require.Equal(t, 1, podLists)
			require.Equal(t, 2+2*4, lwsReads, "two observation checks plus at most two checks per group")
			for _, group := range f.observe().Groups {
				name, coherent := groupSubRole(group)
				require.True(t, coherent)
				require.Equal(t, "a", name, "every group must be labeled in the same pass")
			}
		})
	}

	f := newSubRoleFixture(t, 0, "a", "a", "b")
	_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.lws, subRoleUpdate{target: map[string]int{"a": 1, "b": 1}})
	require.ErrorIs(t, err, errReplicaGroupsPending)
	for _, group := range f.observe().Groups {
		pod := group.Pods[1]
		pod.Labels[disaggregatedsetv1.SubRoleLabelKey] = "incomplete"
		require.NoError(t, f.manager.client.Update(t.Context(), pod))
	}
	f.sync()
	require.EqualValues(t, 3, getLWSReplicas(f.observe().LWS), "repairing every label must not publish the smaller physical target in the same pass")
	f.sync()
	require.EqualValues(t, 2, getLWSReplicas(f.observe().LWS))
}

func TestSubRoleLabelsCheckIntentBeforeCompletingGroup(t *testing.T) {
	for _, tc := range []struct {
		identity leaderworkersetv1.GroupIdentityType
		repair   bool
	}{{leaderworkersetv1.GroupIdentityOrdinal, false}, {leaderworkersetv1.GroupIdentityOrdinal, true}, {leaderworkersetv1.GroupIdentityHash, false}, {leaderworkersetv1.GroupIdentityHash, true}} {
		identity, repair := tc.identity, tc.repair
		t.Run(fmt.Sprintf("%s/repair=%t", identity, repair), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, identity, 0, "a")
			name := "b"
			if repair {
				name = "a"
				for _, pod := range f.observe().Groups[0].Pods[1:] {
					pod.Labels[disaggregatedsetv1.SubRoleLabelKey] = "b"
					require.NoError(t, f.manager.client.Update(t.Context(), pod))
				}
			}
			s := f.observe()
			changed := false
			f.manager.client = interceptor.NewClient(f.manager.client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				err := c.Patch(ctx, obj, patch, opts...)
				if !changed && err == nil {
					changed = true
					lws := s.LWS.DeepCopy()
					lws.Annotations[subRolePlanAnnotation] = `{"a":1}`
					require.NoError(t, c.Update(ctx, lws))
				}
				return err
			}})
			require.ErrorIs(t, f.manager.labelSubRoleGroup(t.Context(), s.LWS, s.Groups[0], name), errReplicaGroupsPending)
			after := f.observe()
			child, coherent := groupSubRole(after.Groups[0])
			require.Equal(t, "a", child, "an obsolete intent must not commit the leader")
			require.False(t, coherent, "repaired workers must also recheck intent before making the group coherent")
			// Retry from the fresh parent observation after the partial write.
			require.NoError(t, f.manager.labelSubRoleGroup(t.Context(), after.LWS, after.Groups[0], name))
			child, coherent = groupSubRole(f.observe().Groups[0])
			require.Equal(t, name, child)
			require.True(t, coherent)
		})
	}
}

func TestSubRoleInterruptedWrites(t *testing.T) {
	for _, failure := range []string{"model-1-1", "model-1-2", "model-1", "model-2-2", "publish", "RV", "UID"} {
		t.Run(failure, func(t *testing.T) {
			f := newSubRoleFixture(t, 0, "a", "a", "b")
			original, base := f.observe().LWS, f.manager.client
			_, err := f.manager.syncSubRoles(t.Context(), f.ds, original, subRoleUpdate{target: map[string]int{"a": 1, "b": 1}})
			require.ErrorIs(t, err, errReplicaGroupsPending)
			pending := f.observe().LWS.Annotations[subRolePlanAnnotation]
			failed := false
			f.manager.client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
				lws, physical := object.(*leaderworkersetv1.LeaderWorkerSet)
				if !failed && (object.GetName() == failure || failure == "publish" && physical && getLWSReplicas(lws) == 2 || (failure == "RV" || failure == "UID") && object.GetName() == "model-1-2") {
					failed = true
					if failure != "RV" && failure != "UID" {
						return errors.New("interrupted")
					}
					pod := &corev1.Pod{}
					require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(object), pod))
					pod.Labels["user"] = "concurrent"
					if failure == "UID" {
						require.NoError(t, c.Delete(ctx, pod))
						pod.UID, pod.ResourceVersion = "replacement", ""
						require.NoError(t, c.Create(ctx, pod))
					} else {
						require.NoError(t, c.Update(ctx, pod))
					}
				}
				return c.Patch(ctx, object, patch, opts...)
			}})
			for attempt := 0; attempt < 4 && !failed; attempt++ {
				_, err = f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{})
			}
			require.True(t, failed)
			require.Error(t, err, "interrupted or stale writes must be rejected")
			s := f.observe()
			require.EqualValues(t, 3, getLWSReplicas(s.LWS))
			require.Equal(t, pending, s.LWS.Annotations[subRolePlanAnnotation])
			if failure != "publish" && failure != "model-2-2" {
				require.Equal(t, "a", s.Groups[1].Leader.Labels[disaggregatedsetv1.SubRoleLabelKey], "workers commit before their leader")
			}
			f.manager = NewLeaderWorkerSetManager(base) // Recovery has only persisted state.
			if failure == "model-1-2" || failure == "model-2-2" {
				initial := s.LWS.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey]
				s.LWS.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey] = "{"
				require.NoError(t, base.Update(t.Context(), s.LWS))
				s.Groups[0].Leader.Status.Conditions = nil
				require.NoError(t, base.Status().Update(t.Context(), s.Groups[0].Leader))
				f.sync() // Repair the partial group before health gating or baseline parsing.
				s = f.observe()
				_, coherent := groupSubRole(s.Groups[1])
				require.True(t, coherent)
				if failure == "model-2-2" {
					child, coherent := groupSubRole(s.Groups[2])
					require.True(t, coherent)
					require.Empty(t, child)
				}
				for _, target := range []map[string]int{{"b": 4}, {"b": -4}} {
					_, err = f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{target: target})
					require.ErrorIs(t, err, errReplicaGroupsPending)
				}
				require.Equal(t, pending, f.observe().LWS.Annotations[subRolePlanAnnotation])
				require.Equal(t, s.Groups, f.observe().Groups, "waiting must not rewrite Pods")
				s.LWS.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey] = initial
				require.NoError(t, base.Update(t.Context(), s.LWS))
				s.Groups[0].Leader.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				require.NoError(t, base.Status().Update(t.Context(), s.Groups[0].Leader))
			}
			for range 4 {
				f.sync()
			}
			s = f.observe()
			require.EqualValues(t, 2, getLWSReplicas(s.LWS))
			require.Empty(t, s.LWS.Annotations[subRolePlanAnnotation])
			child, coherent := groupSubRole(s.Groups[2])
			require.True(t, coherent)
			require.Empty(t, child, "clear the suffix before publishing physical replicas")
			if failure == "UID" || failure == "RV" {
				require.Equal(t, "concurrent", s.Groups[1].Pods[2].Labels["user"])
			}
			_, err = f.manager.syncSubRoles(t.Context(), f.ds, original, subRoleUpdate{target: map[string]int{"a": 2, "b": 2}})
			require.ErrorIs(t, err, errReplicaGroupsPending)
		})
	}
}

func TestSubRoleNativeBoundsAndInvalidIntent(t *testing.T) {
	for _, change := range []string{"prefix hole", "suffix hole", "native victim", "native generation", "native missing", "native deleting", "native target", "LWS generation", "negative", "overflow", "malformed", "owner"} {
		t.Run(change, func(t *testing.T) {
			f := newSubRoleFixture(t, 7, "a", "a", "b")
			s := f.observe()
			target := map[string]int{"a": 1, "b": 1}
			switch change {
			case "prefix hole", "suffix hole":
				i := 0
				if change == "suffix hole" {
					i = 2
				}
				require.NoError(t, f.manager.client.Delete(t.Context(), s.Groups[i].Leader))
			case "native victim":
				s.LWS.Spec.Replicas = ptr.To[int32](2)
				setSubRoleJSON(s.LWS, subRoleReplicasAnnotation, map[string]int{"a": 2, "b": 0})
				require.NoError(t, f.manager.client.Update(t.Context(), s.LWS))
				s.LeaderStatefulSet.Spec.Replicas = ptr.To[int32](2)
				require.NoError(t, f.manager.client.Update(t.Context(), s.LeaderStatefulSet))
				target = map[string]int{"a": 0, "b": 3}
			case "native generation":
				s.LeaderStatefulSet.Generation++
				require.NoError(t, f.manager.client.Update(t.Context(), s.LeaderStatefulSet))
			case "native missing", "native deleting":
				if change == "native deleting" {
					s.LeaderStatefulSet.Finalizers = []string{"test/hold"}
					require.NoError(t, f.manager.client.Update(t.Context(), s.LeaderStatefulSet))
				}
				require.NoError(t, f.manager.client.Delete(t.Context(), s.LeaderStatefulSet))
			case "native target":
				s.LeaderStatefulSet.Spec.Replicas = ptr.To[int32](4)
				require.NoError(t, f.manager.client.Update(t.Context(), s.LeaderStatefulSet))
			case "LWS generation", "owner", "malformed":
				if change == "LWS generation" {
					s.LWS.Generation++
				}
				if change == "owner" {
					s.LWS.OwnerReferences[0].UID = "foreign"
				}
				if change == "malformed" {
					s.LWS.Annotations[subRolePlanAnnotation] = "{"
				}
				require.NoError(t, f.manager.client.Update(t.Context(), s.LWS))
			case "negative":
				target["a"] = -1
			case "overflow":
				target["a"] = math.MaxInt32
			}
			before := f.observe()
			_, err := f.manager.syncSubRoles(t.Context(), f.ds, before.LWS, subRoleUpdate{target: target})
			require.Error(t, err)
			if change == "native victim" {
				s.Groups[0].Leader.Status.Conditions = nil
				require.NoError(t, f.manager.client.Status().Update(t.Context(), s.Groups[0].Leader))
				before = f.observe() // Admission already cleared outgoing labels.
				require.NotEmpty(t, before.LWS.Annotations[subRolePlanAnnotation])
				require.True(t, before.Groups[2].Ready, "a healthy native victim cannot replace the unready retained ordinal")
			}
			for range 3 {
				_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{})
				if change == "malformed" {
					require.ErrorContains(t, err, subRolePlanAnnotation)
				} else if change == "owner" {
					require.Error(t, err)
				} else {
					require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
				}
			}
			after := f.observe()
			if change == "suffix hole" {
				require.EqualValues(t, 2, getLWSReplicas(after.LWS))
			} else {
				require.Equal(t, before.LWS.Spec, after.LWS.Spec)
				require.Equal(t, before.Groups, after.Groups)
			}
		})
	}
}

func TestSubRoleRecoveryPreservesReadyDonor(t *testing.T) {
	f := newSubRoleFixture(t, 0, "a", "a")
	s := f.observe()
	setSubRoleJSON(s.LWS, subRoleReplicasAnnotation, map[string]int{"a": 1, "b": 1})
	require.NoError(t, f.manager.client.Update(t.Context(), s.LWS))
	s.Groups[0].Leader.Status.Conditions = nil
	require.NoError(t, f.manager.client.Status().Update(t.Context(), s.Groups[0].Leader))
	before := f.observe()
	f.sync()
	require.Equal(t, before.Groups, f.observe().Groups, "moving ordinal 1 would remove a's only Ready group")
	s.Groups[0].Leader.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, f.manager.client.Status().Update(t.Context(), s.Groups[0].Leader))
	f.sync()
	child, coherent := groupSubRole(f.observe().Groups[1])
	require.True(t, coherent)
	require.Equal(t, "b", child)
}

func TestHashSubRoleCostsPrecedeScaleAndRepairActualSurvivors(t *testing.T) {
	for _, wrongVictim := range []bool{false, true} {
		t.Run(fmt.Sprint("wrong-native-victim=", wrongVictim), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "a", "b")
			before := f.observe()
			oldB := before.Groups[3].Leader.UID
			base := f.manager.client
			deletes, publications := 0, 0
			f.manager.client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					deletes++
					return errors.New("DS must never delete a Hash group")
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if lws, ok := obj.(*leaderworkersetv1.LeaderWorkerSet); ok && getLWSReplicas(lws) == 2 && publications == 0 {
						preferred := 0
						for _, group := range f.observe().Groups {
							if group.Leader.Annotations[corev1.PodDeletionCost] == subRoleDeletionCost {
								preferred++
							}
						}
						require.Equal(t, 2, preferred, "costs must be observed before publishing the smaller target")
						publications++
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			require.ErrorIs(t, f.scale(map[string]int{"a": 1, "b": 1}), errReplicaGroupsPending)
			pending := f.observe()
			changed := pending.LWS.DeepCopy()
			changed.Annotations["concurrent"] = "keep"
			require.NoError(t, base.Update(t.Context(), changed))
			_, err := f.manager.syncSubRoles(t.Context(), f.ds, pending.LWS, subRoleUpdate{})
			require.ErrorIs(t, err, errReplicaGroupsPending)
			fresh := f.observe()
			require.Equal(t, pending.Groups, fresh.Groups, "a stale parent must not write deletion costs")

			// This pass changes two costs, but checks the parent only during
			// observation and once before acting, not again for every Pod.
			lwsReads := 0
			f.manager.reader = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*leaderworkersetv1.LeaderWorkerSet); ok {
						lwsReads++
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			_, err = f.manager.syncSubRoles(t.Context(), f.ds, fresh.LWS, subRoleUpdate{})
			require.ErrorIs(t, err, errReplicaGroupsPending)
			require.Equal(t, 2, lwsReads, "parent checks must not grow with the number of cost writes")
			f.manager.reader = base
			for attempt := 0; attempt < 6 && getLWSReplicas(f.observe().LWS) != 2; attempt++ {
				f.sync()
			}
			require.Equal(t, 1, publications)
			accepted := f.observe()
			// Neither a restart nor repeated reconciliation may issue an extra
			// delete, clear costs early, or supersede the accepted pending target.
			f.manager = NewLeaderWorkerSetManager(f.manager.client)
			f.manager.reader = base
			for range 2 {
				f.sync()
			}
			require.ErrorIs(t, f.scale(map[string]int{"a": 4, "b": 1}), errReplicaGroupsPending)
			require.Equal(t, accepted.LWS.Annotations[subRolePlanAnnotation], f.observe().LWS.Annotations[subRolePlanAnnotation])
			require.Equal(t, accepted.Groups, f.observe().Groups)
			if wrongVictim {
				f.native(oldB)
			} else {
				f.native()
			}
			afterNative := f.observe()
			keptB, preferredSurvivor := false, false
			for _, group := range afterNative.Groups {
				keptB = keptB || group.Leader.UID == oldB
				preferredSurvivor = preferredSurvivor || group.Leader.Annotations[corev1.PodDeletionCost] == subRoleDeletionCost
			}
			require.Equal(t, !wrongVictim, keptB)
			if wrongVictim {
				require.True(t, preferredSurvivor, "repair a survivor that had been preferred for deletion, never delete it again")
				require.NotEmpty(t, afterNative.LWS.Annotations[subRolePlanAnnotation])
			}
			for range 6 {
				f.sync()
				f.native()
			}
			assertHashSubRoleCounts(t, f, map[string]int{"a": 1, "b": 1})
			require.Zero(t, deletes, "only the native ReplicaSet simulation may delete Pods")
		})
	}
}

func TestHashSubRoleSurvivorRepairPreservesFeasibleReadyFloors(t *testing.T) {
	for _, readyAfterDelete := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("ready-survivors=%d", readyAfterDelete), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "a", "b")
			s := f.observe()
			worker := s.Groups[2].Pods[1]
			worker.Status.Conditions = nil
			require.NoError(t, f.manager.client.Status().Update(t.Context(), worker))
			oldB := s.Groups[3].Leader.UID
			require.ErrorIs(t, f.scale(map[string]int{"a": 2, "b": 1}), errReplicaGroupsPending)
			plan, err := readSubRolePlan(f.observe().LWS)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": 1, "b": 1}, plan.ReadyFloor)
			for range 6 {
				f.sync()
			}
			require.EqualValues(t, 3, getLWSReplicas(f.observe().LWS))
			f.native(oldB) // ReplicaSet can ignore our preference and remove b.
			s = f.observe()
			for _, group := range s.Groups[readyAfterDelete:2] {
				worker := group.Pods[1]
				worker.Status.Conditions = nil
				require.NoError(t, f.manager.client.Status().Update(t.Context(), worker))
			}
			if readyAfterDelete == 2 {
				// Resume a partially relabeled Ready donor after a restart.
				base, podPatches := f.manager.client, 0
				f.manager.client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, pod := obj.(*corev1.Pod); pod {
						podPatches++
						if podPatches == 2 {
							return errors.New("interrupted labels")
						}
					}
					return c.Patch(ctx, obj, patch, opts...)
				}})
				_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{})
				require.ErrorContains(t, err, "interrupted labels")
				f.manager = NewLeaderWorkerSetManager(base)
			}
			for range 8 {
				f.sync()
			}
			assertHashSubRoleCounts(t, f, map[string]int{"a": 2, "b": 1})
			ready := map[string]int{}
			for _, group := range f.observe().Groups {
				if retainedSubRoleEligible(group) {
					ready[group.Leader.Labels[disaggregatedsetv1.SubRoleLabelKey]]++
				}
			}
			require.Equal(t, readyAfterDelete, ready["a"]+ready["b"])
			if readyAfterDelete == 2 {
				require.Equal(t, 1, ready["a"])
				require.Equal(t, 1, ready["b"], "use a Ready donor whenever the saved floors are feasible")
			}
		})
	}
}

func TestHashSubRoleNativeRolloutStartup(t *testing.T) {
	for _, phase := range []string{"steady", "preparing", "published"} {
		t.Run(phase, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "b")
			f.manager.pending = testPendingWrites(f.manager.reader)
			if phase != "steady" {
				require.ErrorIs(t, f.scale(map[string]int{"a": 0, "b": 1}), errReplicaGroupsPending)
			}
			if phase == "published" {
				for attempt := 0; attempt < 6 && getLWSReplicas(f.observe().LWS) != 1; attempt++ {
					f.sync()
				}
				require.EqualValues(t, 1, getLWSReplicas(f.observe().LWS))
			}
			s := f.observe()
			pending := s.LWS.Annotations[subRolePlanAnnotation]
			// Deployment starts a new ReplicaSet while the old surplus is live.
			// Its unlabelled newborn needs routing before it can become Ready.
			rs := s.ReplicaSets[0].DeepCopy()
			rs.Name, rs.UID, rs.ResourceVersion = "new-rs", "new-rs", ""
			rs.Spec.Replicas, rs.Status.Replicas = ptr.To[int32](1), 1
			require.NoError(t, f.manager.client.Create(t.Context(), rs))
			f.createGroup(rs, 99, "")
			s.LeaderDeployment.Spec.Replicas = ptr.To(int32(getLWSReplicas(s.LWS)))
			require.NoError(t, f.manager.client.Update(t.Context(), s.LeaderDeployment))
			deletes := 0
			f.manager.client = interceptor.NewClient(f.manager.client.(client.WithWatch), interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				deletes++
				return errors.New("native controller owns deletions")
			}})
			r := &DisaggregatedSetReconciler{LWSManager: f.manager}
			for range 3 {
				_, err := r.reconcileSlice(t.Context(), r.createRollingUpdateExecutor(), f.ds, 0, "target", nil)
				require.ErrorIs(t, err, errReplicaGroupsPending, "routing repair must not unblock rollout planning")
			}
			after := f.observe()
			require.False(t, subRoleNativeSettled(after))
			require.Equal(t, pending, after.LWS.Annotations[subRolePlanAnnotation])
			require.Equal(t, getLWSReplicas(s.LWS), getLWSReplicas(after.LWS), "startup repair cannot publish a physical scale")
			require.Len(t, after.Groups, 4)
			found := false
			for _, group := range after.Groups {
				if metav1.GetControllerOf(group.Leader).UID == rs.UID {
					found = true
					name, coherent := groupSubRole(group)
					require.True(t, coherent)
					if phase == "steady" {
						require.Contains(t, []string{"a", "b"}, name)
					} else {
						require.Equal(t, "b", name, "startup must use a positive desired child, not removed a")
					}
				}
			}
			require.True(t, found)
			require.Zero(t, deletes)
		})
	}
}

func TestHashSubRoleTransitionsAndStartup(t *testing.T) {
	for _, target := range []map[string]int{{"a": 1, "b": 1}, {"a": 1, "b": 2}, {"a": 2, "b": 2}, {"a": 0, "b": 0}} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "b")
			require.ErrorIs(t, f.scale(target), errReplicaGroupsPending)
			for range 12 {
				f.sync()
				f.native()
			}
			assertHashSubRoleCounts(t, f, target)
		})
	}
	// All newborn labels are filled in one pass even while the groups load.
	f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "a")
	for _, group := range f.observe().Groups {
		for _, pod := range group.Pods {
			delete(pod.Labels, disaggregatedsetv1.SubRoleLabelKey)
			require.NoError(t, f.manager.client.Update(t.Context(), pod))
			pod.Status.Conditions = nil
			require.NoError(t, f.manager.client.Status().Update(t.Context(), pod))
		}
	}
	_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{})
	require.ErrorIs(t, err, errReplicaGroupsPending)
	for _, group := range f.observe().Groups {
		name, coherent := groupSubRole(group)
		require.True(t, coherent)
		require.Equal(t, "a", name)
	}
}

func TestHashSubRolePendingPlanSurvivesWriteFailures(t *testing.T) {
	for _, failure := range []string{"cost patch", "Spec patch", "cost RV", "cost UID"} {
		t.Run(failure, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "a")
			require.ErrorIs(t, f.scale(map[string]int{"a": 2}), errReplicaGroupsPending)
			base, failed := f.manager.client, false
			f.manager.client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				_, pod := obj.(*corev1.Pod)
				lws, physical := obj.(*leaderworkersetv1.LeaderWorkerSet)
				if !failed && (pod && failure != "Spec patch" || physical && getLWSReplicas(lws) == 2 && failure == "Spec patch") {
					failed = true
					if failure == "cost patch" || failure == "Spec patch" {
						return errors.New("interrupted write")
					}
					current := &corev1.Pod{}
					require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), current))
					if failure == "cost UID" {
						require.NoError(t, c.Delete(ctx, current))
						current.UID, current.ResourceVersion = "replacement", ""
						require.NoError(t, c.Create(ctx, current))
					} else {
						current.Annotations["concurrent"] = "keep"
						require.NoError(t, c.Update(ctx, current))
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			}})
			for attempt := 0; attempt < 6 && !failed; attempt++ {
				_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{})
				if failed {
					require.Error(t, err)
				} else {
					require.ErrorIs(t, err, errReplicaGroupsPending)
				}
			}
			require.True(t, failed)
			require.EqualValues(t, 3, getLWSReplicas(f.observe().LWS), "failed guarded writes cannot publish a drain")
			require.NotEmpty(t, f.observe().LWS.Annotations[subRolePlanAnnotation])
			f.manager = NewLeaderWorkerSetManager(base)
			for range 12 {
				f.sync()
				f.native()
			}
			assertHashSubRoleCounts(t, f, map[string]int{"a": 2})
		})
	}
}

func TestHashSubRolesObserveNativeStateAndRepairCosts(t *testing.T) {
	for _, change := range []string{"settled", "loading", "cost", "user minimum", "externally changed cost", "native cost", "pending", "native pending", "wrong distribution", "missing worker"} {
		t.Run(change, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "b")
			s := f.observe()
			pod := s.Groups[0].Leader
			var object client.Object = pod
			switch change {
			case "loading":
				pod.Status.Conditions = nil
				require.NoError(t, f.manager.client.Status().Update(t.Context(), pod))
			case "cost":
				pod.Annotations[corev1.PodDeletionCost] = subRoleDeletionCost
				pod.Annotations[subRoleOriginalDeletionCostAnnotation] = "none"
			case "user minimum":
				pod.Annotations[corev1.PodDeletionCost] = subRoleDeletionCost
			case "externally changed cost":
				pod.Annotations[corev1.PodDeletionCost] = "200"
				pod.Annotations[subRoleOriginalDeletionCostAnnotation] = "100"
			case "native cost":
				pod.Annotations[corev1.PodDeletionCost] = "-100"
			case "pending":
				s.LWS.Annotations[subRolePlanAnnotation] = "pending"
				object = s.LWS
			case "native pending":
				s.ReplicaSets[0].Generation++
				object = s.ReplicaSets[0]
			case "wrong distribution":
				for _, member := range s.Groups[0].Pods {
					member.Labels[disaggregatedsetv1.SubRoleLabelKey] = "b"
					require.NoError(t, f.manager.client.Update(t.Context(), member))
				}
			case "missing worker":
				require.NoError(t, f.manager.client.Delete(t.Context(), s.Groups[0].Pods[1]))
			}
			if change != "wrong distribution" {
				require.NoError(t, f.manager.client.Update(t.Context(), object))
			}
			s = f.observe()
			// The informer-only implementation has no separate cached no-op
			// predicate. Exercise the real reconciliation and its write path.
			f.manager.pending = testPendingWrites(f.manager.reader)
			_, err := f.manager.syncSubRoles(t.Context(), f.ds, s.LWS, subRoleUpdate{})
			switch change {
			case "pending":
				require.Error(t, err, "malformed saved work must not be ignored")
			case "cost", "externally changed cost", "native pending", "wrong distribution":
				require.ErrorIs(t, err, errReplicaGroupsPending)
			default:
				require.NoError(t, err)
			}
			if change == "missing worker" {
				require.False(t, f.observe().Groups[0].Ready, "an incomplete group contributes no Ready credit")
			}
			if change == "cost" || change == "externally changed cost" {
				require.NotContains(t, f.observe().Groups[0].Leader.Annotations, subRoleOriginalDeletionCostAnnotation)
			}
			if change == "native cost" {
				f.sync()
				require.Equal(t, "-100", f.observe().Groups[0].Leader.Annotations[corev1.PodDeletionCost], "DS must preserve native scheduling preferences")
			}
		})
	}
}

func TestHashSubRoleDeletionPreferenceRestoresNativeGateCost(t *testing.T) {
	for _, gateRemains := range []bool{false, true} {
		t.Run(fmt.Sprint("gate-remains=", gateRemains), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a")
			pod := f.observe().Groups[0].Leader
			pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkersetv1.GroupReplacementSchedulingGate}}
			pod.Annotations[corev1.PodDeletionCost] = "-100"
			require.NoError(t, f.manager.client.Update(t.Context(), pod))
			_, err := f.manager.preferSubRoleDeletion(t.Context(), f.observe().Groups[0], true)
			require.NoError(t, err)
			pod = f.observe().Groups[0].Leader
			require.Equal(t, strconv.FormatInt(math.MinInt32, 10), pod.Annotations[corev1.PodDeletionCost])
			want := "-100"
			if !gateRemains {
				pod.Spec.SchedulingGates = nil
				want = ""
				require.NoError(t, f.manager.client.Update(t.Context(), pod))
			}
			_, err = f.manager.preferSubRoleDeletion(t.Context(), f.observe().Groups[0], false)
			require.NoError(t, err)
			require.Equal(t, want, f.observe().Groups[0].Leader.Annotations[corev1.PodDeletionCost])
			require.NotContains(t, f.observe().Groups[0].Leader.Annotations, subRoleOriginalDeletionCostAnnotation)
		})
	}
}

func TestHashSubRoleSurvivorRestoresOriginalDeletionCost(t *testing.T) {
	for _, original := range []string{"", "0", "100", "-100", subRoleDeletionCost} {
		t.Run("original="+original, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "b")
			s := f.observe()
			pod := s.Groups[1].Leader
			if original != "" {
				pod.Annotations[corev1.PodDeletionCost] = original
				require.NoError(t, f.manager.client.Update(t.Context(), pod))
			}
			require.ErrorIs(t, f.scale(map[string]int{"a": 1, "b": 1}), errReplicaGroupsPending)
			for range 6 {
				f.sync()
			}
			require.EqualValues(t, 2, getLWSReplicas(f.observe().LWS))
			require.Equal(t, subRoleDeletionCost, f.observe().Groups[1].Leader.Annotations[corev1.PodDeletionCost])
			f.manager = NewLeaderWorkerSetManager(f.manager.client)
			for range 2 {
				f.sync() // Replaying the pending plan must not replace the saved cost.
			}
			f.native(s.Groups[2].Leader.UID) // Delete b, leaving the preferred victim alive.
			for range 8 {
				f.sync()
			}
			assertHashSubRoleCounts(t, f, map[string]int{"a": 1, "b": 1})
			survivor := &corev1.Pod{}
			require.NoError(t, f.manager.client.Get(t.Context(), client.ObjectKeyFromObject(pod), survivor))
			require.Equal(t, pod.UID, survivor.UID)
			cost, present := survivor.Annotations[corev1.PodDeletionCost]
			require.Equal(t, original, cost)
			require.Equal(t, original != "", present)
		})
	}
}

func TestHashSubRoleCostCleanupPreservesExternalEdits(t *testing.T) {
	for _, edit := range []string{"", "200"} {
		for _, reapply := range []bool{false, true} {
			t.Run(fmt.Sprintf("edit=%s/reapply=%t", edit, reapply), func(t *testing.T) {
				f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a")
				pod := f.observe().Groups[0].Leader
				pod.Annotations[corev1.PodDeletionCost] = "100"
				require.NoError(t, f.manager.client.Update(t.Context(), pod))
				_, err := f.manager.preferSubRoleDeletion(t.Context(), f.observe().Groups[0], true)
				require.NoError(t, err)
				pod = f.observe().Groups[0].Leader
				delete(pod.Annotations, corev1.PodDeletionCost)
				if edit != "" {
					pod.Annotations[corev1.PodDeletionCost] = edit
				}
				require.NoError(t, f.manager.client.Update(t.Context(), pod))
				if reapply {
					_, err = f.manager.preferSubRoleDeletion(t.Context(), f.observe().Groups[0], true)
					require.NoError(t, err)
				}
				_, err = f.manager.preferSubRoleDeletion(t.Context(), f.observe().Groups[0], false)
				require.NoError(t, err)
				annotations := f.observe().Groups[0].Leader.Annotations
				cost, present := annotations[corev1.PodDeletionCost]
				require.Equal(t, edit, cost)
				require.Equal(t, edit != "", present)
				require.NotContains(t, annotations, subRoleOriginalDeletionCostAnnotation)
			})
		}
	}
}

func TestHashSubRoleNativeObservationBarrier(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*replicagroups.Snapshot)
		settled bool
	}{
		{"unready", func(s *replicagroups.Snapshot) { s.Groups[0].Ready = false }, true},
		{"LWS generation", func(s *replicagroups.Snapshot) { s.LWS.Generation++ }, false},
		{"Deployment generation", func(s *replicagroups.Snapshot) { s.LeaderDeployment.Generation++ }, false},
		{"ReplicaSet generation", func(s *replicagroups.Snapshot) { s.ReplicaSets[0].Generation++ }, false},
		{"stale native count", func(s *replicagroups.Snapshot) { s.ReplicaSets[0].Status.Replicas++ }, false},
		{"native target", func(s *replicagroups.Snapshot) { s.LeaderDeployment.Spec.Replicas = ptr.To[int32](3) }, false},
		{"native deleting", func(s *replicagroups.Snapshot) { s.LeaderDeployment.DeletionTimestamp = ptr.To(metav1.Now()) }, false},
		{"native absent", func(s *replicagroups.Snapshot) { s.LeaderDeployment = nil }, false},
		{"wrong ownership", func(s *replicagroups.Snapshot) { s.Groups[1].Leader.OwnerReferences[0].UID = "foreign" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "b").observe()
			tc.change(s)
			require.Equal(t, tc.settled, subRoleNativeSettled(s))
		})
	}
}

func TestSubRoleScaleDownAfterNativeCreationFailure(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, identity, 0, "a", "a", "a")
			s := f.observe()
			// Native creation is short, for example because quota permits only two groups.
			for _, pod := range s.Groups[2].Pods {
				require.NoError(t, f.manager.client.Delete(t.Context(), pod))
			}
			require.NoError(t, f.manager.client.Delete(t.Context(), s.Groups[2].WorkerStatefulSet))
			if identity == leaderworkersetv1.GroupIdentityHash {
				rs := s.ReplicaSets[0]
				rs.Status.Replicas = 2
				require.NoError(t, f.manager.client.Status().Update(t.Context(), rs))
			} else {
				s.LeaderStatefulSet.Status.Replicas = 2
				require.NoError(t, f.manager.client.Status().Update(t.Context(), s.LeaderStatefulSet))
			}
			before := f.observe().Groups
			// Informer mode waits for the full cohort, even for a creation
			// shortfall. The assignment algorithm alone can cancel missing growth.
			f.manager.pending = testPendingWrites(f.manager.reader)
			require.ErrorIs(t, f.scale(map[string]int{"a": 2}), errReplicaGroupsPending)
			require.EqualValues(t, 3, getLWSReplicas(f.observe().LWS))
			f.manager.pending = nil
			f.manager.client = interceptor.NewClient(f.manager.client.(client.WithWatch), interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				t.Fatal("cancelling missing growth must not delete any group")
				return nil
			}})
			for range 8 {
				if err := f.scale(map[string]int{"a": 2}); err != nil {
					require.ErrorIs(t, err, errReplicaGroupsPending)
				}
			}
			s = f.observe()
			require.Equal(t, before, s.Groups)
			require.EqualValues(t, 2, getLWSReplicas(s.LWS), "cancel missing growth without first creating the unwanted group")
			if identity == leaderworkersetv1.GroupIdentityHash {
				require.NotEmpty(t, s.LWS.Annotations[subRolePlanAnnotation], "wait for native acknowledgement of the new target")
				f.native()
				f.sync()
				require.Empty(t, f.observe().LWS.Annotations[subRolePlanAnnotation])
			}
		})
	}
}

func TestHashSubRoleMissingGrowthKeepsNativeBarriers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*replicagroups.Snapshot)
		allowed bool
	}{
		{"creation shortfall", func(*replicagroups.Snapshot) {}, true},
		{"stale LWS", func(s *replicagroups.Snapshot) { s.LWS.Generation++ }, false},
		{"stale Deployment", func(s *replicagroups.Snapshot) { s.LeaderDeployment.Generation++ }, false},
		{"stale ReplicaSet", func(s *replicagroups.Snapshot) { s.ReplicaSets[0].Generation++ }, false},
		{"stale Pod count", func(s *replicagroups.Snapshot) { s.ReplicaSets[0].Status.Replicas++ }, false},
		{"surplus group", func(s *replicagroups.Snapshot) { s.ReplicaSets[0].Spec.Replicas = ptr.To[int32](1) }, false},
		{"unacknowledged target", func(s *replicagroups.Snapshot) { s.ReplicaSets[0].Spec.Replicas = ptr.To[int32](2) }, false},
		{"terminating group", func(s *replicagroups.Snapshot) { s.Groups[0].Terminating = true }, false},
		{"native rollout", func(s *replicagroups.Snapshot) {
			s.ReplicaSets[0].Spec.Replicas = ptr.To[int32](2)
			newRS := s.ReplicaSets[0].DeepCopy()
			newRS.Name, newRS.UID, newRS.Spec.Replicas, newRS.Status.Replicas = "new", "new", ptr.To[int32](1), 0
			s.ReplicaSets = append(s.ReplicaSets, newRS)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "a").observe()
			s.Groups, s.ReplicaSets[0].Status.Replicas = s.Groups[:2], 2
			tc.change(s)
			require.Equal(t, tc.allowed, hashSubRoleNativeAcknowledged(s, true))
			require.False(t, subRoleNativeSettled(s), "creation shortfall is not completed scale")
		})
	}
}

func TestHashSubRolePlanValidationAndReadyReservation(t *testing.T) {
	lws := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "b").lws
	for _, value := range []string{"{", "null", "{}", `{"counts":{"a":-1},"readyFloor":{}}`, `{"counts":{"a":1},"readyFloor":{"a":2}}`, `{"counts":{"a":1},"readyFloor":{"a":-1}}`} {
		lws.Annotations[subRolePlanAnnotation] = value
		_, err := readSubRolePlan(lws)
		require.Error(t, err, value)
	}
	s := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "b").observe()
	s.Groups[1].Ready = false
	assignments := readySubRoleAssignments(s.Groups, map[string]int{"a": 1, "b": 1}, map[string]int{"b": 1})
	require.Equal(t, "b", assignments[s.Groups[0].Leader.UID])
	require.Equal(t, "a", assignments[s.Groups[1].Leader.UID])
}

func TestHashSubRoleLostReadyFloorDelaysPublication(t *testing.T) {
	f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "a")
	require.ErrorIs(t, f.scale(map[string]int{"a": 2}), errReplicaGroupsPending)
	for _, group := range f.observe().Groups[:2] {
		group.Leader.Status.Conditions = nil
		require.NoError(t, f.manager.client.Status().Update(t.Context(), group.Leader))
	}
	for range 3 {
		f.sync()
		require.EqualValues(t, 3, getLWSReplicas(f.observe().LWS), "one Ready group cannot cover the accepted floor of two")
	}
	ready := f.observe().Groups[0].Leader
	ready.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, f.manager.client.Status().Update(t.Context(), ready))
	for range 8 {
		f.sync()
		f.native()
	}
	assertHashSubRoleCounts(t, f, map[string]int{"a": 2})
}

func TestHashSubRolePrefersReadyGroupsBeyondFloor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		children []string
		unready  int
		target   map[string]int
		child    string
	}{
		{"same child", []string{"a", "a"}, 0, map[string]int{"a": 1}, "a"},
		{"Ready donor before unready child", []string{"a", "b"}, 1, map[string]int{"a": 0, "b": 1}, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, tc.children...)
			s := f.observe()
			worker := s.Groups[tc.unready].Pods[1]
			worker.Status.Conditions = nil
			require.NoError(t, f.manager.client.Status().Update(t.Context(), worker))
			s = f.observe()
			require.False(t, s.Groups[tc.unready].Ready)
			require.True(t, s.Groups[1-tc.unready].Ready)
			// Both leaders are equally healthy: ReplicaSet sees only their cost,
			// not the unready worker that makes one whole group unavailable.
			require.Equal(t, s.Groups[0].Leader.Status, s.Groups[1].Leader.Status)
			require.Equal(t, s.Groups[0].Leader.Spec.NodeName, s.Groups[1].Leader.Spec.NodeName)
			survivor := s.Groups[1-tc.unready].Leader.UID
			require.ErrorIs(t, f.scale(tc.target), errReplicaGroupsPending)
			for range 6 {
				f.sync()
			}
			s = f.observe()
			require.EqualValues(t, 1, getLWSReplicas(s.LWS))
			require.Equal(t, subRoleDeletionCost, s.Groups[tc.unready].Leader.Annotations[corev1.PodDeletionCost])
			require.Empty(t, s.Groups[1-tc.unready].Leader.Annotations[corev1.PodDeletionCost])
			f.native()
			for range 4 {
				f.sync()
			}
			assertHashSubRoleCounts(t, f, tc.target)
			group := f.observe().Groups[0]
			require.Equal(t, survivor, group.Leader.UID)
			require.True(t, group.Ready)
			require.Equal(t, tc.child, group.Leader.Labels[disaggregatedsetv1.SubRoleLabelKey])
		})
	}
}

func TestHashSubRoleReadyFloorExcludesIneligibleGroups(t *testing.T) {
	for _, reason := range []string{"terminating worker", "unobserved worker generation", "exhausted restart budget"} {
		t.Run(reason, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "a")
			for _, group := range f.observe().Groups[1:] {
				switch reason {
				case "terminating worker":
					worker := group.Pods[1]
					worker.Finalizers = []string{"test/hold"}
					require.NoError(t, f.manager.client.Update(t.Context(), worker))
					require.NoError(t, f.manager.client.Delete(t.Context(), worker))
				case "unobserved worker generation":
					group.WorkerStatefulSet.Generation++
					require.NoError(t, f.manager.client.Update(t.Context(), group.WorkerStatefulSet))
				case "exhausted restart budget":
					group.Leader.Annotations[leaderworkersetv1.GroupRestartBudgetExhaustedAnnotationKey] = "true"
					require.NoError(t, f.manager.client.Update(t.Context(), group.Leader))
				}
			}
			for i, group := range f.observe().Groups {
				require.True(t, group.Ready, "raw readiness alone does not grant retained credit")
				require.Equal(t, i == 0, retainedSubRoleEligible(group))
			}
			require.ErrorIs(t, f.scale(map[string]int{"a": 2}), errReplicaGroupsPending)
			plan, err := readSubRolePlan(f.observe().LWS)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": 0}, plan.ReadyFloor)
			for range 4 {
				f.sync()
			}
			require.EqualValues(t, 2, getLWSReplicas(f.observe().LWS), "do not wait for an unattainable raw-Ready floor")
		})
	}
}

func TestHashSubRoleAdapterMembershipAndRemoval(t *testing.T) {
	f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "b")
	s := f.observe()
	spoof := s.Groups[0].Leader.DeepCopy()
	spoof.Name, spoof.UID, spoof.ResourceVersion = "spoof", "foreign", ""
	spoof.OwnerReferences[0].UID = "foreign-rs"
	require.NoError(t, f.manager.client.Create(t.Context(), spoof))
	worker := s.Groups[0].Pods[1]
	worker.Labels[disaggregatedsetv1.SubRoleLabelKey] = "b"
	require.NoError(t, f.manager.client.Update(t.Context(), worker))
	s = f.observe()
	require.Len(t, s.Groups, 3, "foreign ownership cannot inflate logical readiness")
	observed := rolloutReadiness{}
	observeSubRoleReadiness(&subRoleState{snapshot: s, issued: map[string]int{"a": 2, "b": 1}, initial: map[string]int{"a": 2, "b": 1}}, observed)
	require.Equal(t, replicaReadiness{spec: 2, initial: 2, raw: 1, committed: 1}, observed.role(f.revision("old"), "model/a"))
	require.Equal(t, replicaReadiness{spec: 1, initial: 1, raw: 1, committed: 1}, observed.role(f.revision("old"), "model/b"))
	require.Equal(t, replicaReadiness{}, observed.role(f.revision("old"), "model"))
	f.sync() // Repair the partial group before accepting new membership.
	_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{membership: map[string]int{"a": 2, "b": 1, "new": 1}})
	require.ErrorIs(t, err, errReplicaGroupsPending)
	counts, err := subRoleCounts(f.observe().LWS, subRoleReplicasAnnotation)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"a": 2, "b": 1, "new": 0}, counts)
	for range 12 {
		_, err = f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{membership: map[string]int{}})
		require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
		f.native()
	}
	s = f.observe()
	require.EqualValues(t, 3, getLWSReplicas(s.LWS), "unpartitioning keeps physical capacity")
	require.Empty(t, s.LWS.Annotations[subRoleReplicasAnnotation])
	for _, group := range s.Groups {
		child, coherent := groupSubRole(group)
		require.True(t, coherent)
		require.Empty(t, child)
	}
}

func TestHashSubRoleCollectorEnrollsOnlyCurrentTarget(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		t.Run(fmt.Sprint("ordinary-old=", ordinary), func(t *testing.T) {
			children := []string{"a", "a"}
			if ordinary {
				children = []string{"", ""}
			}
			old := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, children...)
			if ordinary {
				lws := old.observe().LWS
				delete(lws.Annotations, subRoleReplicasAnnotation)
				delete(lws.Annotations, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey)
				require.NoError(t, old.manager.client.Update(t.Context(), lws))
			}
			target := old.withRevision("current", "a")
			target.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{
				Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "new"}},
			}}
			target.ds.Spec.Roles[0].Spec.GroupIdentity = leaderworkersetv1.GroupIdentityHash
			desired := map[string]int{"model": 2, "model/a": 1, "model/new": 1}
			before := old.observe()
			var err error
			for range 8 {
				_, err = target.manager.collectRolloutObservations(t.Context(), target.ds,
					disaggregatedsetutils.RevisionRolesList{old.revision("old")}, target.revision("current"), desired)
				if err == nil {
					break
				}
				require.ErrorIs(t, err, errReplicaGroupsPending)
			}
			require.NoError(t, err)
			after := old.observe()
			require.Equal(t, before.LWS.Annotations, after.LWS.Annotations, "old Hash layout and history stay frozen")
			require.Equal(t, before.Groups, after.Groups, "old Hash groups must not be enrolled or relabeled")
			counts, err := subRoleCounts(target.observe().LWS, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": 1, "new": 0}, counts, "the current Hash target enrolls its new child before scaling")

			executor := &RollingUpdateExecutor{LWSManager: target.manager}
			require.NoError(t, executor.scaleRevision(t.Context(), target.ds, old.revision("old"),
				[]string{"model/a", "model/new", "model"}, RoleReplicaState{2, 0, 2}, scaleDown))
			require.Equal(t, before.LWS.Annotations, old.observe().LWS.Annotations, "old execution cannot add a foreign zero-count child")
			require.NoError(t, executor.scaleRevision(t.Context(), target.ds, target.revision("current"),
				[]string{"model/a", "model/new"}, RoleReplicaState{1, 1}, scaleUp), "accepted Hash growth does not block sibling roles")
			counts, err = subRoleCounts(target.observe().LWS, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": 1, "new": 1}, counts)
			require.EqualValues(t, 2, getLWSReplicas(target.lws), "enrolled current children remain executable")
		})
	}
}

func TestHashSubRoleStaleCountsCannotUndoAcceptedDrain(t *testing.T) {
	for _, targets := range []RoleReplicaState{{4, 2}, {4, 3}, {3, 2}} {
		t.Run(fmt.Sprint(targets), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "a", "a", "a", "b")
			executor := &RollingUpdateExecutor{LWSManager: f.manager}
			original, names := f.revision("old"), []string{"model/a", "model/b"}
			require.NoError(t, executor.scaleRevision(t.Context(), f.ds, original, names, targets, scaleDown), "accepted Hash targets do not block sibling roles")
			for range 12 {
				f.sync()
				f.native()
			}
			counts, err := subRoleCounts(f.observe().LWS, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": targets[0], "b": 1}, counts)
			require.ErrorIs(t, executor.scaleRevision(t.Context(), f.ds, original, names, targets, scaleUp), errReplicaGroupsPending)
			err = executor.scaleRevision(t.Context(), f.ds, f.revision("old"), names, targets, scaleUp)
			require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
			counts, err = subRoleCounts(f.observe().LWS, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": targets[0], "b": targets[1]}, counts)
		})
	}
}

func TestHashSubRoleAcceptedDrainPreservesReadyFloorWithoutWaitingForEverySurvivor(t *testing.T) {
	for _, initialReady := range []int{2, 0} {
		t.Run(fmt.Sprint(initialReady), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderworkersetv1.GroupIdentityHash, 0, "a", "a", "a", "a")
			for _, group := range f.observe().Groups[initialReady:] {
				group.Leader.Status.Conditions = nil
				require.NoError(t, f.manager.client.Status().Update(t.Context(), group.Leader))
			}
			require.ErrorIs(t, f.scale(map[string]int{"a": 3}), errReplicaGroupsPending)
			plan, err := readSubRolePlan(f.observe().LWS)
			require.NoError(t, err)
			floor := map[string]int{}
			if initialReady > 0 {
				floor["a"] = initialReady - 1
			}
			require.Equal(t, floor, plan.ReadyFloor, "an explicit empty floor must survive serialized replay")
			f.manager = NewLeaderWorkerSetManager(f.manager.client)
			for range 12 {
				f.sync()
				f.native()
			}
			s := f.observe()
			require.Empty(t, s.LWS.Annotations[subRolePlanAnnotation])
			require.Len(t, s.Groups, 3)
			ready := 0
			for _, group := range s.Groups {
				if group.Ready {
					ready++
				}
			}
			require.GreaterOrEqual(t, ready, floor["a"])
			require.Less(t, ready, len(s.Groups), "safe progress must not require every retained group to recover")
		})
	}
}

func TestSubRoleRolloutAppliesEveryParent(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		initial, old, target []int
		surge                []int
		bootstrap            bool
	}{
		{"growth and old drain", []int{2, 2}, []int{2, 2}, []int{0, 0}, []int{1, 1}, false},
		{"bootstrap", []int{2, 5}, []int{2, 4}, []int{0, 1}, []int{0, 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, target, executor := subRoleRolloutParents(t, tc.initial, tc.old, tc.target)
			state := rolloutState(tc.initial, tc.old, tc.old, nil, nil, tc.target, tc.target, tc.initial, configs(tc.surge, []int{1, 1}))
			if tc.bootstrap {
				state.ActiveOld.RawReadyReplicas[0], state.ActiveOld.ReadyReplicas[0] = 1, 1
			}
			step := ComputeNextStep(state)
			require.NotNil(t, step)
			require.Equal(t, tc.bootstrap, step.Kind == BootstrapSurgeStep)
			err := executor.applyRolloutStep(t.Context(), old[0].ds, subRoleParentRevision(target), rolloutInputs{allRoleNames: []string{"prefill/pool", "decode/pool"}}, subRoleParentRevision(old), state, step)
			require.NoError(t, err)
			for i := range target {
				require.EqualValues(t, step.New[i], getLWSReplicas(target[i].observe().LWS), "accept both parents before waiting")
				if step.Past[i] < tc.old[i] {
					counts, err := subRoleCounts(old[i].observe().LWS, subRolePlanAnnotation)
					require.NoError(t, err)
					require.Equal(t, step.Past[i], counts["pool"], "accept the old drain in the same step")
				}
			}
			recorder := executor.Record.(*events.FakeRecorder)
			var emitted string
			for len(recorder.Events) > 0 {
				emitted += <-recorder.Events
			}
			require.Contains(t, emitted, EventReasonScalingUp)
			for i, parent := range testRoleNames() {
				if step.New[i] > tc.target[i] {
					require.Contains(t, emitted, parent)
				}
			}
			require.Equal(t, tc.bootstrap, strings.Contains(emitted, EventReasonBootstrapSurge), "accepted bootstrap growth must be observable")
		})
	}
}

func TestSubRoleRolloutStopsOnFailedSiblingWrite(t *testing.T) {
	old, target, executor := subRoleRolloutParents(t, []int{2, 2}, []int{2, 2}, []int{0, 0})
	executor.LWSManager.client = interceptor.NewClient(executor.LWSManager.client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if object.GetName() == target[1].lws.Name {
				return fmt.Errorf("injected sibling write failure")
			}
			return c.Patch(ctx, object, patch, options...)
		},
	})
	state := rolloutState([]int{2, 2}, []int{2, 2}, []int{2, 2}, nil, nil, []int{0, 0}, []int{0, 0}, []int{2, 2}, configs([]int{1, 1}, []int{1, 1}))
	err := executor.applyRolloutStep(t.Context(), old[0].ds, subRoleParentRevision(target), rolloutInputs{allRoleNames: []string{"prefill/pool", "decode/pool"}}, subRoleParentRevision(old), state, ComputeNextStep(state))
	require.ErrorContains(t, err, "injected sibling write failure")
	require.EqualValues(t, 1, getLWSReplicas(target[0].observe().LWS))
	require.Zero(t, getLWSReplicas(target[1].observe().LWS))
	for _, fixture := range old {
		require.Empty(t, fixture.observe().LWS.Annotations[subRolePlanAnnotation], "failed growth must not publish an old drain")
		require.EqualValues(t, 2, getLWSReplicas(fixture.lws))
	}
	recorder := executor.Record.(*events.FakeRecorder)
	require.NotEmpty(t, recorder.Events)
	require.Contains(t, <-recorder.Events, EventReasonScalingUp, "retain the event for the successful first parent")
}

func subRoleRolloutParents(t *testing.T, initial, oldCounts, targetCounts []int) (old, target []*subRoleFixture, executor *RollingUpdateExecutor) {
	root := newSubRoleFixture(t, 0)
	root.ds.Spec.Roles = nil
	for i, name := range []string{"prefill", "decode"} {
		root.ds.Spec.Roles = append(root.ds.Spec.Roles, disaggregatedsetv1.DisaggregatedRoleSpec{Name: name, SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "pool", Replicas: ptr.To(int32(initial[i]))}}})
		for j, counts := range [][]int{oldCounts, targetCounts} {
			fixture := root.withRevision(fmt.Sprintf("%s-%d", name, j), slices.Repeat([]string{"pool"}, counts[i])...)
			fixture.lws.Labels[disaggregatedsetv1.RoleLabelKey] = name
			setSubRoleJSON(fixture.lws, subRoleReplicasAnnotation, map[string]int{"pool": counts[i]})
			setSubRoleJSON(fixture.lws, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey, map[string]int{"pool": initial[i]})
			setInitialReplicasAnnotation(fixture.lws, initial[i])
			require.NoError(t, root.manager.client.Update(t.Context(), fixture.lws))
			if j == 0 {
				old = append(old, fixture)
			} else {
				target = append(target, fixture)
			}
		}
	}
	return old, target, &RollingUpdateExecutor{LWSManager: root.manager, Record: events.NewFakeRecorder(20)}
}

func subRoleParentRevision(parents []*subRoleFixture) disaggregatedsetutils.RevisionRoles {
	return disaggregatedsetutils.RevisionRoles{Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		"prefill": parents[0].observe().LWS, "decode": parents[1].observe().LWS,
	}}
}
