package revision

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/history"
)

// Functions in this package are adapted from https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/statefulset/

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet")

func GetLeaderWorkerSetRevisionFromTemplateHash(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, templateHash string) (*appsv1.ControllerRevision, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)
	controllerHistory := history.NewHistory(ctx, k8sClient)
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{
		leaderworkerset.TemplateRevisionHashKey: templateHash,
	}})
	if err != nil {
		return nil, err
	}
	revisions, err := controllerHistory.ListControllerRevisions(lws, selector)
	if err != nil {
		log.Error(err, "Listing all controller revisions")
		return nil, err
	}

	if len(revisions) == 0 {
		return nil, fmt.Errorf("could not find LWS revision based on %s", templateHash)
	}

	if len(revisions) > 1 {
		// Since we only create a controllerRevision when the template hash changes, only one should match
		return nil, fmt.Errorf("found more than one revision matching templateHash %s", templateHash)
	}

	return revisions[0], nil
}

func ExistingControllerRevisions(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet) (bool, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)
	controllerHistory := history.NewHistory(ctx, k8sClient)
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}})
	if err != nil {
		return false, err
	}
	revisions, err := controllerHistory.ListControllerRevisions(lws, selector)
	if err != nil {
		return false, err
	}
	return len(revisions) > 0, nil
}

// getPatch returns a strategic merge patch that can be applied to restore a LeaderWorkerSet to a
// previous version. If the returned error is nil the patch is valid. The current state that we save is the
// leaderWorkerTemplate and NetworkConfig. We can modify this later to encompass more state (or less) and
// remain compatible with previously recorded patches.

func GetPatch(lws *leaderworkerset.LeaderWorkerSet) ([]byte, error) {
	str := &bytes.Buffer{}
	clone := lws.DeepCopy()
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
	specCopy["networkConfig"] = spec["networkConfig"]
	specCopy["leaderWorkerTemplate"] = spec["leaderWorkerTemplate"].(map[string]interface{})
	specCopy["$patch"] = "replace"
	objCopy["spec"] = specCopy
	return json.Marshal(objCopy)
}

func CreateLeaderWorkerSetRevision(
	ctx context.Context,
	k8sClient client.Client,
	lws *leaderworkerset.LeaderWorkerSet,
	templateHash string) error {
	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)
	controllerHistory := history.NewHistory(ctx, k8sClient)
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}})
	if err != nil {
		return err
	}
	revisions, err := controllerHistory.ListControllerRevisions(lws, selector)
	if err != nil {
		log.Error(err, "Listing all controller revisions")
		return err
	}

	currentRevision, err := NewRevision(lws, NextRevision(revisions), templateHash)
	if err != nil {
		log.Error(err, "Creating new revision for lws")
		return err
	}

	_, err = controllerHistory.CreateControllerRevision(lws, currentRevision)
	log.V(2).Info("Created new controller revision")
	if err != nil {
		log.Error(err, "Creating new controller revision for lws")
		return err
	}

	return nil
}

// newRevision creates a new ControllerRevision containing a patch that reapplies the target state of LeaderWorkerSet.
// The Revision of the returned ControllerRevision is set to revision. If the returned error is nil, the returned
// ControllerRevision is valid. LeaderWorkerSet revisions are stored as patches that re-apply the current state of set
// to a new LeaderWorkerSet using a strategic merge patch to replace the saved state of the new LeaderWorkerSet.
func NewRevision(lws *leaderworkerset.LeaderWorkerSet, revision int64, templateHash string) (*appsv1.ControllerRevision, error) {
	patch, err := GetPatch(lws)
	if err != nil {
		return nil, err
	}

	return history.NewControllerRevision(lws,
		controllerKind,
		map[string]string{
			leaderworkerset.TemplateRevisionHashKey: templateHash,
			leaderworkerset.SetNameLabelKey:         lws.Name,
		},
		runtime.RawExtension{Raw: patch},
		revision)
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

// nextRevision finds the next valid revision number based on revisions. If the length of revisions
// is 0 this is 1. Otherwise, it is 1 greater than the largest revision's Revision. This method
// assumes that revisions has been sorted by Revision.
func NextRevision(revisions []*appsv1.ControllerRevision) int64 {
	count := len(revisions)
	if count <= 0 {
		return 1
	}

	max := int64(1)
	for _, revision := range revisions {
		if max < revision.Revision {
			max = revision.Revision
		}
	}
	return max + 1
}

// TruncateHistory cleans up all other controller revisions except the currentRevision.
// currentRevision is the one that matches the templateHash that is passed
func TruncateHistory(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, templateHash string) error {
	controllerHistory := history.NewHistory(ctx, k8sClient)
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}})
	if err != nil {
		return err
	}
	revisions, err := controllerHistory.ListControllerRevisions(lws, selector)
	if err != nil {
		return err
	}

	for i, revision := range revisions {
		if revision.Labels[leaderworkerset.TemplateRevisionHashKey] != templateHash {
			if err := controllerHistory.DeleteControllerRevision(revisions[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func EqualLeaderWorkerTemplates(lhs *leaderworkerset.LeaderWorkerSet, rhs *leaderworkerset.LeaderWorkerSet) bool {
	if !reflect.DeepEqual(lhs.Spec.LeaderWorkerTemplate, rhs.Spec.LeaderWorkerTemplate) {
		return false
	}
	if (lhs.Spec.NetworkConfig == nil || string(*lhs.Spec.NetworkConfig.SubdomainPolicy) == string(leaderworkerset.SubdomainShared)) && (rhs.Spec.NetworkConfig == nil || string(*rhs.Spec.NetworkConfig.SubdomainPolicy) == string(leaderworkerset.SubdomainShared)) {
		return true
	}

	if lhs.Spec.NetworkConfig == nil || rhs.Spec.NetworkConfig == nil {
		return false
	}

	return string(*lhs.Spec.NetworkConfig.SubdomainPolicy) == string(*rhs.Spec.NetworkConfig.SubdomainPolicy)
}

// Sha1Hash accepts an input string and returns the 40 character SHA1 hash digest of the input string.
func Sha1Hash(s string) string {
	h := sha1.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func LeaderWorkerTemplateHash(lws *leaderworkerset.LeaderWorkerSet) string {
	if lws.Spec.NetworkConfig == nil || string(*lws.Spec.NetworkConfig.SubdomainPolicy) == string(leaderworkerset.SubdomainShared) {
		return Sha1Hash(lws.Spec.LeaderWorkerTemplate.LeaderTemplate.String() +
			lws.Spec.LeaderWorkerTemplate.WorkerTemplate.String())
	}

	return Sha1Hash(lws.Spec.LeaderWorkerTemplate.LeaderTemplate.String() +
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.String() + string(*lws.Spec.NetworkConfig.SubdomainPolicy))
}
