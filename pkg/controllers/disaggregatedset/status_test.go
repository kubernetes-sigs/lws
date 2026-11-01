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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilcache "k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/apimachinery/pkg/util/intstr"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestStatusWriteCacheLifecycle(t *testing.T) {
	ds := newDSWithRoles("ds", staticRole("prefill"))
	base := newTestClient(ds)
	key := client.ObjectKeyFromObject(ds)
	require.NoError(t, base.Get(t.Context(), key, ds))
	writes := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	r := newTestReconciler(c)
	clock := clocktesting.NewFakeClock(time.Now())
	r.staleStatusVersions = utilcache.NewLRUExpireCacheWithClock(maxStaleStatusVersions, clock)
	desired := map[string]int{"prefill": 1}
	publish := func() error {
		return r.updateStatus(t.Context(), ds.DeepCopy(), []string{"prefill"}, "current", desired, nil)
	}
	require.NoError(t, publish())
	desired["prefill"] = 2
	for range 100 {
		require.ErrorIs(t, publish(), errStatusPending)
	}
	require.Equal(t, 1, writes, "child events must not repeat a write from the same stale parent")

	for i, eviction := range []string{"expiry", "capacity"} {
		if eviction == "expiry" {
			clock.Step(statusVersionTTL + time.Nanosecond)
		} else {
			for j := range maxStaleStatusVersions {
				r.staleStatusVersions.Add(client.ObjectKey{Name: fmt.Sprint(j)}, statusVersion{}, statusVersionTTL)
			}
		}
		// Eviction permits an attempt, not an unsafe write. Its conflict is then suppressed.
		require.ErrorIs(t, publish(), errStatusPending, eviction)
		require.ErrorIs(t, publish(), errStatusPending, eviction)
		require.Equal(t, i+2, writes, eviction)
	}
	require.NoError(t, base.Get(t.Context(), key, ds))
	require.EqualValues(t, 1, *ds.Status.RoleStatuses[0].DesiredReplicas, "stale writes were rejected")
	// Another writer advances the version again; a watch can skip our write response.
	meta.SetStatusCondition(&ds.Status.Conditions, metav1.Condition{
		Type: "OtherController", Status: metav1.ConditionTrue, Reason: "Healthy", Message: "preserve me",
	})
	require.NoError(t, base.Status().Update(t.Context(), ds))
	require.NoError(t, publish())
	require.Equal(t, 4, writes)
	require.NoError(t, base.Get(t.Context(), key, ds))
	require.EqualValues(t, 2, *ds.Status.RoleStatuses[0].DesiredReplicas)
	require.True(t, meta.IsStatusConditionTrue(ds.Status.Conditions, "OtherController"))
	require.NoError(t, publish())
	require.Equal(t, 4, writes, "unchanged status remains a no-op")
	_, retained := r.staleStatusVersions.Get(key)
	require.False(t, retained, "even a no-op clears the old marker after the cache advances")
	desired["prefill"] = 3
	require.NoError(t, publish())
	require.NoError(t, base.Delete(t.Context(), ds))
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	_, retained = r.staleStatusVersions.Get(key)
	require.False(t, retained, "NotFound clears the marker")
}

func TestStatusPendingPreservesOtherWorkAndErrors(t *testing.T) {
	for _, failure := range []string{"none", "workload", "scaler", "status"} {
		t.Run(failure, func(t *testing.T) {
			ds := newDSWithRoles("ds", externalRole("prefill"))
			ds.Annotations = map[string]string{
				disaggregatedsetv1.RevisionHashVersionAnnotationKey: disaggregatedsetv1.RevisionHashVersion,
				disaggregatedsetv1.MaxUnavailableAnnotationKey:      `{"prefill":0}`,
			}
			statusWrites, creates, scalerWrites := 0, 0, 0
			injectedErr := apierrors.NewForbidden(schema.GroupResource{}, ds.Name, errors.New("denied"))
			c := interceptor.NewClient(newTestClient(ds), interceptor.Funcs{
				SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					statusWrites++
					if failure == "status" {
						return injectedErr
					}
					return apierrors.NewConflict(schema.GroupResource{}, ds.Name, errors.New("stale"))
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					scalerWrites++
					if failure == "scaler" {
						return injectedErr
					}
					return c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, workload := obj.(*leaderworkersetv1.LeaderWorkerSet); workload {
						creates++
						if failure == "workload" {
							return injectedErr
						}
					}
					return c.Create(ctx, obj, opts...)
				},
			})
			r := newTestReconciler(c)
			for range 2 {
				result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)})
				if failure == "none" {
					require.NoError(t, err)
					require.Equal(t, time.Second, result.RequeueAfter)
				} else {
					require.ErrorIs(t, err, injectedErr)
					require.NotErrorIs(t, err, errStatusPending)
				}
			}
			wantStatusWrites := 1
			if failure == "status" {
				wantStatusWrites = 2 // Genuine errors must not add a suppression marker.
			}
			require.Equal(t, wantStatusWrites, statusWrites)
			require.Equal(t, 2, scalerWrites, "parent status waiting must not suppress scaler status")
			require.Positive(t, creates)
			if failure == "workload" {
				require.Equal(t, 2, creates, "workload retries continue while status waits")
			}
		})
	}
}

