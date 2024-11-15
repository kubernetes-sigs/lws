/*
Copyright 2023.

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

package controller

import (
	"bytes"
	"context"
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/history"
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet")

func CreateHeadlessServiceIfNotExists(ctx context.Context, k8sClient client.Client, Scheme *runtime.Scheme, lws *leaderworkerset.LeaderWorkerSet, serviceName string, serviceSelector map[string]string, owner metav1.Object) error {
	log := ctrl.LoggerFrom(ctx)
	// If the headless service does not exist in the namespace, create it.
	var headlessService corev1.Service
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: lws.Namespace}, &headlessService); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		headlessService := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: lws.Namespace,
			},
			Spec: corev1.ServiceSpec{
				ClusterIP:                "None", // defines service as headless
				Selector:                 serviceSelector,
				PublishNotReadyAddresses: true,
			},
		}

		// Set the controller owner reference for garbage collection and reconciliation.
		if err := ctrl.SetControllerReference(owner, &headlessService, Scheme); err != nil {
			return err
		}
		// create the service in the cluster
		log.V(2).Info("Creating headless service.")
		if err := k8sClient.Create(ctx, &headlessService); err != nil {
			return err
		}
	}
	return nil
}

// GetLeaderWorkerSetRevisions returns the current and update ControllerRevisions for leaerWorkerSet. It also
// returns a collision count that records the number of name collisions set saw when creating
// new ControllerRevisions. This count is incremented on every name collision and is used in
// building the ControllerRevision names for name collision avoidance. This method may create
// a new revision, or modify the Revision of an existing revision if an update to set is detected.
// This method expects that revisions is sorted when supplied.
func GetLeaderWorkerSetRevisions(
	ctx context.Context,
	k8sClient client.Client,
	lws *leaderworkerset.LeaderWorkerSet) (*appsv1.ControllerRevision, *appsv1.ControllerRevision, int32, error) {
	var currentRevision *appsv1.ControllerRevision
	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)
	controllerHistory := history.NewHistory(k8sClient, ctx)
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{})
	if err != nil {
		return nil, nil, int32(-1), err
	}
	revisions, err := controllerHistory.ListControllerRevisions(lws, selector)
	if err != nil {
		log.Error(err, "Listing all controller revisions")
		return nil, nil, int32(-1), err
	}
	revisionCount := len(revisions)
	history.SortControllerRevisions(revisions)

	// Use a local copy of set.Status.CollisionCount to avoid modifying set.Status directly.
	// This copy is returned so the value gets carried over to set.Status in updateStatefulSet.
	var collisionCount int32
	if lws.Status.CollisionCount != nil {
		collisionCount = *lws.Status.CollisionCount
	}

	// create a new revision from the current set
	updateRevision, err := NewRevision(lws, NextRevision(revisions), &collisionCount)
	if err != nil {
		log.Error(err, "Creating new revision for lws")
		return nil, nil, collisionCount, err
	}

	// find any equivalent revisions
	equalRevisions := history.FindEqualRevisions(revisions, updateRevision)
	equalCount := len(equalRevisions)

	if equalCount > 0 && history.EqualRevision(revisions[revisionCount-1], equalRevisions[equalCount-1]) {
		// if the equivalent revision is immediately prior the update revision has not changed
		updateRevision = revisions[revisionCount-1]
	} else if equalCount > 0 {
		// if the equivalent revision is not immediately prior we will roll back by incrementing the
		// Revision of the equivalent revision
		updateRevision, err = controllerHistory.UpdateControllerRevision(
			equalRevisions[equalCount-1],
			updateRevision.Revision)
		if err != nil {
			log.Error(err, "updating controller revision")
			return nil, nil, collisionCount, err
		}
	} else {
		//if there is no equivalent revision we create a new one
		updateRevision, err = controllerHistory.CreateControllerRevision(lws, updateRevision, &collisionCount)
		if err != nil {
			log.Error(err, "Creating new controller revision for lws")
			return nil, nil, collisionCount, err
		}
	}

	// attempt to find the revision that corresponds to the current revision
	for i := range revisions {
		if revisions[i].Name == lws.Status.CurrentRevision {
			currentRevision = revisions[i]
			break
		}
	}

	// if the current revision is nil we initialize the history by setting it to the update revision
	if currentRevision == nil {
		currentRevision = updateRevision
	}

	return currentRevision, updateRevision, collisionCount, nil
}

// getPatch returns a strategic merge patch that can be applied to restore a LeaderWorkerSet to a
// previous version. If the returned error is nil the patch is valid. The current state that we save is the
// leaderWorkerTemplate. We can modify this later to encompass more state (or less) and remain compatible with previously
// recorded patches.
func getPatch(lws *leaderworkerset.LeaderWorkerSet) ([]byte, error) {
	str := &bytes.Buffer{}
	err := unstructured.UnstructuredJSONScheme.Encode(lws, str)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	err = json.Unmarshal(str.Bytes(), &raw)
	if err != nil {
		return nil, err
	}
	objCopy := make(map[string]interface{})
	specCopy := make(map[string]interface{})
	spec := raw["spec"].(map[string]interface{})
	template := spec["leaderWorkerTemplate"].(map[string]interface{})
	specCopy["leaderWorkerTemplate"] = template
	template["$patch"] = "replace"
	objCopy["spec"] = specCopy
	patch, err := json.Marshal(objCopy)
	return patch, err
}

// newRevision creates a new ControllerRevision containing a patch that reapplies the target state of LeaderWorkerSet.
// The Revision of the returned ControllerRevision is set to revision. If the returned error is nil, the returned
// ControllerRevision is valid. LeaderWorkerSet revisions are stored as patches that re-apply the current state of set
// to a new LeaderWorkerSet using a strategic merge patch to replace the saved state of the new LeaderWorkerSet.
func NewRevision(lws *leaderworkerset.LeaderWorkerSet, revision int64, collisionCount *int32) (*appsv1.ControllerRevision, error) {
	patch, err := getPatch(lws)
	if err != nil {
		return nil, err
	}
	combinedLabels := make(map[string]string)
	for k, v := range lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Labels {
		combinedLabels[k] = v
	}
	for k, v := range lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Labels {
		combinedLabels[k] = v
	}
	cr, err := history.NewControllerRevision(lws,
		controllerKind,
		combinedLabels,
		runtime.RawExtension{Raw: patch},
		revision,
		collisionCount)
	if err != nil {
		return nil, err
	}
	if cr.ObjectMeta.Annotations == nil {
		cr.ObjectMeta.Annotations = make(map[string]string)
	}
	for key, value := range lws.Annotations {
		cr.ObjectMeta.Annotations[key] = value
	}
	return cr, nil
}

// ApplyRevision returns a new LeaderWorkerSet constructed by restoring the state in revision to set. If the returned error
// is nil, the returned LeaderWorkerSet is valid.
func ApplyRevision(lws *leaderworkerset.LeaderWorkerSet, revision *appsv1.ControllerRevision) (*leaderworkerset.LeaderWorkerSet, error) {
	clone := lws.DeepCopy()
	str := &bytes.Buffer{}
	err := unstructured.UnstructuredJSONScheme.Encode(lws, str)
	if err != nil {
		return nil, err
	}
	patched, err := strategicpatch.StrategicMergePatch(str.Bytes(), revision.Data.Raw, clone)
	if err != nil {
		return nil, err
	}
	restoredLws := &leaderworkerset.LeaderWorkerSet{}
	err = json.Unmarshal(patched, restoredLws)
	if err != nil {
		return nil, err
	}
	return restoredLws, nil
}

// nextRevision finds the next valid revision number based on revisions. If the length of revisions
// is 0 this is 1. Otherwise, it is 1 greater than the largest revision's Revision. This method
// assumes that revisions has been sorted by Revision.
func NextRevision(revisions []*appsv1.ControllerRevision) int64 {
	count := len(revisions)
	if count <= 0 {
		return 1
	}
	return revisions[count-1].Revision + 1
}

// TruncateHistory cleans up all other controller revisions expect the currentRevision and updateRevision
func TruncateHistory(history history.Interface, revisions []*appsv1.ControllerRevision, updateRevision *appsv1.ControllerRevision, currentRevision *appsv1.ControllerRevision) error {
	for i, revision := range revisions {
		if revision.Name != updateRevision.Name && revision.Name != currentRevision.Name {
			if err := history.DeleteControllerRevision(revisions[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
