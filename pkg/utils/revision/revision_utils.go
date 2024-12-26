package revision

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"

	"github.com/davecgh/go-spew/spew"
	appsv1 "k8s.io/api/apps/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// Functions in this package are adapted from https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/statefulset/ and
// https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/history/controller_history.go

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet")

// ControllerRevisionName returns the Name for a ControllerRevision in the form prefix-hash. If the length
// of prefix is greater than 223 bytes, it is truncated to allow for a name that is no larger than 253 bytes.
func ControllerRevisionName(prefix string, hash string) string {
	if len(prefix) > 223 {
		prefix = prefix[:223]
	}

	return fmt.Sprintf("%s-%s", prefix, hash)
}

// NewControllerRevision returns a ControllerRevision with a ControllerRef pointing to parent and indicating that
// parent is of parentKind. The ControllerRevision has labels matching template labels, contains Data equal to data, and
// has a Revision equal to revision. If the returned error is nil, the returned ControllerRevision is valid. If the
// returned error is not nil, the returned ControllerRevision is invalid for use.
func NewControllerRevision(parent metav1.Object,
	parentKind schema.GroupVersionKind,
	templateLabels map[string]string,
	data runtime.RawExtension,
	revision int64) *appsv1.ControllerRevision {
	labelMap := make(map[string]string)
	for k, v := range templateLabels {
		labelMap[k] = v
	}
	cr := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Labels:          labelMap,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, parentKind)},
			Namespace:       parent.GetNamespace(),
		},
		Data:     data,
		Revision: revision,
	}
	hash := HashControllerRevision(cr)
	cr.Name = ControllerRevisionName(parent.GetName(), hash)
	if cr.Labels[leaderworkerset.TemplateRevisionHashKey] == "" {
		cr.Labels[leaderworkerset.TemplateRevisionHashKey] = hash
	}
	return cr
}

// HashControllerRevision hashes the contents of revision's Data using FNV hashing.
// The returned hash will be a safe encoded string to avoid bad words.
func HashControllerRevision(revision *appsv1.ControllerRevision) string {
	hf := fnv.New32()
	if len(revision.Data.Raw) > 0 {
		hf.Write(revision.Data.Raw)
	}
	if revision.Data.Object != nil {
		DeepHashObject(hf, revision.Data.Object)
	}
	return rand.SafeEncodeString(fmt.Sprint(hf.Sum32()))
}

// EqualRevision returns true if lhs and rhs are either both nil, if the templateRevisionHash is the same,
// or if they are semantically equivalent.
func EqualRevision(lhs *appsv1.ControllerRevision, rhs *appsv1.ControllerRevision) bool {

	if lhs == nil || rhs == nil {
		return lhs == rhs
	}

	if lhs.Labels[leaderworkerset.TemplateRevisionHashKey] == rhs.Labels[leaderworkerset.TemplateRevisionHashKey] {
		return true
	}

	return bytes.Equal(lhs.Data.Raw, rhs.Data.Raw) && apiequality.Semantic.DeepEqual(lhs.Data.Object, rhs.Data.Object)
}

type History struct {
	client.Client
	context context.Context
}

// ListControllerRevisions lists all ControllerRevisions matching selector and owned by parent or no other
// controller. If the returned error is nil the returned slice of ControllerRevisions is valid. If the
// returned error is not nil, the returned slice is not valid.
func (h *History) ListControllerRevisions(parent metav1.Object, selector labels.Selector) ([]*appsv1.ControllerRevision, error) {
	// List all revisions in the namespace that match the selector
	revisionList := new(appsv1.ControllerRevisionList)
	err := h.List(h.context, revisionList, client.InNamespace(parent.GetNamespace()), client.MatchingLabelsSelector{Selector: selector})
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

// CreateControllerRevision attempts to create the revision as owned by parent via a ControllerRef. If the returned
// error is not nil, creation failed. If the returned error is nil, the returned ControllerRevision has been
// created.
func (h *History) CreateControllerRevision(parent metav1.Object, revision *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
	ns := parent.GetNamespace()
	err := h.Create(h.context, revision)
	if errors.IsAlreadyExists(err) {
		exists := &appsv1.ControllerRevision{}
		err := h.Get(h.context, types.NamespacedName{Namespace: ns, Name: revision.Name}, exists)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(exists.Data.Raw, revision.Data.Raw) {
			return exists, nil
		} else {
			// Since the contents of the revision are used to create the hash, the only way this
			// happens is if the contents of the revision were changed, which is unintended behavior
			return nil, fmt.Errorf("controller Revision with same name but different content exists")
		}
	}
	if err != nil {
		return nil, err
	}
	// Fetched the controller revision that was created, in case the revision webhook modified it.
	created := &appsv1.ControllerRevision{}
	if err := h.Get(h.context, types.NamespacedName{Namespace: ns, Name: revision.Name}, created); err != nil {
		return nil, err
	}
	return created, err
}

// DeleteControllerRevision attempts to delete revision. If the returned error is not nil, deletion has failed.
func (h *History) DeleteControllerRevision(revision *appsv1.ControllerRevision) error {
	return h.Delete(h.context, revision)
}

func DeepHashObject(hasher hash.Hash, objectToWrite interface{}) {
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

func GenerateDeleteOwnerRefStrategicMergeBytes(revisionUID types.UID, parentUID types.UID) []byte {
	return []byte(fmt.Sprintf(`{"metadata":{"ownerReferences":[{"$patch":"delete","uid":"%s"}],"uid":"%s"}}`, revisionUID, parentUID))
}

func GetLeaderWorkerSetRevisionFromTemplateHash(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, templateHash string) (*appsv1.ControllerRevision, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)
	controllerHistory := History{Client: k8sClient, context: ctx}
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
		return nil, nil
	}

	if len(revisions) > 1 {
		// Since we only create a controllerRevision when the template hash changes, only one should match
		log.Error(err, "More than one revision exists for the given templateHash")
		return revisions[len(revisions)-1], nil
	}

	return revisions[0], nil
}

// GetPatch returns a strategic merge patch that can be applied to restore a LeaderWorkerSet to a
// previous version. If the returned error is nil the patch is valid. The current state that we save is the
// leaderWorkerTemplate and NetworkConfig. We can modify this later to encompass more state (or less) and
// remain compatible with previously recorded patches.
func GetPatch(lws *leaderworkerset.LeaderWorkerSet) ([]byte, error) {
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
	templateHash string) (*appsv1.ControllerRevision, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)
	controllerHistory := History{Client: k8sClient, context: ctx}
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}})
	if err != nil {
		return nil, err
	}
	revisions, err := controllerHistory.ListControllerRevisions(lws, selector)
	if err != nil {
		log.Error(err, "Listing all controller revisions")
		return nil, err
	}

	currentRevision, err := NewRevision(lws, NextRevision(revisions), templateHash)
	if err != nil {
		log.Error(err, "Creating new revision for lws")
		return nil, err
	}

	return controllerHistory.CreateControllerRevision(lws, currentRevision)
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

	return NewControllerRevision(lws,
		controllerKind,
		map[string]string{
			leaderworkerset.TemplateRevisionHashKey: templateHash,
			leaderworkerset.SetNameLabelKey:         lws.Name,
		},
		runtime.RawExtension{Raw: patch},
		revision), nil
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
	controllerHistory := History{Client: k8sClient, context: ctx}
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

// Sha1Hash accepts an input string and returns the 40 character SHA1 hash digest of the input string.
func Sha1Hash(s string) string {
	h := sha1.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}