// TestSetDisaggregatedSetCondition_ObservedGenerationOnlyDoesNotResetTransitionTime
// guards against a condition-transition-time bug: bumping only ObservedGeneration
// while a condition's Status stays the same must not look like a fresh transition
// to clients (metav1.Condition contract), so LastTransitionTime must be preserved.
func TestSetDisaggregatedSetCondition_ObservedGenerationOnlyDoesNotResetTransitionTime(t *testing.T) {
	original := metav1.NewTime(time.Now().Add(-time.Hour))
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Status: disaggregatedsetv1.DisaggregatedSetStatus{
			Conditions: []metav1.Condition{{
				Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				LastTransitionTime: original,
				Reason:             "AllRolesReady",
				Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
			}},
		},
	}

	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 2, // spec generation advanced, but the condition's Status did not change.
		Reason:             "AllRolesReady",
		Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
	}

	changed := setDisaggregatedSetCondition(ds, newCondition)

	assert.True(t, changed, "ObservedGeneration bump should still be reported as a status change")
	require.Len(t, ds.Status.Conditions, 1)
	assert.EqualValues(t, 2, ds.Status.Conditions[0].ObservedGeneration, "ObservedGeneration should be updated")
	assert.Equal(t, original.Time, ds.Status.Conditions[0].LastTransitionTime.Time, "LastTransitionTime must not change when Status didn't transition")
}

// TestSetDisaggregatedSetCondition_StatusFlipUpdatesTransitionTime verifies the
// opposite case: an actual Status flip (Available -> Progressing) must refresh
// LastTransitionTime on both the newly-true and newly-false conditions.
func TestSetDisaggregatedSetCondition_StatusFlipUpdatesTransitionTime(t *testing.T) {
	original := metav1.NewTime(time.Now().Add(-time.Hour))
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Status: disaggregatedsetv1.DisaggregatedSetStatus{
			Conditions: []metav1.Condition{{
				Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				LastTransitionTime: original,
				Reason:             "AllRolesReady",
				Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
			}},
		},
	}

	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetProgressing),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "RolloutInProgress",
		Message:            "Not all roles have reached their desired replica count, ready and updated to the current revision",
	}

	changed := setDisaggregatedSetCondition(ds, newCondition)

	assert.True(t, changed)
	require.Len(t, ds.Status.Conditions, 2)

	available := findCondition(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable))
	require.NotNil(t, available)
	assert.Equal(t, metav1.ConditionFalse, available.Status)
	assert.True(t, available.LastTransitionTime.Time.After(original.Time), "a real Status flip must refresh LastTransitionTime")
	assert.Equal(t, newCondition.Reason, available.Reason, "the flipped-to-false condition must not keep a Reason that contradicts its new Status")
	assert.Equal(t, newCondition.Message, available.Message)
	assert.EqualValues(t, 1, available.ObservedGeneration)

	progressing := findCondition(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetProgressing))
	require.NotNil(t, progressing)
	assert.Equal(t, metav1.ConditionTrue, progressing.Status)
}

// TestSetDisaggregatedSetCondition_SameStatusSyncsReasonAndMessage verifies that
// Reason/Message stay in sync with the newly-computed condition even when Status
// doesn't change (only LastTransitionTime is preserved in that case).
func TestSetDisaggregatedSetCondition_SameStatusSyncsReasonAndMessage(t *testing.T) {
	original := metav1.NewTime(time.Now().Add(-time.Hour))
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Status: disaggregatedsetv1.DisaggregatedSetStatus{
			Conditions: []metav1.Condition{{
				Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				LastTransitionTime: original,
				Reason:             "Stale",
				Message:            "stale message from a previous reconcile",
			}},
		},
	}

	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "AllRolesReady",
		Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
	}

	changed := setDisaggregatedSetCondition(ds, newCondition)

	assert.True(t, changed)
	require.Len(t, ds.Status.Conditions, 1)
	assert.Equal(t, "AllRolesReady", ds.Status.Conditions[0].Reason)
	assert.Equal(t, newCondition.Message, ds.Status.Conditions[0].Message)
	assert.Equal(t, original.Time, ds.Status.Conditions[0].LastTransitionTime.Time, "LastTransitionTime must not change when Status didn't transition")
}

// TestSetDisaggregatedSetCondition_StaleFalseConditionObservedGenerationCatchesUp
// is a regression test for #980 review feedback: the *other* condition in the
// exclusive pair, once it has settled to Status=False, was only ever touched by
// the Status-flip branch — so if it stays False across further reconciles while
// spec generation keeps advancing, its ObservedGeneration froze at whatever it
// was on the last flip. That's misleading for a client reading
// conditions[Progressing].observedGeneration expecting it to track how current
// the reported state is. Reason/Message must NOT change, since they still
// accurately describe why the condition became false.
func TestSetDisaggregatedSetCondition_StaleFalseConditionObservedGenerationCatchesUp(t *testing.T) {
	original := metav1.NewTime(time.Now().Add(-time.Hour))
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Status: disaggregatedsetv1.DisaggregatedSetStatus{
			Conditions: []metav1.Condition{
				{
					Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 2,
					LastTransitionTime: original,
					Reason:             "AllRolesReady",
					Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
				},
				{
					Type:               string(disaggregatedsetv1.DisaggregatedSetProgressing),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: 1, // stale: set on an earlier reconcile, before Available flipped true.
					LastTransitionTime: original,
					Reason:             "AllRolesReady",
					Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
				},
			},
		},
	}

	// Available stays true on a later reconcile (generation advanced, no flip).
	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 3,
		Reason:             "AllRolesReady",
		Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
	}

	changed := setDisaggregatedSetCondition(ds, newCondition)

	assert.True(t, changed)
	progressing := findCondition(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetProgressing))
	require.NotNil(t, progressing)
	assert.Equal(t, metav1.ConditionFalse, progressing.Status, "must not flip back to true")
	assert.EqualValues(t, 3, progressing.ObservedGeneration, "ObservedGeneration must catch up even while staying False")
	assert.Equal(t, original.Time, progressing.LastTransitionTime.Time, "LastTransitionTime must not change when Status didn't transition")
	assert.Equal(t, "AllRolesReady", progressing.Reason, "Reason must not change: it still accurately describes why this became false")
}

