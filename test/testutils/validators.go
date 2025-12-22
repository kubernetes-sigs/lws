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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

const (
	Timeout  = 30 * time.Second
	Interval = time.Millisecond * 250
)

func ExpectLeaderSetExist(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, k8sClient client.Client) {
	gomega.Eventually(func() bool {
		var leaderSet appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSet); err != nil {
			return false
		}
		return true
	}, Timeout, Interval).Should(gomega.Equal(true))
}

func ExpectValidServices(ctx context.Context, k8sClient client.Client, leaderWorkerSet *leaderworkerset.LeaderWorkerSet, numServices int) {
	gomega.Eventually(func() (bool, error) {

		// Always got the latest lws.
		var lws leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: leaderWorkerSet.Namespace}, &lws); err != nil {
			return false, err
		}

		var service corev1.Service
		var serviceList corev1.ServiceList
		if err := k8sClient.List(ctx, &serviceList, client.InNamespace(lws.Namespace)); err != nil {
			return false, err
		}

		if len(serviceList.Items) != (numServices) {
			return false, fmt.Errorf("expected %d headless services, got %d", numServices, len(serviceList.Items))
		}

		if *lws.Spec.NetworkConfig.SubdomainPolicy == leaderworkerset.SubdomainShared {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &service); err != nil {
				return false, err
			}
			return validateService(service, lws.Name, map[string]string{leaderworkerset.SetNameLabelKey: lws.Name}, true)
		}

		if len(lws.Spec.NetworkConfig.LeaderServicePorts) > 0 {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + leaderworkerset.LeaderServicePostfix, Namespace: lws.Namespace}, &service); err != nil {
				return false, err
			}
			return validateService(service, lws.Name+leaderworkerset.LeaderServicePostfix, map[string]string{leaderworkerset.SetNameLabelKey: lws.Name, leaderworkerset.WorkerIndexLabelKey: "0"}, false)
		}

		for i := 0; i < int(*lws.Spec.Replicas); i++ {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s-%s", lws.Name, strconv.Itoa(i)), Namespace: lws.Namespace}, &service); err != nil {
				return false, err
			}
			if _, err := validateService(service, fmt.Sprintf("%s-%s", lws.Name, strconv.Itoa(i)), map[string]string{leaderworkerset.SetNameLabelKey: lws.Name, leaderworkerset.GroupIndexLabelKey: strconv.Itoa(i)}, true); err != nil {
				return false, err
			}
		}
		return true, nil
	}, Timeout, Interval).Should(gomega.Equal(true))
}

func validateService(headlessService corev1.Service, serviceName string, wantSelector map[string]string, headless bool) (bool, error) {
	if headless && headlessService.Spec.ClusterIP != "None" {
		return false, errors.New("service type mismatch")
	} else if !headless && headlessService.Spec.Type != corev1.ServiceTypeClusterIP {
		return false, errors.New("service type mismatch")
	}
	if !headlessService.Spec.PublishNotReadyAddresses {
		return false, errors.New("service publish not ready should be true")
	}
	if headlessService.OwnerReferences[0].Name != serviceName {
		return false, fmt.Errorf("service name is %s, expected %s", headlessService.OwnerReferences[0].Name, serviceName)
	}
	selector := headlessService.Spec.Selector
	if diff := cmp.Diff(selector, wantSelector); diff != "" {
		return false, errors.New("service does not have the correct selectors: " + diff)
	}

	return true, nil
}

