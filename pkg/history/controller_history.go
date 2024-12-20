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

package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"sort"
	"strconv"

	"github.com/davecgh/go-spew/spew"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"
)

// ControllerRevisionHashLabel is the label used to indicate the hash value of a ControllerRevision's Data.
const ControllerRevisionHashLabel = "controller.kubernetes.io/hash"

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
	revision int64) (*appsv1.ControllerRevision, error) {
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
	cr.Labels[ControllerRevisionHashLabel] = hash
	return cr, nil
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

// SortControllerRevisions sorts revisions by their Revision.
func SortControllerRevisions(revisions []*appsv1.ControllerRevision) {
	sort.Stable(byRevision(revisions))
}

// EqualRevision returns true if lhs and rhs are either both nil, or both point to non-nil ControllerRevisions that
// contain semantically equivalent data. Otherwise this method returns false.
func EqualRevision(lhs *appsv1.ControllerRevision, rhs *appsv1.ControllerRevision) bool {
	var lhsHash, rhsHash *uint32

	if lhs.Labels[leaderworkerset.TemplateRevisionHashKey] == rhs.Labels[leaderworkerset.TemplateRevisionHashKey] {
		return true
	}

	if lhs == nil || rhs == nil {
		return lhs == rhs
	}
	if hs, found := lhs.Labels[ControllerRevisionHashLabel]; found {
		hash, err := strconv.ParseInt(hs, 10, 32)
		if err == nil {
			lhsHash = new(uint32)
			*lhsHash = uint32(hash)
		}
	}
	if hs, found := rhs.Labels[ControllerRevisionHashLabel]; found {
		hash, err := strconv.ParseInt(hs, 10, 32)
		if err == nil {
			rhsHash = new(uint32)
			*rhsHash = uint32(hash)
		}
	}
	if lhsHash != nil && rhsHash != nil && *lhsHash != *rhsHash {
		return false
	}
	return bytes.Equal(lhs.Data.Raw, rhs.Data.Raw) && apiequality.Semantic.DeepEqual(lhs.Data.Object, rhs.Data.Object)
}

// FindEqualRevisions returns all ControllerRevisions in revisions that are equal to needle using EqualRevision as the
// equality test. The returned slice preserves the order of revisions.
func FindEqualRevisions(revisions []*appsv1.ControllerRevision, needle *appsv1.ControllerRevision) []*appsv1.ControllerRevision {
	var eq []*appsv1.ControllerRevision
	for i := range revisions {
		if EqualRevision(revisions[i], needle) {
			eq = append(eq, revisions[i])
		}
	}
	return eq
}

// byRevision implements sort.Interface to allow ControllerRevisions to be sorted by Revision.
type byRevision []*appsv1.ControllerRevision

func (br byRevision) Len() int {
	return len(br)
}

// Less breaks ties first by creation timestamp, then by name
func (br byRevision) Less(i, j int) bool {
	if br[i].Revision == br[j].Revision {
		if br[j].CreationTimestamp.Equal(&br[i].CreationTimestamp) {
			return br[i].Name < br[j].Name
		}
		return br[j].CreationTimestamp.After(br[i].CreationTimestamp.Time)
	}
	return br[i].Revision < br[j].Revision
}

func (br byRevision) Swap(i, j int) {
	br[i], br[j] = br[j], br[i]
}

// Interface provides an interface allowing for management of a Controller's history as realized by recorded
// ControllerRevisions. An instance of Interface can be retrieved from NewHistory. Implementations must treat all
// pointer parameters as "in" parameter, and they must not be mutated.
type Interface interface {
	// ListControllerRevisions lists all ControllerRevisions matching selector and owned by parent or no other
	// controller. If the returned error is nil the returned slice of ControllerRevisions is valid. If the
	// returned error is not nil, the returned slice is not valid.
	ListControllerRevisions(parent metav1.Object, selector labels.Selector) ([]*appsv1.ControllerRevision, error)
	// CreateControllerRevision attempts to create the revision as owned by parent via a ControllerRef. Implementations may
	// cease to attempt to retry creation after some number of attempts and return an error. If the returned
	// error is not nil, creation failed. If the returned error is nil, the returned ControllerRevision has been
	// created.
	CreateControllerRevision(parent metav1.Object, revision *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error)
	// DeleteControllerRevision attempts to delete revision. If the returned error is not nil, deletion has failed.
	DeleteControllerRevision(revision *appsv1.ControllerRevision) error
	// UpdateControllerRevision updates revision such that its Revision is equal to newRevision. Implementations
	// may retry on conflict. If the returned error is nil, the update was successful and returned ControllerRevision
	// is valid. If the returned error is not nil, the update failed and the returned ControllerRevision is invalid.
	UpdateControllerRevision(revision *appsv1.ControllerRevision, newRevision int64) (*appsv1.ControllerRevision, error)
	// AdoptControllerRevision attempts to adopt revision by adding a ControllerRef indicating that the parent
	// Object of parentKind is the owner of revision. If revision is already owned, an error is returned. If the
	// resource patch fails, an error is returned. If no error is returned, the returned ControllerRevision is
	// valid.
	AdoptControllerRevision(parent metav1.Object, parentKind schema.GroupVersionKind, revision *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error)
	// ReleaseControllerRevision attempts to release parent's ownership of revision by removing parent from the
	// OwnerReferences of revision. If an error is returned, parent remains the owner of revision. If no error is
	// returned, the returned ControllerRevision is valid.
	ReleaseControllerRevision(parent metav1.Object, revision *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error)
}

// NewHistory returns an instance of Interface that uses client to communicate with the API Server and lister to list
// ControllerRevisions. This method should be used to create an Interface for all scenarios other than testing.
func NewHistory(context context.Context, k8sclient client.Client) Interface {
	return &realHistory{k8sclient, context}
}

type realHistory struct {
	client.Client
	context context.Context
}

func (rh *realHistory) ListControllerRevisions(parent metav1.Object, selector labels.Selector) ([]*appsv1.ControllerRevision, error) {
	// List all revisions in the namespace that match the selector
	revisionList := new(appsv1.ControllerRevisionList)
	err := rh.List(rh.context, revisionList, client.InNamespace(parent.GetNamespace()), client.MatchingLabelsSelector{Selector: selector})
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

func (rh *realHistory) CreateControllerRevision(parent metav1.Object, revision *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
	// Clone the input
	clone := revision.DeepCopy()

	hash := HashControllerRevision(revision)
	// Update the revisions name
	clone.Name = ControllerRevisionName(parent.GetName(), hash)
	ns := parent.GetNamespace()
	err := rh.Create(rh.context, clone)
	if errors.IsAlreadyExists(err) {
		exists := &appsv1.ControllerRevision{}
		err := rh.Get(rh.context, types.NamespacedName{Namespace: ns, Name: clone.Name}, exists)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(exists.Data.Raw, clone.Data.Raw) {
			return exists, nil
		} else {
			// Since the contents of the revision are used to create the hash, the only way this
			// happens is if the contents of the revision were changed, which is unintended behavior
			return nil, fmt.Errorf("controller Revision with same name but different content exists")
		}
	}
	return clone, err
}

func (rh *realHistory) UpdateControllerRevision(revision *appsv1.ControllerRevision, newRevision int64) (*appsv1.ControllerRevision, error) {
	clone := revision.DeepCopy()
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if clone.Revision == newRevision {
			return nil
		}
		clone.Revision = newRevision
		updateErr := rh.Update(rh.context, clone)
		if updateErr == nil {
			return nil
		}
		updated := &appsv1.ControllerRevision{}
		if err := rh.Get(rh.context, types.NamespacedName{Namespace: clone.Namespace, Name: clone.Name}, updated); err == nil {
			// make a copy so we don't mutate the shared cache
			clone = updated.DeepCopy()
		}
		return updateErr
	})
	return clone, err
}

