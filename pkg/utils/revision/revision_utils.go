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

package revision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"

	"github.com/davecgh/go-spew/spew"
	appsv1 "k8s.io/api/apps/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// Functions in this package are adapted from https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/statefulset/ and
// https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/history/controller_history.go

// NewRevision instantiates a new ControllerRevision containing a patch that reapplies the target state of LeaderWorkerSet.
// The Revision of the returned ControllerRevision is set to revision. If the returned error is nil, the returned
// ControllerRevision is valid. LeaderWorkerSet revisions are stored as patches that re-apply the current state of set
// to a new LeaderWorkerSet using a strategic merge patch to replace the saved state of the new LeaderWorkerSet.
func NewRevision(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, revisionKey string) (*appsv1.ControllerRevision, error) {
	var controllerKind = leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet")
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}})
	if err != nil {
		return nil, err
	}
	revisions, err := ListRevisions(ctx, k8sClient, lws, selector)
	highestRevision := getHighestRevision(revisions)
	revision := int64(1)
	if highestRevision != nil {
		revision = highestRevision.Revision + 1
	}
	if err != nil {
		return nil, err
	}
	patch, err := getPatch(lws)
	if err != nil {
		return nil, err
	}

	cr := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey: lws.Name,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, controllerKind)},
			Namespace:       lws.Namespace,
		},
		Data:     runtime.RawExtension{Raw: patch},
		Revision: revision,
	}

	hash := hashRevision(cr)
	if revisionKey == "" {
		revisionKey = hash
	}
	cr.Name = revisionName(lws.Name, hash, revision)
	cr.Labels[leaderworkerset.RevisionKey] = revisionKey
	return cr, nil
}

func CreateRevision(ctx context.Context, k8sClient client.Client, revision *appsv1.ControllerRevision, lws *leaderworkerset.LeaderWorkerSet) (*appsv1.ControllerRevision, error) {
	if err := k8sClient.Create(ctx, revision); err != nil {
		return nil, err
	}
	return revision, nil
}

func GetRevisionKey(obj metav1.Object) string {
	if obj.GetLabels() != nil {
		return obj.GetLabels()[leaderworkerset.RevisionKey]
	}
	return ""
}

// GetRevision returns the controllerRevision that matches the revisionKey that is passed. A nil controllerRevision will be returned if the passed revisionKey is nil,
// or if no controllerRevisions match the revisionKey passed. If more than one controllerRevision matches, the latest revision will be passed.
func GetRevision(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, revisionKey string) (*appsv1.ControllerRevision, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)
	if revisionKey == "" {
		return nil, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
		leaderworkerset.RevisionKey:     revisionKey,
	}})
	if err != nil {
		return nil, err
	}
	revisions, err := ListRevisions(ctx, k8sClient, lws, selector)
	if err != nil {
		log.Error(err, "Listing all controller revisions")
		return nil, err
	}

	if len(revisions) == 0 {
		return nil, nil
	}

	if len(revisions) > 1 {
		// Since we only create a controllerRevision when the template hash changes, only one should match
		log.Error(err, "More than one revision exists for the given template hash; returning the latest revision")
		return getHighestRevision(revisions), nil
	}

	return revisions[0], nil
}

// ListRevisions lists all ControllerRevisions matching selector and owned by parent or no other
// controller. If the returned error is nil the returned slice of ControllerRevisions is valid. If the
// returned error is not nil, the returned slice is not valid.
func ListRevisions(ctx context.Context, k8sClient client.Client, parent metav1.Object, selector labels.Selector) ([]*appsv1.ControllerRevision, error) {
	// List all revisions in the namespace that match the selector
	revisionList := new(appsv1.ControllerRevisionList)
	err := k8sClient.List(ctx, revisionList, client.InNamespace(parent.GetNamespace()), client.MatchingLabelsSelector{Selector: selector})
	if err != nil {
		return nil, err
	}
	history := revisionList.Items
	var owned []*appsv1.ControllerRevision
	for i := range history {
		ref := metav1.GetControllerOfNoCopy(&history[i])
		if ref == nil || ref.UID == parent.GetUID() {
			owned = append(owned, &history[i])
		}

	}
	return owned, err
}

// ApplyRevision returns a new LeaderWorkerSet constructed by restoring the state in revision to set. If the returned error
// is nil, the returned LeaderWorkerSet is valid.
func ApplyRevision(lws *leaderworkerset.LeaderWorkerSet, revision *appsv1.ControllerRevision) (*leaderworkerset.LeaderWorkerSet, error) {
	// clone := lws.DeepCopy()
	str := &bytes.Buffer{}
	err := unstructured.UnstructuredJSONScheme.Encode(lws, str)
	if err != nil {
		return nil, err
	}
	patched, err := strategicpatch.StrategicMergePatch(str.Bytes(), revision.Data.Raw, lws)
	if err != nil {
		return nil, err
	}
	restoredLws := &leaderworkerset.LeaderWorkerSet{}
	if err = json.Unmarshal(patched, restoredLws); err != nil {
		return nil, err
	}
	return restoredLws, nil
}

