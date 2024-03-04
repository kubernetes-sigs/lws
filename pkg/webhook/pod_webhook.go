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

package webhook

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	acceleratorutils "sigs.k8s.io/lws/pkg/utils/accelerators"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

type PodWebhook struct {
	client client.Client
	//decoder *admission.Decoder
}

func NewPodWebhook(mgr ctrl.Manager) *PodWebhook {
	return &PodWebhook{client: mgr.GetClient()}
}

func (p *PodWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithDefaulter(p).
		WithValidator(p).
		Complete()
}

//+kubebuilder:webhook:path=/validate--v1-pod,mutating=false,failurePolicy=fail,sideEffects=None,groups="",resources=pods,verbs=create;update,versions=v1,name=vpod.kb.io,sideEffects=None,admissionReviewVersions=v1

// validate admits a pod if a specific annotation exists.
func (p *PodWebhook) validate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	log := logf.FromContext(ctx)
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("expected a Pod but got a %T", obj)
	}

	log.V(2).Info("Validating Pod")

	// if pod is not part of leaderworkerset, skip
	_, found := pod.Labels[leaderworkerset.SetNameLabelKey]
	if !found {
		return nil, nil
	}

	return nil, nil
}

func (p *PodWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return p.validate(ctx, obj)
}

func (p *PodWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (p *PodWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

//+kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=fail,groups="",resources=pods,verbs=create;update,versions=v1,name=mpod.kb.io,sideEffects=None,admissionReviewVersions=v1

func (p *PodWebhook) Default(ctx context.Context, obj runtime.Object) error {
	log := logf.FromContext(ctx)
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("expected a Pod but got a %T", obj)
	}

	log.V(2).Info("Defaulting Pod")
	// if pod is not part of leaderworkerset, skip
	_, found := pod.Labels[leaderworkerset.SetNameLabelKey]
	if !found {
		return nil
	}

	// adding labels for pods
	index, found := pod.Labels[leaderworkerset.WorkerIndexLabelKey]
	if found && index == "0" {
		// add group index label to group pods
		_, found := pod.Labels[leaderworkerset.GroupIndexLabelKey]
		if !found {
			_, groupIndex := statefulsetutils.GetParentNameAndOrdinal(pod.Name)
			if groupIndex == -1 {
				return fmt.Errorf("parsing pod ordinal for pod %s", pod.Name)
			}
			pod.Labels[leaderworkerset.GroupIndexLabelKey] = fmt.Sprint(groupIndex)
		}
		// add group unique key label for exclusive placement, and use it to check whether the node affinity has been applied
		_, foundGroupKey := pod.Labels[leaderworkerset.GroupUniqueHashLabelKey]
		var groupUniqueKey string
		if !foundGroupKey {
			groupUniqueKey = genGroupUniqueKey(pod.Namespace, pod.Name)
			pod.Labels[leaderworkerset.GroupUniqueHashLabelKey] = groupUniqueKey
		} else {
			groupUniqueKey = pod.Labels[leaderworkerset.GroupUniqueHashLabelKey]
		}
		_, foundEpKey := pod.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
		if foundEpKey && !exclusiveAffinityApplied(*pod) {
			SetExclusiveAffinities(pod, groupUniqueKey)
		}
	} else if !found {
		// since leader pods will have the label already, we expect only worker pods here
		_, workerIndex := statefulsetutils.GetParentNameAndOrdinal(pod.Name)
		if workerIndex == -1 {
			return fmt.Errorf("parsing pod ordinal for pod %s", pod.Name)
		}
		pod.Labels[leaderworkerset.WorkerIndexLabelKey] = fmt.Sprint(workerIndex)
	}

	// injecting env vars if needed
	if acceleratorutils.PodRequestsTPUs(pod.Spec) {
		size, exist := pod.Annotations[leaderworkerset.SizeAnnotationKey]
		if !exist {
			return fmt.Errorf("size annotation is unexpectedly missing for pod %s", pod.Name)
		}
		podCount, err := strconv.Atoi(size)
		if err != nil {
			return err
		}
		if err := acceleratorutils.AddTPUVariables(pod, podCount); err != nil {
			return err
		}
	}
	return nil
}