func (rh *realHistory) DeleteControllerRevision(revision *appsv1.ControllerRevision) error {
	return rh.Delete(rh.context, revision)
}

type objectForPatch struct {
	Metadata objectMetaForPatch `json:"metadata"`
}

// objectMetaForPatch define object meta struct for patch operation
type objectMetaForPatch struct {
	OwnerReferences []metav1.OwnerReference `json:"ownerReferences"`
	UID             types.UID               `json:"uid"`
}

func (rh *realHistory) AdoptControllerRevision(parent metav1.Object, parentKind schema.GroupVersionKind, revision *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
	blockOwnerDeletion := true
	isController := true
	// Return an error if the revision is not orphan
	if owner := metav1.GetControllerOfNoCopy(revision); owner != nil {
		return nil, fmt.Errorf("attempt to adopt revision owned by %v", owner)
	}
	addControllerPatch := objectForPatch{
		Metadata: objectMetaForPatch{
			UID: revision.UID,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         parentKind.GroupVersion().String(),
				Kind:               parentKind.Kind,
				Name:               parent.GetName(),
				UID:                parent.GetUID(),
				Controller:         &isController,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
	}
	patchBytes, err := json.Marshal(&addControllerPatch)
	if err != nil {
		return nil, err
	}
	// Use strategic merge patch to add an owner reference indicating a controller ref
	err = rh.Patch(rh.context, revision, client.RawPatch(types.StrategicMergePatchType, patchBytes))
	return revision, err
}

func (rh *realHistory) ReleaseControllerRevision(parent metav1.Object, revision *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
	dataBytes := GenerateDeleteOwnerRefStrategicMergeBytes(revision.UID, parent.GetUID())
	// Use strategic merge patch to add an owner reference indicating a controller ref
	err := rh.Patch(rh.context, revision, client.RawPatch(types.StrategicMergePatchType, dataBytes))

	if err != nil {
		if errors.IsNotFound(err) {
			// We ignore deleted revisions
			return nil, nil
		}
		if errors.IsInvalid(err) {
			// We ignore cases where the parent no longer owns the revision or where the revision has no
			// owner.
			return nil, nil
		}
	}
	return revision, err
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