// TestSetDisaggregatedSetCondition_LeavesUnrelatedConditionTypeUntouched guards
// against a broader mutual-exclusivity bug: setDisaggregatedSetCondition must
// only flip the specific Available/Progressing pair, not any other Status=True
// condition it happens to find (Copilot review on #980) — otherwise a future
// condition type, or one written by another controller, would get silently
// clobbered just for being true when Available/Progressing changes.
func TestSetDisaggregatedSetCondition_LeavesUnrelatedConditionTypeUntouched(t *testing.T) {
	original := metav1.NewTime(time.Now().Add(-time.Hour))
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Status: disaggregatedsetv1.DisaggregatedSetStatus{
			Conditions: []metav1.Condition{{
				Type:               "SomeUnrelatedCondition",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				LastTransitionTime: original,
				Reason:             "Unrelated",
				Message:            "set by something else entirely",
			}},
		},
	}

	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "AllRolesReady",
		Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
	}

	setDisaggregatedSetCondition(ds, newCondition)

	unrelated := findCondition(ds.Status.Conditions, "SomeUnrelatedCondition")
	require.NotNil(t, unrelated)
	assert.Equal(t, metav1.ConditionTrue, unrelated.Status, "a condition type outside the Available/Progressing pair must not be flipped")
	assert.Equal(t, original.Time, unrelated.LastTransitionTime.Time)
	assert.Equal(t, "Unrelated", unrelated.Reason)
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// TestSetDesiredReplicas checks that each role and sub-role publishes the target
// the controller judges availability against, totalled across slices, and that an
// unknown target is left unset rather than reported as a requested zero.
func TestSetDesiredReplicas(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desired map[string]int
		slices  int32
		want    []disaggregatedsetv1.RoleStatus
	}{
		{
			name:    "known targets",
			desired: map[string]int{"prefill": 2, "decode": 0},
			slices:  1,
			want:    []disaggregatedsetv1.RoleStatus{{Name: "prefill", DesiredReplicas: ptr.To[int32](2)}, {Name: "decode", DesiredReplicas: ptr.To[int32](0)}},
		},
		{
			name:    "totalled across slices",
			desired: map[string]int{"prefill": 2, "decode": 3},
			slices:  4,
			want:    []disaggregatedsetv1.RoleStatus{{Name: "prefill", DesiredReplicas: ptr.To[int32](8)}, {Name: "decode", DesiredReplicas: ptr.To[int32](12)}},
		},
		{
			name:    "unknown target is unset",
			desired: map[string]int{"decode": 1},
			slices:  1,
			want:    []disaggregatedsetv1.RoleStatus{{Name: "prefill"}, {Name: "decode", DesiredReplicas: ptr.To[int32](1)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statuses := []disaggregatedsetv1.RoleStatus{{Name: "prefill"}, {Name: "decode"}}
			setDesiredReplicas(statuses, tc.desired, tc.slices)
			require.Equal(t, tc.want, statuses)
		})
	}

	t.Run("sub-roles", func(t *testing.T) {
		statuses := []disaggregatedsetv1.RoleStatus{{Name: "model", SubRoleStatuses: []disaggregatedsetv1.SubRoleStatus{{Name: "a"}, {Name: "b"}}}}
		setDesiredReplicas(statuses, map[string]int{"model/a": 2}, 1)
		require.Equal(t, []disaggregatedsetv1.RoleStatus{{Name: "model", SubRoleStatuses: []disaggregatedsetv1.SubRoleStatus{
			{Name: "a", DesiredReplicas: ptr.To[int32](2)}, {Name: "b"},
		}}}, statuses, "a parent whose children are not all resolved has no target")
	})
}

// TestSummarizeRoleStatusesUsesDesiredReplicas checks that availability is judged
// against the published target, so status readers and the controller agree.
func TestSummarizeRoleStatusesUsesDesiredReplicas(t *testing.T) {
	ready := func(n int32, desired *int32) disaggregatedsetv1.RoleStatus {
		return disaggregatedsetv1.RoleStatus{Name: "decode", DesiredReplicas: desired, Replicas: n, ReadyReplicas: n, UpdatedReplicas: n}
	}
	for _, tc := range []struct {
		name   string
		status disaggregatedsetv1.RoleStatus
		want   bool
	}{
		{"at its target", ready(4, ptr.To[int32](4)), true},
		{"below its target", ready(4, ptr.To[int32](6)), false},
		{"scaled to zero", ready(0, ptr.To[int32](0)), true},
		{"unknown target", ready(0, nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replicas, available := summarizeRoleStatuses([]disaggregatedsetv1.RoleStatus{tc.status})
			assert.Equal(t, tc.want, available)
			assert.Equal(t, map[string]int32{"decode": tc.status.Replicas}, replicas)
		})
	}
}