func ExpectValidLeaderStatefulSet(ctx context.Context, k8sClient client.Client, leaderWorkerSet *leaderworkerset.LeaderWorkerSet, replicas int32) {
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
		if lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] != sts.Spec.Template.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] {
			return fmt.Errorf("mismatch subgroup exclusive placement annotation between leader statefulset and leaderworkerset")
		}
		sizeAnnotation := sts.Spec.Template.Annotations[leaderworkerset.SizeAnnotationKey]
		if sizeAnnotation == "" {
			return fmt.Errorf("leader statefulSet pod template misses worker replicas annotation")
		}
		if size, err := strconv.Atoi(sizeAnnotation); err != nil || size != int(*lws.Spec.LeaderWorkerTemplate.Size) {
			return fmt.Errorf("error parsing size annotation or size mismatch for value %s", sizeAnnotation)
		}
		replicasAnnotation := sts.Annotations[leaderworkerset.ReplicasAnnotationKey]
		if replicasAnnotation == "" {
			return fmt.Errorf("leader statefulSet misses replicas annotation")
		}
		if replicas, err := strconv.Atoi(replicasAnnotation); err != nil || replicas != int(*lws.Spec.Replicas) {
			return fmt.Errorf("error parsing replicas annotation or replicas mismatch for value %s", replicasAnnotation)
		}
		if sts.Spec.Template.Labels[leaderworkerset.WorkerIndexLabelKey] != "0" {
			return fmt.Errorf("leader statefulset pod template misses worker index label")
		}
		if sts.Spec.Template.Labels[leaderworkerset.SetNameLabelKey] == "" {
			return fmt.Errorf("leader statefulset pod template misses leaderworkerset label")
		}
		cr, err := revisionutils.NewRevision(ctx, k8sClient, &lws, "")
		if err != nil {
			return err
		}
		hash := revisionutils.GetRevisionKey(cr)
		if revisionutils.GetRevisionKey(&sts) != hash {
			return fmt.Errorf("mismatch template revision hash for leader statefulset, got: %s, want: %s", revisionutils.GetRevisionKey(&sts), hash)
		}
		if sts.Spec.ServiceName != lws.Name {
			return errors.New("leader StatefulSet service name should match leaderWorkerSet name")
		}
		if *sts.Spec.Replicas != replicas {
			return fmt.Errorf("leader StatefulSet replicas should match with specified replicas, want %d, got %d", replicas, *sts.Spec.Replicas)
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
			leaderworkerset.SetNameLabelKey:     lws.Name,
			leaderworkerset.WorkerIndexLabelKey: "0",
			leaderworkerset.RevisionKey:         hash,
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

		var leaderSts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: leaderWorkerSet.Namespace}, &leaderSts); err != nil {
			return err
		}
		var statefulSetList appsv1.StatefulSetList
		if err := k8sClient.List(ctx, &statefulSetList, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
			return err
		}
		stsNumber := *leaderSts.Spec.Replicas
		if *lws.Spec.LeaderWorkerTemplate.Size == 1 {
			stsNumber = 0
		}
		if leaderPodScheduled && len(statefulSetList.Items)-1 != int(stsNumber) {
			return fmt.Errorf("running worker statefulsets replicas not right, want %d, got %d", len(statefulSetList.Items)-1, stsNumber)
		}
		if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" && !leaderPodScheduled && len(statefulSetList.Items) != 1 {
			return fmt.Errorf("when exclusive placement is enabled, only expect sts count to be 1")
		}
		var podList corev1.PodList
		if err := k8sClient.List(ctx, &podList, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
			return err
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
			cr, err := revisionutils.NewRevision(ctx, k8sClient, &lws, "")
			if err != nil {
				return err
			}
			hash := revisionutils.GetRevisionKey(cr)
			if revisionutils.GetRevisionKey(&sts) != hash {
				return fmt.Errorf("mismatch template revision hash for worker statefulset, got: %s, want: %s", revisionutils.GetRevisionKey(&sts), hash)
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

// Expect that the revisionKey and the container name in the Worker Sts have been updated
func ExpectUpdatedWorkerStatefulSet(ctx context.Context, k8sClient client.Client, leaderWorkerSet *leaderworkerset.LeaderWorkerSet, statefulsetName string) {
	gomega.Eventually(func() error {
		var lws leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: leaderWorkerSet.Namespace}, &lws); err != nil {
			return err
		}
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: statefulsetName, Namespace: lws.Namespace}, &sts); err != nil {
			return err
		}
		revision, err := revisionutils.NewRevision(ctx, k8sClient, &lws, "")
		if err != nil {
			return err
		}
		if revisionutils.GetRevisionKey(&sts) != revisionutils.GetRevisionKey(revision) {
			return errors.New("workerStatefulSet doesn't have the correct revisionKey")
		}
		podTemplateSpec := *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
		if podTemplateSpec.Spec.Containers[0].Name != sts.Spec.Template.Spec.Containers[0].Name {
			return errors.New("pod template is not updated")
		}
		return nil
	}, Timeout, Interval).Should(gomega.Succeed())
}