// EqualRevision returns true if lhs and rhs are either both nil, if the revisionKey is the same,
// or if they are semantically equivalent.
func EqualRevision(lhs *appsv1.ControllerRevision, rhs *appsv1.ControllerRevision) bool {
	if lhs == nil || rhs == nil {
		return lhs == rhs
	}

	return bytes.Equal(lhs.Data.Raw, rhs.Data.Raw) && apiequality.Semantic.DeepEqual(lhs.Data.Object, rhs.Data.Object)
}

// TruncateRevisions cleans up all other controller revisions except the currentRevision.
// currentRevision is the one that matches the revisionKey that is passed
func TruncateRevisions(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, revisionKey string) error {
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}})
	if err != nil {
		return err
	}
	revisions, err := ListRevisions(ctx, k8sClient, lws, selector)
	if err != nil {
		return err
	}

	for i, revision := range revisions {
		if GetRevisionKey(revision) != revisionKey {
			if err := k8sClient.Delete(ctx, revisions[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// getPatch returns a strategic merge patch that can be applied to restore a LeaderWorkerSet to a
// previous version. If the returned error is nil the patch is valid. The current state that we save is the
// leaderWorkerTemplate and NetworkConfig. We can modify this later to encompass more state (or less) and
// remain compatible with previously recorded patches.
func getPatch(lws *leaderworkerset.LeaderWorkerSet) ([]byte, error) {
	str := &bytes.Buffer{}
	clone := lws.DeepCopy()
	// When upgrading from an LWS version that doesn't contain NetworkConfig, NetworkConfig will be nil
	// until another field in the LWS object is changed triggering the LWS webhook. This allows the revision
	// to be the same before and after the LWS webhook actually defaults the value.
	if clone.Spec.NetworkConfig == nil {
		clone.Spec.NetworkConfig = &leaderworkerset.NetworkConfig{}
		subdomainPolicy := leaderworkerset.SubdomainShared
		clone.Spec.NetworkConfig = &leaderworkerset.NetworkConfig{
			SubdomainPolicy: &subdomainPolicy,
		}
	}

	if err := unstructured.UnstructuredJSONScheme.Encode(clone, str); err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(str.Bytes(), &raw); err != nil {
		return nil, err
	}
	objCopy := make(map[string]interface{})
	specCopy := make(map[string]interface{})
	spec := raw["spec"].(map[string]interface{})
	networkConfig := spec["networkConfig"].(map[string]interface{})
	specCopy["networkConfig"] = networkConfig
	template := spec["leaderWorkerTemplate"].(map[string]interface{})
	specCopy["leaderWorkerTemplate"] = template
	networkConfig["$patch"] = "replace"
	template["$patch"] = "replace"
	objCopy["spec"] = specCopy
	return json.Marshal(objCopy)
}

// getHighestRevision finds the next valid revision number based on revisions. If the length of revisions
// is 0 this is 1. Otherwise, it is 1 greater than the largest revision's Revision. It also returns the revision
// with the highest Revision value.
func getHighestRevision(revisions []*appsv1.ControllerRevision) *appsv1.ControllerRevision {
	count := len(revisions)
	if count <= 0 {
		return nil
	}

	max := int64(0)
	var maxRevision *appsv1.ControllerRevision
	for _, revision := range revisions {
		if max <= revision.Revision {
			max = revision.Revision
			maxRevision = revision
		}
	}
	return maxRevision
}

// revisionName returns the Name for a ControllerRevision in the form prefix-hash-revisionnumber. If the length
// of prefix is greater than 220 bytes, it is truncated to allow for a name that is no larger than 253 bytes.
// revision-number allows us to avoid collisions if the created prefix-hash already exists in the history, since revision
// will be unique.
func revisionName(prefix string, hash string, revisionNumber int64) string {
	if len(prefix) > 220 {
		prefix = prefix[:220]
	}

	return fmt.Sprintf("%s-%s-%v", prefix, hash, revisionNumber)
}

// hashRevision hashes the contents of revision's Data using FNV hashing.
// The returned hash will be a safe encoded string to avoid bad words.
func hashRevision(revision *appsv1.ControllerRevision) string {
	hf := fnv.New32()
	if len(revision.Data.Raw) > 0 {
		hf.Write(revision.Data.Raw)
	}
	if revision.Data.Object != nil {
		deepHashObject(hf, revision.Data.Object)
	}
	return rand.SafeEncodeString(fmt.Sprint(hf.Sum32()))
}

func deepHashObject(hasher hash.Hash, objectToWrite interface{}) {
	hasher.Reset()
	printer := spew.ConfigState{
		Indent:         " ",
		SortKeys:       true,
		DisableMethods: true,
		SpewKeys:       true,
	}
	_, err := printer.Fprintf(hasher, "%#v", objectToWrite)
	if err != nil {
		return
	}
}