func withMaxUnavailable(role disaggregatedsetv1.DisaggregatedRoleSpec, budget intstr.IntOrString) disaggregatedsetv1.DisaggregatedRoleSpec {
	role.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt32(0), MaxUnavailable: budget}
	return role
}

// servingLWS builds an ordinary role workload owned by ds. An optional initial
// value records the revision's frozen intended replica count.
func servingLWS(ds *disaggregatedsetv1.DisaggregatedSet, role, slice, revision string, spec, ready int32, initial ...int32) *leaderworkersetv1.LeaderWorkerSet {
	lws := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s-%s", ds.Name, slice, revision, role),
			Namespace: ds.Namespace,
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey:  ds.Name,
				disaggregatedsetv1.RoleLabelKey:     role,
				disaggregatedsetv1.SliceLabelKey:    slice,
				disaggregatedsetv1.RevisionLabelKey: revision,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ds, disaggregatedsetv1.GroupVersion.WithKind("DisaggregatedSet"))},
		},
		Spec:   leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(spec)},
		Status: leaderworkersetv1.LeaderWorkerSetStatus{Replicas: spec, ReadyReplicas: ready, UpdatedReplicas: spec},
	}
	if len(initial) > 0 {
		lws.Annotations = map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: fmt.Sprint(initial[0])}
	}
	return lws
}

// TestSetMinAvailableReplicas checks that each floor uses the planner's budget
// resolution: per slice, against the latest target, then totalled across slices.
func TestSetMinAvailableReplicas(t *testing.T) {
	ds := newDSWithRoles("ds",
		withMaxUnavailable(staticRole("prefill"), intstr.FromString("50%")),
		staticRole("decode"),
		disaggregatedsetv1.DisaggregatedRoleSpec{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{
			{Name: "hot", MaxSurge: ptr.To(intstr.FromInt32(1)), MaxUnavailable: ptr.To(intstr.FromInt32(0))},
			{Name: "cold"},
		}},
	)
	statuses := func() []disaggregatedsetv1.RoleStatus {
		return []disaggregatedsetv1.RoleStatus{{Name: "prefill"}, {Name: "decode"}, {Name: "model", SubRoleStatuses: []disaggregatedsetv1.SubRoleStatus{{Name: "hot"}, {Name: "cold"}}}}
	}
	for _, tc := range []struct {
		name    string
		desired map[string]int
		slices  int32
		want    []*int32 // prefill, decode, model, model/hot, model/cold
	}{
		{
			name:    "budgets resolved per slice and totalled",
			desired: map[string]int{"prefill": 4, "decode": 0, "model": 5, "model/hot": 3, "model/cold": 2},
			slices:  2,
			// prefill: 4-50%=2; decode: never below zero; hot: maxUnavailable 0; cold: default 1.
			want: []*int32{ptr.To[int32](4), ptr.To[int32](0), ptr.To[int32](8), ptr.To[int32](6), ptr.To[int32](2)},
		},
		{
			name:    "unknown targets stay unset",
			desired: map[string]int{"decode": 2, "model/cold": 2},
			slices:  1,
			want:    []*int32{nil, ptr.To[int32](1), nil, nil, ptr.To[int32](1)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := statuses()
			setMinAvailableReplicas(ds, got, tc.desired, tc.slices)
			assert.Equal(t, tc.want, []*int32{
				got[0].MinAvailableReplicas, got[1].MinAvailableReplicas, got[2].MinAvailableReplicas,
				got[2].SubRoleStatuses[0].MinAvailableReplicas, got[2].SubRoleStatuses[1].MinAvailableReplicas,
			})
		})
	}
}

// TestServingCapacityRequiresEveryRoleOfARevision checks that, as in the rollout
// planner, Ready groups count as available only in a revision of a slice where
// every role that revision requires has a Ready group.
func TestServingCapacityRequiresEveryRoleOfARevision(t *testing.T) {
	ds := newDSWithRoles("ds", staticRole("prefill"), staticRole("decode"))
	lws := func(role, slice, revision string, spec, ready int32, initial ...int32) *leaderworkersetv1.LeaderWorkerSet {
		return servingLWS(ds, role, slice, revision, spec, ready, initial...)
	}
	for _, tc := range []struct {
		name                    string
		workloads               []*leaderworkersetv1.LeaderWorkerSet
		wantPrefill, wantDecode int32
	}{
		{
			name: "old revision serves while its replacement is incomplete",
			workloads: []*leaderworkersetv1.LeaderWorkerSet{
				lws("prefill", "0", "old", 1, 1, 2), lws("decode", "0", "old", 3, 3, 4),
				lws("prefill", "0", "new", 1, 1), lws("decode", "0", "new", 1, 0),
			},
			wantPrefill: 1, wantDecode: 3,
		},
		{
			name:      "a Ready role cannot serve alone",
			workloads: []*leaderworkersetv1.LeaderWorkerSet{lws("prefill", "0", "new", 2, 2), lws("decode", "0", "new", 2, 0)},
		},
		{
			name: "slices serve independently",
			workloads: []*leaderworkersetv1.LeaderWorkerSet{
				lws("prefill", "0", "new", 2, 2), lws("decode", "0", "new", 2, 0),
				lws("prefill", "1", "new", 2, 0), lws("decode", "1", "new", 2, 2),
				lws("prefill", "2", "new", 1, 1), lws("decode", "2", "new", 1, 1),
			},
			wantPrefill: 1, wantDecode: 1,
		},
		{
			name:        "a role with no intended replicas is not required",
			workloads:   []*leaderworkersetv1.LeaderWorkerSet{lws("prefill", "0", "new", 2, 2), lws("decode", "0", "new", 0, 0)},
			wantPrefill: 2,
		},
		{
			name:      "a drained role stays required by its frozen baseline",
			workloads: []*leaderworkersetv1.LeaderWorkerSet{lws("prefill", "0", "old", 2, 2, 2), lws("decode", "0", "old", 0, 0, 3)},
		},
		{
			name: "a removed role is still required by its old revision",
			workloads: []*leaderworkersetv1.LeaderWorkerSet{
				lws("prefill", "0", "old", 2, 2, 2), lws("decode", "0", "old", 2, 2, 2), lws("encode", "0", "old", 1, 0, 1),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capacity := servingCapacity{}
			for _, workload := range tc.workloads {
				capacity.observeWorkload(workload, workload.Labels[disaggregatedsetv1.RoleLabelKey])
			}
			statuses := []disaggregatedsetv1.RoleStatus{{Name: "prefill", ReadyReplicas: 10}, {Name: "decode", ReadyReplicas: 10}}
			capacity.setAvailableReplicas(statuses)
			assert.Equal(t, []int32{tc.wantPrefill, tc.wantDecode}, []int32{statuses[0].AvailableReplicas, statuses[1].AvailableReplicas})
		})
	}
}

