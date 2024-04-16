/*
Copyright 2023 The Kubernetes Authors.
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
package testutils

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/utils"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

const (
	Timeout  = 30 * time.Second
	Interval = time.Millisecond * 250
)

func ExpectValidReplicasCount(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, count int, k8sClient client.Client) {
	gomega.Eventually(func() (int, error) {
		var leaderStatefulSet appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderStatefulSet); err != nil {
			return -1, err
		}
		return int(*leaderStatefulSet.Spec.Replicas), nil
	}).Should(gomega.Equal(count))
}

func ExpectLeaderSetExist(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, k8sClient client.Client) {
	gomega.Eventually(func() bool {
		var leaderSet appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSet); err != nil {
			return false
		}
		return true
	}, Timeout, Interval).Should(gomega.Equal(true))
}

func ExpectValidServices(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet) {
	gomega.Eventually(func() (bool, error) {
		var headlessService corev1.Service
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &headlessService); err != nil {
			return false, err
		}
		// we expect exactly one service and it must be of the correct type.
		if headlessService.ObjectMeta.Name != lws.Name {
			return false, errors.New("service name mismatch")
		}
		if headlessService.ObjectMeta.Namespace != lws.Namespace {
			return false, errors.New("service namespace mismatch")
		}
		if headlessService.Spec.ClusterIP != "None" {
			return false, errors.New("service type mismatch")
		}
		if headlessService.Spec.PublishNotReadyAddresses != true {
			return false, errors.New("service publish not ready should be true")
		}
		selector := headlessService.Spec.Selector
		value, exists := selector["leaderworkerset.sigs.k8s.io/name"]
		if !exists || value != lws.Name {
			return false, errors.New("selector name incorrect")
		}
		return true, nil
	}, Timeout, Interval).Should(gomega.Equal(true))
}

func ExpectValidLeaderStatefulSet(ctx context.Context, leaderWorkerSet *leaderworkerset.LeaderWorkerSet, k8sClient client.Client) {
	gomega.Eventually(func() error {
		// Always got the latest lws.
		var lws leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: leaderWorkerSet.Namespace}, &lws); err != nil {
			return err
		}
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &sts); err != nil {
			return err
		}

		// check labels and annotations
		if sts.Labels[leaderworkerset.SetNameLabelKey] == "" {
			return errors.New("leader StatefulSet should have label leaderworkerset.sigs.k8s.io/name")
		}
		if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != sts.Spec.Template.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] {
			return fmt.Errorf("mismatch exclusive placement annotation between leader statefulset and leaderworkerset")
		}
		sizeAnnotation := sts.Spec.Template.Annotations[leaderworkerset.SizeAnnotationKey]
		if sizeAnnotation == "" {
			return fmt.Errorf("leader statefuSet pod template misses worker replicas annotation")
		}
		if size, err := strconv.Atoi(sizeAnnotation); err != nil || size != int(*lws.Spec.LeaderWorkerTemplate.Size) {
			return fmt.Errorf("error parsing size annotation or size mismatch for value %s", sizeAnnotation)
		}
		if sts.Spec.Template.Labels[leaderworkerset.WorkerIndexLabelKey] != "0" {
			return fmt.Errorf("leader statefulset pod template misses worker index label")
		}
		if sts.Spec.Template.Labels[leaderworkerset.SetNameLabelKey] == "" {
			return fmt.Errorf("leader statefulset pod template misses leaderworkerset label")
		}
		hash := utils.LeaderWorkerTemplateHash(&lws)
		if sts.Labels[leaderworkerset.TemplateRevisionHashKey] != hash {
			return fmt.Errorf("mismatch template revision hash for leader statefulset, got: %s, want: %s", sts.Spec.Template.Labels[leaderworkerset.TemplateRevisionHashKey], hash)
		}
		if sts.Spec.ServiceName != lws.Name {
			return errors.New("leader StatefulSet service name should match leaderWorkerSet name")
		}
		if *sts.Spec.Replicas != *lws.Spec.Replicas {
			return errors.New("leader StatefulSet replicas should match leaderWorkerSet replicas")
		}
		if sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement {
			return errors.New("leader StatefulSet should use parallel pod management")
		}
		if diff := cmp.Diff(*sts.Spec.Selector, metav1.LabelSelector{
			MatchLabels: map[string]string{
				leaderworkerset.SetNameLabelKey:     lws.Name,
				leaderworkerset.WorkerIndexLabelKey: "0",
			},
		}); diff != "" {
			return errors.New("leader StatefulSet doesn't have the correct labels: " + diff)
		}
		var podTemplateSpec corev1.PodTemplateSpec
		// if leader template is nil, use worker template
		if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
			podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.LeaderTemplate.DeepCopy()
		} else {
			podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
		}
		// check pod template has correct label
		if diff := cmp.Diff(sts.Spec.Template.Labels, map[string]string{
			leaderworkerset.SetNameLabelKey:         lws.Name,
			leaderworkerset.WorkerIndexLabelKey:     "0",
			leaderworkerset.TemplateRevisionHashKey: utils.LeaderWorkerTemplateHash(&lws),
		}); diff != "" {
			return errors.New("leader StatefulSet pod template doesn't have the correct labels: " + diff)
		}
		// we can't do a full diff of the pod template since there will be default fields added to pod template
		if podTemplateSpec.Spec.Containers[0].Name != sts.Spec.Template.Spec.Containers[0].Name {
			return errors.New("pod template is not updated, expect " + podTemplateSpec.Spec.Containers[0].Name + ", got " + sts.Spec.Template.Spec.Containers[0].Name)
		}
		return nil
	}, Timeout, Interval).Should(gomega.Succeed())
}

func ExpectValidWorkerStatefulSets(ctx context.Context, leaderWorkerSet *leaderworkerset.LeaderWorkerSet, k8sClient client.Client, leaderPodScheduled bool) {
	gomega.Eventually(func() error {
		// Always got the latest lws.
		var lws leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: leaderWorkerSet.Namespace}, &lws); err != nil {
			return err
		}

		var statefulSetList appsv1.StatefulSetList
		if err := k8sClient.List(ctx, &statefulSetList, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
			return err
		}

		var podList corev1.PodList
		if err := k8sClient.List(ctx, &podList, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
			return err
		}
		if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" && !leaderPodScheduled && len(statefulSetList.Items) != 1 {
			return fmt.Errorf("when exclusive placement is enabled, only expect sts count to be 1")
		}
		if len(statefulSetList.Items) != int(*lws.Spec.Replicas)+1 && lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] == "" {
			return fmt.Errorf("incorrect statefulSets count, got %d, expect %d", len(statefulSetList.Items), int(*lws.Spec.Replicas)+1)
		}
		for _, sts := range statefulSetList.Items {
			// Skip the leader sts
			if sts.Name == lws.Name {
				continue
			}
			// verify statefulset labels
			if sts.Labels[leaderworkerset.SetNameLabelKey] == "" {
				return errors.New("worker StatefulSet should have label leaderworkerset.sigs.k8s.io/name")
			}
			groupIndexLabel := sts.Labels[leaderworkerset.GroupIndexLabelKey]
			if groupIndexLabel == "" {
				return fmt.Errorf("worker statefulset should have label leaderworkerset.sigs.k8s.io/group-index")
			}
			if _, groupIndex := statefulsetutils.GetParentNameAndOrdinal(sts.Name); groupIndexLabel != strconv.Itoa(groupIndex) {
				return fmt.Errorf("group index label mismatch for worker statefulset %s", sts.Name)
			}
			if sts.Labels[leaderworkerset.GroupUniqueHashLabelKey] == "" {
				return fmt.Errorf("missing group unique hash label for worker statefulset %s", sts.Name)
			}
			// verify pod template labels
			if sts.Spec.Template.Labels[leaderworkerset.SetNameLabelKey] == "" {
				return fmt.Errorf("worker statefulset pod template misses leaderworkerset label")
			}
			groupIndexLabel = sts.Spec.Template.Labels[leaderworkerset.GroupIndexLabelKey]
			if groupIndexLabel == "" {
				return fmt.Errorf("worker statefulset should have label leaderworkerset.sigs.k8s.io/group-index")
			}
			if _, groupIndex := statefulsetutils.GetParentNameAndOrdinal(sts.Name); groupIndexLabel != strconv.Itoa(groupIndex) {
				return fmt.Errorf("group index label mismatch for worker statefulset %s", sts.Name)
			}
			if sts.Labels[leaderworkerset.GroupUniqueHashLabelKey] == "" {
				return fmt.Errorf("missing group unique hash label for worker statefulset %s", sts.Name)
			}
			// verify pod annotations
			sizeAnnotation := sts.Spec.Template.Annotations[leaderworkerset.SizeAnnotationKey]
			if sizeAnnotation == "" {
				return fmt.Errorf("worker statefuSet pod template misses worker replicas annotation")
			}
			if size, err := strconv.Atoi(sizeAnnotation); err != nil || size != int(*lws.Spec.LeaderWorkerTemplate.Size) {
				return fmt.Errorf("error parsing size annotation or size mismatch for value %s", sizeAnnotation)
			}
			if sts.Spec.Template.Annotations[leaderworkerset.LeaderPodNameAnnotationKey] != sts.Name {
				return fmt.Errorf("worker statefulset pod template misses leader pod name annotation")
			}
			if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != sts.Spec.Template.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] {
				return fmt.Errorf("mismatch exclusive placement annotation between worker statefulset and leaderworkerset")
			}
			hash := utils.LeaderWorkerTemplateHash(&lws)
			if sts.Labels[leaderworkerset.TemplateRevisionHashKey] != hash {
				return fmt.Errorf("mismatch template revision hash for worker statefulset, got: %s, want: %s", sts.Labels[leaderworkerset.TemplateRevisionHashKey], hash)
			}
			if sts.Spec.ServiceName != lws.Name {
				return errors.New("worker StatefulSet service name should match leaderWorkerSet name")
			}
			if *sts.Spec.Replicas != *lws.Spec.LeaderWorkerTemplate.Size-1 {
				return errors.New("worker StatefulSet replicas should match leaderWorkerSet replicas")
			}
			if sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement {
				return errors.New("worker StatefulSet should use parallel pod management")
			}
			if diff := cmp.Diff(*sts.Spec.Ordinals, appsv1.StatefulSetOrdinals{Start: 1}); diff != "" {
				return errors.New("worker StatefulSet should have start ordinal as 1")
			}
			podTemplateSpec := *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
			// we can't do a full diff of the pod template since there will be default fields added to pod template
			if podTemplateSpec.Spec.Containers[0].Name != sts.Spec.Template.Spec.Containers[0].Name {
				return errors.New("pod template is not updated, expect " + podTemplateSpec.Spec.Containers[0].Name + ", got " + sts.Spec.Template.Spec.Containers[0].Name)
			}
		}
		return nil
	}, Timeout, Interval).Should(gomega.Succeed())
}

func ExpectLeaderWorkerSetProgressing(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, message string) {
	ginkgo.By(fmt.Sprintf("checking leaderworkerset status(%s) is true", leaderworkerset.LeaderWorkerSetProgressing))
	condition := metav1.Condition{
		Type:    string(leaderworkerset.LeaderWorkerSetProgressing),
		Status:  metav1.ConditionTrue,
		Message: message,
	}
	gomega.Eventually(CheckLeaderWorkerSetHasCondition, Timeout, Interval).WithArguments(ctx, k8sClient, lws, condition).Should(gomega.Equal(true))
}

func ExpectLeaderWorkerSetNotProgressing(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, message string) {
	ginkgo.By(fmt.Sprintf("checking leaderworkerset status(%s) is false", leaderworkerset.LeaderWorkerSetProgressing))
	condition := metav1.Condition{
		Type:    string(leaderworkerset.LeaderWorkerSetProgressing),
		Status:  metav1.ConditionFalse,
		Message: message,
	}
	gomega.Eventually(CheckLeaderWorkerSetHasCondition, Timeout, Interval).WithArguments(ctx, k8sClient, lws, condition).Should(gomega.Equal(true))
}

func ExpectLeaderWorkerSetStatusReplicas(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, readyReplicas, updatedReplicas int) {
	ginkgo.By("checking leaderworkerset status replicas")
	gomega.Eventually(func() error {
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: lws.Name}, lws); err != nil {
			return err
		}
		if lws.Status.ReadyReplicas != int32(readyReplicas) {
			return fmt.Errorf("readyReplicas in status not match, want: %d, got %d", readyReplicas, lws.Status.ReadyReplicas)
		}
		if lws.Status.UpdatedReplicas != int32(updatedReplicas) {
			return fmt.Errorf("updatedReplicas in status not match, want: %d, got %d", updatedReplicas, lws.Status.UpdatedReplicas)
		}
		return nil
	}, Timeout, Interval).Should(gomega.Succeed())
}

func ExpectLeaderWorkerSetAvailable(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, message string) {
	ginkgo.By(fmt.Sprintf("checking leaderworkerset status(%s) is true", leaderworkerset.LeaderWorkerSetAvailable))
	condition := metav1.Condition{
		Type:    string(leaderworkerset.LeaderWorkerSetAvailable),
		Status:  metav1.ConditionTrue,
		Message: message,
	}
	gomega.Eventually(CheckLeaderWorkerSetHasCondition, Timeout, Interval).WithArguments(ctx, k8sClient, lws, condition).Should(gomega.Equal(true))
}

func ExpectStatefulsetPartitionEqualTo(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, partition int32) {
	ginkgo.By("checking statefulset partition")
	gomega.Eventually(func() bool {
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &sts); err != nil {
			return false
		}
		return *sts.Spec.UpdateStrategy.RollingUpdate.Partition == partition
	}, Timeout, Interval).Should(gomega.Equal(true))
}

func ExpectLeaderWorkerSetUnavailable(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, message string) {
	ginkgo.By(fmt.Sprintf("checking leaderworkerset status(%s) is false", leaderworkerset.LeaderWorkerSetAvailable))
	condition := metav1.Condition{
		Type:    string(leaderworkerset.LeaderWorkerSetAvailable),
		Status:  metav1.ConditionFalse,
		Message: message,
	}
	gomega.Eventually(CheckLeaderWorkerSetHasCondition, Timeout, Interval).WithArguments(ctx, k8sClient, lws, condition).Should(gomega.Equal(true))
}

// ValidateLatestEvent will return true if the latest event is as you want.
func ValidateLatestEvent(ctx context.Context, k8sClient client.Client, eventReason string, eventType string, eventNote string, namespace string) {
	gomega.Eventually(func() error {
		events := &eventsv1.EventList{}
		if err := k8sClient.List(ctx, events, &client.ListOptions{Namespace: namespace}); err != nil {
			return err
		}

		length := len(events.Items)
		if length == 0 {
			return fmt.Errorf("no events currently exist")
		}

		item := events.Items[length-1]
		if item.Reason == eventReason && item.Type == eventType && item.Note == eventNote {
			return nil
		}

		return fmt.Errorf("mismatch with the latest event: got r:%v t:%v n:%v, reg %v", item.Reason, item.Type, item.Note, item.Regarding)

	}, Timeout, Interval).Should(gomega.BeNil())
}
