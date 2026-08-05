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

package disaggregatedset

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestAssignmentManagerReconcile(t *testing.T) {
	objects := []client.Object{
		assignmentPod("lws-0", 0, 0, "short"),
		assignmentPod("lws-0", 0, 1, "short"),
		assignmentPod("lws-0", 1, 0, "invalid"),
		assignmentPod("lws-0", 1, 1, "invalid"),
		assignmentPod("lws-0", 2, 0, "long"),
		assignmentPod("lws-0", 2, 1, "long"),
	}
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(objects...).Build()
	manager := NewAssignmentManager(cl)

	changed, summary, err := manager.Reconcile(context.Background(), "default", "lws-0", []string{"short", "long"}, map[string]int{"short": 2, "long": 1})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, map[string]int{"short": 2, "long": 1}, summary.Replicas)
	assert.Equal(t, 0, summary.Unassigned)

	for _, worker := range []int{0, 1} {
		pod := &corev1.Pod{}
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("lws-0-1-%d", worker)}, pod))
		assert.Equal(t, "short", pod.Labels[disaggregatedsetv1.SubRoleLabelKey])
	}

	changed, _, err = manager.Reconcile(context.Background(), "default", "lws-0", []string{"short", "long"}, map[string]int{"short": 2, "long": 1})
	require.NoError(t, err)
	assert.False(t, changed, "converged assignments must not cause writes")
}

func TestAssignmentManagerPrepareScaleDown(t *testing.T) {
	objects := []client.Object{
		assignmentPod("lws-0", 0, 0, "short"),
		assignmentPod("lws-0", 1, 0, "short"),
		assignmentPod("lws-0", 2, 0, "long"),
	}
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(objects...).Build()
	manager := NewAssignmentManager(cl)

	changed, err := manager.PrepareScaleDown(context.Background(), "default", "lws-0", []string{"short", "long"}, map[string]int{"short": 1, "long": 1})
	require.NoError(t, err)
	assert.True(t, changed)

	want := map[int]string{0: "short", 1: "long", 2: "short"}
	for index, subRole := range want {
		pod := &corev1.Pod{}
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("lws-0-%d-0", index)}, pod))
		assert.Equal(t, subRole, pod.Labels[disaggregatedsetv1.SubRoleLabelKey])
	}
}

func TestAssignmentManagerMirrorsLeaderAndRemovesDisabledLabels(t *testing.T) {
	leader := assignmentPod("lws-0", 0, 0, "short")
	worker := assignmentPod("lws-0", 0, 1, "long")
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(leader, worker).Build()
	manager := NewAssignmentManager(cl)

	changed, _, err := manager.Reconcile(context.Background(), "default", "lws-0", []string{"short", "long"}, map[string]int{"short": 1})
	require.NoError(t, err)
	assert.True(t, changed)
	gotWorker := &corev1.Pod{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: worker.Name}, gotWorker))
	assert.Equal(t, "short", gotWorker.Labels[disaggregatedsetv1.SubRoleLabelKey], "the leader assignment is authoritative for its group")

	changed, summary, err := manager.Reconcile(context.Background(), "default", "lws-0", nil, nil)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, summary.Unassigned)
	for _, name := range []string{leader.Name, worker.Name} {
		pod := &corev1.Pod{}
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, pod))
		assert.NotContains(t, pod.Labels, disaggregatedsetv1.SubRoleLabelKey)
	}
}

func TestFitSubRoleTargets(t *testing.T) {
	assert.Equal(t, map[string]int{"short": 3}, fitSubRoleTargets([]string{"short", "long"}, map[string]int{"short": 5, "long": 2}, 3))
	assert.Equal(t, map[string]int{"short": 1, "long": 1}, fitSubRoleTargets([]string{"short", "long"}, map[string]int{"short": 1, "long": 1}, 2))
}

func TestAssignmentManagerRejectsNonContiguousScaleDownOrdinals(t *testing.T) {
	objects := []client.Object{
		assignmentPod("lws-0", 0, 0, "short"),
		assignmentPod("lws-0", 2, 0, "long"),
		assignmentPod("lws-0", 3, 0, "short"),
	}
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(objects...).Build()
	manager := NewAssignmentManager(cl)

	_, err := manager.PrepareScaleDown(context.Background(), "default", "lws-0", []string{"short", "long"}, map[string]int{"short": 1, "long": 1})
	require.ErrorContains(t, err, "expected group ordinal 1, observed 2")
}

func assignmentPod(lws string, group, worker int, subRole string) *corev1.Pod {
	labels := map[string]string{
		leaderworkersetv1.SetNameLabelKey:     lws,
		leaderworkersetv1.GroupIndexLabelKey:  fmt.Sprint(group),
		leaderworkersetv1.WorkerIndexLabelKey: fmt.Sprint(worker),
	}
	if subRole != "" {
		labels[disaggregatedsetv1.SubRoleLabelKey] = subRole
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d-%d", lws, group, worker), Namespace: "default", Labels: labels},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}