// TestAvailableReplicasNeverExceedReadyReplicas covers recorded child groups that
// briefly count more Ready replicas than the workload's own status reports.
func TestAvailableReplicasNeverExceedReadyReplicas(t *testing.T) {
	ds := newDSWithRoles("ds", staticRole("prefill"))
	capacity := servingCapacity{}
	capacity.observeWorkload(servingLWS(ds, "prefill", "0", "current", 2, 2), "prefill")
	statuses := []disaggregatedsetv1.RoleStatus{{Name: "prefill", ReadyReplicas: 1}}
	capacity.setAvailableReplicas(statuses)
	assert.EqualValues(t, 1, statuses[0].AvailableReplicas)
}

// TestServingCapacityRequiresTargets checks that the current revision requires
// every role with a positive target in each slice, even with no workload for it.
func TestServingCapacityRequiresTargets(t *testing.T) {
	ds := newDSWithRoles("ds", staticRole("prefill"), staticRole("decode"))
	ds.Spec.Slices = ptr.To[int32](2)
	for _, tc := range []struct {
		name      string
		workloads []*leaderworkersetv1.LeaderWorkerSet
		desired   map[string]int
		want      int32
	}{
		{
			name:      "a missing role stops its peer from serving",
			workloads: []*leaderworkersetv1.LeaderWorkerSet{servingLWS(ds, "prefill", "0", "current", 2, 2)},
			desired:   map[string]int{"prefill": 2, "decode": 1},
		},
		{
			name:      "a missing role with a zero target is not required",
			workloads: []*leaderworkersetv1.LeaderWorkerSet{servingLWS(ds, "prefill", "0", "current", 2, 2)},
			desired:   map[string]int{"prefill": 2, "decode": 0},
			want:      2,
		},
		{
			name:      "a missing role with an unknown target is not required",
			workloads: []*leaderworkersetv1.LeaderWorkerSet{servingLWS(ds, "prefill", "0", "current", 2, 2)},
			desired:   map[string]int{"prefill": 2},
			want:      2,
		},
		{
			name: "a slice missing a role does not stop the other slice",
			workloads: []*leaderworkersetv1.LeaderWorkerSet{
				servingLWS(ds, "prefill", "0", "current", 2, 2), servingLWS(ds, "decode", "0", "current", 1, 1),
				servingLWS(ds, "prefill", "1", "current", 2, 2),
			},
			desired: map[string]int{"prefill": 2, "decode": 1},
			want:    2,
		},
		{
			name: "an old revision is not held to the current targets",
			workloads: []*leaderworkersetv1.LeaderWorkerSet{
				servingLWS(ds, "prefill", "0", "old", 2, 2, 2),
			},
			desired: map[string]int{"prefill": 2, "decode": 1},
			want:    2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capacity := servingCapacity{}
			for _, workload := range tc.workloads {
				capacity.observeWorkload(workload, workload.Labels[disaggregatedsetv1.RoleLabelKey])
			}
			capacity.requireTargets(ds, "current", tc.desired)
			statuses := []disaggregatedsetv1.RoleStatus{{Name: "prefill", ReadyReplicas: 10}, {Name: "decode", ReadyReplicas: 10}}
			capacity.setAvailableReplicas(statuses)
			assert.Equal(t, tc.want, statuses[0].AvailableReplicas)
		})
	}
}

