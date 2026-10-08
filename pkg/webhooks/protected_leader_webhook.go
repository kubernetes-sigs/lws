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

package webhooks

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// This separate DELETE-only webhook is restricted by a CEL matchCondition to
// annotated leaders. Ordinary Pod admission and unprotected deletes are unchanged.
// ReplicaSet deletion cost is only a preference; it cannot fence a stale victim
// choice. Admission sees the actual Pod UID and reads the active LWS intent live.
type protectedLeaderWebhook struct{ reader client.Reader }

//+kubebuilder:webhook:path=/validate-protected-leader,mutating=false,failurePolicy=fail,sideEffects=None,groups="",resources=pods,verbs=delete,versions=v1,name=vprotectedleader.kb.io,admissionReviewVersions=v1

func (*protectedLeaderWebhook) ValidateCreate(context.Context, *corev1.Pod) (admission.Warnings, error) {
	return nil, nil
}

func (*protectedLeaderWebhook) ValidateUpdate(context.Context, *corev1.Pod, *corev1.Pod) (admission.Warnings, error) {
	return nil, nil
}

func (w *protectedLeaderWebhook) ValidateDelete(ctx context.Context, pod *corev1.Pod) (admission.Warnings, error) {
	// No availability remains to protect, and PodGC must be able to remove
	// completed/failed groups even without a native restart-policy delete.
	if !pod.DeletionTimestamp.IsZero() || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return nil, nil
	}
	marker := strings.Split(pod.Annotations[leaderv1.GroupScaleProtectionAnnotationKey], "/")
	if len(marker) != 2 || marker[0] == "" || marker[1] != string(pod.UID) || pod.Labels[leaderv1.WorkerIndexLabelKey] != "0" {
		return nil, nil
	}
	uid := marker[0]
	if pod.Annotations[leaderv1.GroupReplacementDeleteAnnotationKey] == string(pod.UID) {
		return nil, nil
	}
	lws := &leaderv1.LeaderWorkerSet{}
	err := w.reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Labels[leaderv1.SetNameLabelKey]}, lws)
	if apierrors.IsNotFound(err) {
		return nil, nil // Never obstruct garbage collection of a removed parent.
	}
	if err != nil {
		return nil, err
	}
	if string(lws.UID) != uid || !lws.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	if id := lws.Annotations[leaderv1.GroupScalePlanAnnotationKey]; id != "" &&
		pod.Annotations[leaderv1.GroupScaleVictimAnnotationKey] == id+"/"+string(pod.UID) {
		return nil, nil
	}
	return nil, fmt.Errorf("leader %s is retained by LeaderWorkerSet %s; only an accepted scale victim or native health replacement may delete it (remove %s for an administrative override)", pod.Name, lws.Name, leaderv1.GroupScaleProtectionAnnotationKey)
}