// Expect that the revisionKey and the container name in the Worker Sts have not been updated
func ExpectNotUpdatedWorkerStatefulSet(ctx context.Context, k8sClient client.Client, leaderWorkerSet *leaderworkerset.LeaderWorkerSet, statefulsetName string) {
	gomega.Eventually(func() error {
		var lws leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: leaderWorkerSet.Namespace}, &lws); err != nil {
			return err
		}
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: statefulsetName, Namespace: lws.Namespace}, &sts); err != nil {
			return err
		}
		revision, err := revisionutils.NewRevision(ctx, k8sClient, &lws, "")
		if err != nil {
			return err
		}
		if revisionutils.GetRevisionKey(&sts) == revisionutils.GetRevisionKey(revision) {
			return errors.New("workerStatefulSet has an updated revisionKey")
		}
		podTemplateSpec := *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
		if podTemplateSpec.Spec.Containers[0].Name == sts.Spec.Template.Spec.Containers[0].Name {
			return errors.New("pod template is updated")
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

func ExpectLeaderWorkerSetUpgradeInProgress(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, message string) {
	ginkgo.By(fmt.Sprintf("checking leaderworkerset status(%s) is true", leaderworkerset.LeaderWorkerSetUpdateInProgress))
	condition := metav1.Condition{
		Type:    string(leaderworkerset.LeaderWorkerSetUpdateInProgress),
		Status:  metav1.ConditionTrue,
		Message: message,
	}
	gomega.Eventually(CheckLeaderWorkerSetHasCondition, Timeout, Interval).WithArguments(ctx, k8sClient, lws, condition).Should(gomega.Equal(true))
}

func ExpectLeaderWorkerSetNoUpgradeInProgress(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, message string) {
	ginkgo.By(fmt.Sprintf("checking leaderworkerset status(%s) is true", leaderworkerset.LeaderWorkerSetUpdateInProgress))
	condition := metav1.Condition{
		Type:    string(leaderworkerset.LeaderWorkerSetUpdateInProgress),
		Status:  metav1.ConditionFalse,
		Message: message,
	}
	gomega.Eventually(CheckLeaderWorkerSetHasCondition, Timeout, Interval).WithArguments(ctx, k8sClient, lws, condition).Should(gomega.Equal(true))
}

func ExpectLeaderWorkerSetNotExist(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, k8sClient client.Client) {
	gomega.Eventually(func() bool {
		var leaderSet leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSet); err != nil {
			return apierrors.IsNotFound(err)
		}
		return false
	}, Timeout, Interval).Should(gomega.Equal(true))
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
	gomega.Eventually(func() int32 {
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &sts); err != nil {
			return -1
		}
		return *sts.Spec.UpdateStrategy.RollingUpdate.Partition
	}, Timeout, Interval).Should(gomega.Equal(partition))
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
func ValidateEvent(ctx context.Context, k8sClient client.Client, eventReason string, eventType string, eventNote string, namespace string) {
	gomega.Eventually(func() error {
		events := &eventsv1.EventList{}
		if err := k8sClient.List(ctx, events, &client.ListOptions{Namespace: namespace}); err != nil {
			return err
		}

		length := len(events.Items)
		if length == 0 {
			return fmt.Errorf("no events currently exist")
		}

		for _, item := range events.Items {
			if item.Reason == eventReason && item.Type == eventType && item.Note == eventNote {
				return nil
			}
		}

		return fmt.Errorf("mismatch with the expected event: expected r:%v t:%v n:%v", eventReason, eventType, eventNote)

	}, Timeout, Interval).Should(gomega.BeNil())
}

func ExpectWorkerStatefulSetsNotCreated(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet) {
	ginkgo.By("checking no worker statefulset created")
	gomega.Consistently(func() bool {
		var statefulSetList appsv1.StatefulSetList
		if err := k8sClient.List(ctx, &statefulSetList, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
			return false
		}
		return len(statefulSetList.Items) == 1 && statefulSetList.Items[0].Name == lws.Name
	}, Timeout, Interval).Should(gomega.Equal(true))
}

func ExpectSpecifiedWorkerStatefulSetsCreated(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, start, end int) {
	gomega.Eventually(func() error {
		var sts appsv1.StatefulSet
		for i := start; i < end; i++ {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s-%d", lws.Name, i), Namespace: lws.Namespace}, &sts); err != nil {
				return err
			}
		}
		return nil
	}, Timeout, Interval).Should(gomega.BeNil())
}

func ExpectSpecifiedWorkerStatefulSetsNotCreated(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, start, end int) {
	gomega.Consistently(func() bool {
		var sts appsv1.StatefulSet
		for i := start; i < end; i++ {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s-%d", lws.Name, i), Namespace: lws.Namespace}, &sts); err == nil ||
				!apierrors.IsNotFound(err) {
				return false
			}
		}
		return true
	}, Timeout, Interval).Should(gomega.Equal(true))
}

func ExpectRevisions(ctx context.Context, k8sClient client.Client, leaderWorkerSet *leaderworkerset.LeaderWorkerSet, numRevisions int) {
	gomega.Eventually(func() error {
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{
			leaderworkerset.SetNameLabelKey: leaderWorkerSet.Name,
		}})
		if err != nil {
			return err
		}
		revisions, err := revisionutils.ListRevisions(ctx, k8sClient, leaderWorkerSet, selector)
		if err != nil {
			return err
		}
		if len(revisions) != numRevisions {
			return fmt.Errorf("expected %d revisions, got %d instead", numRevisions, len(revisions))
		}

		currentRevision, err := revisionutils.NewRevision(ctx, k8sClient, leaderWorkerSet, "")
		if err != nil {
			return err
		}

		var leaderSts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: leaderWorkerSet.Namespace}, &leaderSts); err != nil {
			return err
		}
		foundRevisionMatch := false
		for _, revision := range revisions {
			if revisionutils.GetRevisionKey(revision) == revisionutils.GetRevisionKey(&leaderSts) && revisionutils.EqualRevision(currentRevision, revision) {
				foundRevisionMatch = true
			}
		}

		if !foundRevisionMatch {
			return fmt.Errorf("no revision matches the current state of lws")
		}

		return nil
	}, Timeout, Interval).Should(gomega.Succeed())
}