// TestUpdateStatusReportsMissingWorkloadOfCurrentRevision covers a workload that
// was deleted and cannot be recreated: its peer must not stay available without it.
func TestUpdateStatusReportsMissingWorkloadOfCurrentRevision(t *testing.T) {
	ds := newDSWithRoles("ds",
		withMaxUnavailable(staticRole("prefill"), intstr.FromInt32(1)),
		withMaxUnavailable(staticRole("decode"), intstr.FromInt32(1)),
	)
	c := newTestClient(ds, servingLWS(ds, "prefill", "0", "current", 2, 2))
	key := client.ObjectKeyFromObject(ds)
	require.NoError(t, c.Get(t.Context(), key, ds))
	r := newTestReconciler(c)
	desired := map[string]int{"prefill": 2, "decode": 1}
	require.NoError(t, r.updateStatus(t.Context(), ds, []string{"prefill", "decode"}, "current", desired, nil))
	require.NoError(t, c.Get(t.Context(), key, ds))
	assert.Zero(t, ds.Status.RoleStatuses[0].AvailableReplicas)
	condition := meta.FindStatusCondition(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetMinimumReplicasAvailable))
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Contains(t, condition.Message, "prefill (0/1)")
}

// TestCollectRoleStatusesUsesRecordedLayout covers old partitioned workloads whose
// role no longer has sub-roles in the spec, or is gone from it: each old child is
// still required while they drain, as the planner sees them.
func TestCollectRoleStatusesUsesRecordedLayout(t *testing.T) {
	setUp := func(t *testing.T, bReady bool) *subRoleFixture {
		f := newSubRoleFixture(t, 0, "a", "b")
		for _, group := range f.observe().Groups {
			if name, _ := groupSubRole(group); name == "b" && !bReady {
				group.Leader.Status.Conditions = nil
				require.NoError(t, f.manager.client.Status().Update(t.Context(), group.Leader))
			}
		}
		lws := f.observe().LWS
		lws.Status.Replicas, lws.Status.ReadyReplicas = 2, 1
		if bReady {
			lws.Status.ReadyReplicas = 2
		}
		require.NoError(t, f.manager.client.Status().Update(t.Context(), lws))
		return f
	}
	create := func(t *testing.T, f *subRoleFixture, role, revision string, spec, ready int32, initial ...int32) {
		lws := servingLWS(f.ds, role, "0", revision, spec, ready, initial...)
		status := lws.Status
		require.NoError(t, f.manager.client.Create(t.Context(), lws))
		lws.Status = status
		require.NoError(t, f.manager.client.Status().Update(t.Context(), lws))
	}

	t.Run("a parent that dropped its sub-roles needs every old child Ready", func(t *testing.T) {
		f := setUp(t, false)
		f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{withMaxUnavailable(staticRole("model"), intstr.FromInt32(1))}
		create(t, f, "model", "new", 2, 0)
		desired := map[string]int{"model": 2}
		statuses, _, err := f.manager.collectRoleStatuses(t.Context(), f.ds, []string{"model"}, "new", desired)
		require.NoError(t, err)
		assert.EqualValues(t, 1, statuses[0].ReadyReplicas)
		assert.Zero(t, statuses[0].AvailableReplicas, "neither revision can serve")

		setMinAvailableReplicas(f.ds, statuses, desired, 1)
		assert.Equal(t, metav1.ConditionFalse, minimumReplicasAvailableCondition(f.ds, statuses).Status)
	})
	t.Run("a parent that dropped its sub-roles serves with every old child Ready", func(t *testing.T) {
		f := setUp(t, true)
		f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{staticRole("model")}
		create(t, f, "model", "new", 2, 0)
		statuses, _, err := f.manager.collectRoleStatuses(t.Context(), f.ds, []string{"model"}, "new", map[string]int{"model": 2})
		require.NoError(t, err)
		assert.EqualValues(t, 2, statuses[0].AvailableReplicas)
	})
	t.Run("a removed partitioned role still needs every old child Ready", func(t *testing.T) {
		f := setUp(t, false)
		f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{staticRole("prefill")}
		// The fixture's revision label is its workload name.
		create(t, f, "prefill", "model", 1, 1, 1)
		statuses, _, err := f.manager.collectRoleStatuses(t.Context(), f.ds, []string{"prefill"}, "new", map[string]int{"prefill": 1})
		require.NoError(t, err)
		assert.EqualValues(t, 1, statuses[0].ReadyReplicas)
		assert.Zero(t, statuses[0].AvailableReplicas, "the old revision cannot serve without old child b")
	})

	annotate := func(t *testing.T, f *subRoleFixture, annotations map[string]string) {
		lws := f.observe().LWS
		for key, value := range annotations {
			if value == "" {
				delete(lws.Annotations, key)
			} else {
				lws.Annotations[key] = value
			}
		}
		require.NoError(t, f.manager.client.Update(t.Context(), lws))
	}
	partitioned := func(children ...string) []disaggregatedsetv1.DisaggregatedRoleSpec {
		role := disaggregatedsetv1.DisaggregatedRoleSpec{Name: "model"}
		for _, child := range children {
			role.SubRoles = append(role.SubRoles, disaggregatedsetv1.DisaggregatedSubRoleSpec{Name: child})
		}
		return []disaggregatedsetv1.DisaggregatedRoleSpec{role}
	}

	// Sub-roles do not change the revision, so these workloads stay current. The
	// fixture's revision label is its workload name.
	t.Run("dropping sub-roles in place counts the current workload as one role", func(t *testing.T) {
		f := setUp(t, false)
		f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{staticRole("model")}
		statuses, _, err := f.manager.collectRoleStatuses(t.Context(), f.ds, []string{"model"}, "model", map[string]int{"model": 2})
		require.NoError(t, err)
		assert.EqualValues(t, 1, statuses[0].AvailableReplicas)
	})
	t.Run("removing a child in place does not require it on the current revision", func(t *testing.T) {
		f := setUp(t, true)
		f.ds.Spec.Roles = partitioned("a")
		statuses, _, err := f.manager.collectRoleStatuses(t.Context(), f.ds, []string{"model"}, "model", map[string]int{"model": 1, "model/a": 1})
		require.NoError(t, err)
		assert.EqualValues(t, 1, statuses[0].AvailableReplicas)
		assert.EqualValues(t, 1, statuses[0].SubRoleStatuses[0].AvailableReplicas)
	})
	t.Run("gaining sub-roles in place does not require the parent", func(t *testing.T) {
		f := setUp(t, true)
		annotate(t, f, map[string]string{subRoleReplicasAnnotation: "", disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey: ""})
		f.ds.Spec.Roles = partitioned("a", "b")
		statuses, _, err := f.manager.collectRoleStatuses(t.Context(), f.ds, []string{"model"}, "model", map[string]int{"model": 2, "model/a": 1, "model/b": 1})
		require.NoError(t, err)
		assert.EqualValues(t, 2, statuses[0].AvailableReplicas)
	})
	t.Run("a current child with a positive target is still required", func(t *testing.T) {
		f := setUp(t, false)
		f.ds.Spec.Roles = partitioned("a", "b")
		statuses, _, err := f.manager.collectRoleStatuses(t.Context(), f.ds, []string{"model"}, "model", map[string]int{"model": 2, "model/a": 1, "model/b": 1})
		require.NoError(t, err)
		assert.EqualValues(t, 1, statuses[0].ReadyReplicas)
		assert.Zero(t, statuses[0].AvailableReplicas, "child b has no Ready group")
	})

	// Without a valid issued layout, groups cannot be matched to the frozen children.
	for name, issued := range map[string]string{"invalid": "not-json", "missing": ""} {
		for _, revision := range []string{"model", "new"} {
			t.Run(fmt.Sprintf("%s subrole-replicas with initial counts set, revision %s", name, revision), func(t *testing.T) {
				f := setUp(t, false)
				annotate(t, f, map[string]string{subRoleReplicasAnnotation: issued})
				f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{staticRole("model")}
				statuses, _, err := f.manager.collectRoleStatuses(t.Context(), f.ds, []string{"model"}, revision, map[string]int{"model": 2})
				require.NoError(t, err)
				assert.EqualValues(t, 1, statuses[0].AvailableReplicas)
			})
		}
	}
}

func TestMinimumReplicasAvailableCondition(t *testing.T) {
	role := func(name string, floor *int32, available int32, children ...disaggregatedsetv1.SubRoleStatus) disaggregatedsetv1.RoleStatus {
		return disaggregatedsetv1.RoleStatus{Name: name, MinAvailableReplicas: floor, AvailableReplicas: available, SubRoleStatuses: children}
	}
	for _, tc := range []struct {
		name     string
		statuses []disaggregatedsetv1.RoleStatus
		status   metav1.ConditionStatus
		reason   string
		message  string
	}{
		{
			name:     "every role at or above its floor",
			statuses: []disaggregatedsetv1.RoleStatus{role("prefill", ptr.To[int32](1), 1), role("decode", ptr.To[int32](3), 4)},
			status:   metav1.ConditionTrue, reason: "MinimumReplicasAvailable",
		},
		{
			name:     "zero floors are met by zero available replicas",
			statuses: []disaggregatedsetv1.RoleStatus{role("prefill", ptr.To[int32](0), 0)},
			status:   metav1.ConditionTrue, reason: "MinimumReplicasAvailable",
		},
		{
			name: "parents and children below their floors are named",
			statuses: []disaggregatedsetv1.RoleStatus{
				role("decode", ptr.To[int32](3), 2),
				role("model", ptr.To[int32](1), 1, disaggregatedsetv1.SubRoleStatus{Name: "a", MinAvailableReplicas: ptr.To[int32](1)}),
			},
			status: metav1.ConditionFalse, reason: "MinimumReplicasUnavailable",
			message: "Roles below their minimum available replicas (available/minimum): decode (2/3), model/a (0/1)",
		},
		{
			name:     "an unknown floor is not reported as met or missed",
			statuses: []disaggregatedsetv1.RoleStatus{role("prefill", nil, 4), role("decode", ptr.To[int32](3), 3)},
			status:   metav1.ConditionUnknown, reason: "ReplicaTargetsUnresolved",
			message: "Minimum available replicas are unknown for roles: prefill",
		},
		{
			name:     "a known failure is not hidden by an unknown floor",
			statuses: []disaggregatedsetv1.RoleStatus{role("prefill", nil, 4), role("decode", ptr.To[int32](3), 0)},
			status:   metav1.ConditionFalse, reason: "MinimumReplicasUnavailable",
			message: "Roles below their minimum available replicas (available/minimum): decode (0/3); minimum available replicas are unknown for roles: prefill",
		},
		{
			name: "long role lists are capped",
			statuses: []disaggregatedsetv1.RoleStatus{
				role("r00", ptr.To[int32](1), 0), role("r01", ptr.To[int32](1), 0), role("r02", ptr.To[int32](1), 0), role("r03", ptr.To[int32](1), 0),
				role("r04", ptr.To[int32](1), 0), role("r05", ptr.To[int32](1), 0), role("r06", ptr.To[int32](1), 0), role("r07", ptr.To[int32](1), 0),
				role("r08", ptr.To[int32](1), 0), role("r09", ptr.To[int32](1), 0), role("r10", ptr.To[int32](1), 0), role("r11", ptr.To[int32](1), 0),
			},
			status: metav1.ConditionFalse, reason: "MinimumReplicasUnavailable",
			message: "Roles below their minimum available replicas (available/minimum): r00 (0/1), r01 (0/1), r02 (0/1), r03 (0/1), r04 (0/1), r05 (0/1), r06 (0/1), r07 (0/1), r08 (0/1), r09 (0/1) and 2 more",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := &disaggregatedsetv1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{Generation: 7}}
			condition := minimumReplicasAvailableCondition(ds, tc.statuses)
			assert.Equal(t, string(disaggregatedsetv1.DisaggregatedSetMinimumReplicasAvailable), condition.Type)
			assert.Equal(t, tc.status, condition.Status)
			assert.Equal(t, tc.reason, condition.Reason)
			assert.EqualValues(t, 7, condition.ObservedGeneration)
			if tc.message != "" {
				assert.Equal(t, tc.message, condition.Message)
			}
		})
	}
}

// TestUpdateStatusSeparatesMinimumAvailabilityFromCompletion covers the cases in
// kubernetes-sigs/lws#1191: a rollout or scale-up that stays within maxUnavailable
// is not complete, so Available is False, but it still serves at least its floor.
func TestUpdateStatusSeparatesMinimumAvailabilityFromCompletion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		roles     []disaggregatedsetv1.DisaggregatedRoleSpec
		desired   map[string]int
		workloads func(*disaggregatedsetv1.DisaggregatedSet) []*leaderworkersetv1.LeaderWorkerSet
		decode    disaggregatedsetv1.RoleStatus
	}{
		{
			name:    "rollout within maxUnavailable",
			roles:   []disaggregatedsetv1.DisaggregatedRoleSpec{staticRole("prefill"), staticRole("decode")},
			desired: map[string]int{"prefill": 2, "decode": 4},
			workloads: func(ds *disaggregatedsetv1.DisaggregatedSet) []*leaderworkersetv1.LeaderWorkerSet {
				return []*leaderworkersetv1.LeaderWorkerSet{
					servingLWS(ds, "prefill", "0", "old", 1, 1, 2), servingLWS(ds, "decode", "0", "old", 3, 3, 4),
					servingLWS(ds, "prefill", "0", "current", 1, 1), servingLWS(ds, "decode", "0", "current", 1, 0),
				}
			},
			decode: disaggregatedsetv1.RoleStatus{Name: "decode", DesiredReplicas: ptr.To[int32](4), MinAvailableReplicas: ptr.To[int32](3), Replicas: 4, ReadyReplicas: 3, AvailableReplicas: 3, UpdatedReplicas: 1},
		},
		{
			name:    "scale-up within maxUnavailable",
			roles:   []disaggregatedsetv1.DisaggregatedRoleSpec{withMaxUnavailable(staticRole("prefill"), intstr.FromString("50%")), withMaxUnavailable(staticRole("decode"), intstr.FromString("50%"))},
			desired: map[string]int{"prefill": 2, "decode": 6},
			workloads: func(ds *disaggregatedsetv1.DisaggregatedSet) []*leaderworkersetv1.LeaderWorkerSet {
				return []*leaderworkersetv1.LeaderWorkerSet{servingLWS(ds, "prefill", "0", "current", 2, 2), servingLWS(ds, "decode", "0", "current", 6, 4)}
			},
			decode: disaggregatedsetv1.RoleStatus{Name: "decode", DesiredReplicas: ptr.To[int32](6), MinAvailableReplicas: ptr.To[int32](3), Replicas: 6, ReadyReplicas: 4, AvailableReplicas: 4, UpdatedReplicas: 6},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := newDSWithRoles("ds", tc.roles...)
			workloads := tc.workloads(ds)
			objects := []client.Object{ds}
			for _, lws := range workloads {
				objects = append(objects, lws)
			}
			c := newTestClient(objects...)
			key := client.ObjectKeyFromObject(ds)
			require.NoError(t, c.Get(t.Context(), key, ds))
			r := newTestReconciler(c)
			require.NoError(t, r.updateStatus(t.Context(), ds, []string{"prefill", "decode"}, "current", tc.desired, nil))
			require.NoError(t, c.Get(t.Context(), key, ds))
			require.Equal(t, tc.decode, ds.Status.RoleStatuses[1])
			require.False(t, meta.IsStatusConditionTrue(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable)), "the change has not completed")
			require.True(t, meta.IsStatusConditionTrue(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetMinimumReplicasAvailable)), "every role still serves at least its floor")

			// Two serving decode replicas remain, one below the floor.
			decode := workloads[1]
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(decode), decode))
			decode.Status.ReadyReplicas = 2
			require.NoError(t, c.Status().Update(t.Context(), decode))
			require.NoError(t, r.updateStatus(t.Context(), ds, []string{"prefill", "decode"}, "current", tc.desired, nil))
			require.NoError(t, c.Get(t.Context(), key, ds))
			condition := meta.FindStatusCondition(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetMinimumReplicasAvailable))
			require.NotNil(t, condition)
			require.Equal(t, metav1.ConditionFalse, condition.Status)
			require.Contains(t, condition.Message, "decode (2/3)")
		})
	}
}
