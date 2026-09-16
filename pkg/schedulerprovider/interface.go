/*
Copyright 2025.

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

package schedulerprovider

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

var (
	SupportedSchedulerProviders = sets.New("volcano", "kubernetes")
)

const (
	PodGroupNameFmt     = "%s-%s-%s"
	RolePodGroupNameFmt = "%s-%s-%s-%s"
	LWSGroupNameFmt     = "%s-lws"

	// These linkage annotations are defined by Kubernetes KEP-6089. They are
	// not exported by k8s.io/api yet.
	GroupTemplateNameAnnotation       = "scheduling.k8s.io/group-template-name"
	ParentCompositePodGroupAnnotation = "scheduling.k8s.io/parent-compositepodgroup"

	ReasonAPINotAvailable                = "APINotAvailable"
	ReasonUnsupportedProviderCapability  = "UnsupportedProviderCapability"
	ReasonInvalidSchedulingConfiguration = "InvalidSchedulingConfiguration"
	ReasonWorkloadCreateFailed           = "WorkloadCreateFailed"
	ReasonPodGroupCreateFailed           = "PodGroupCreateFailed"
	ReasonParentWorkloadNotReady         = "ParentWorkloadNotReady"
	ReasonPodGroupCleanupBlocked         = "PodGroupCleanupBlocked"
)

// ReconcileError carries a stable LWS condition reason without hiding the
// underlying API error used for retries and diagnostics.
type ReconcileError struct {
	Reason string
	Err    error
}

func (e *ReconcileError) Error() string { return e.Err.Error() }
func (e *ReconcileError) Unwrap() error { return e.Err }

func NewReconcileError(reason string, err error) error {
	return &ReconcileError{Reason: reason, Err: err}
}

func ReconcileErrorReason(err error) string {
	var reconcileErr *ReconcileError
	if errors.As(err, &reconcileErr) {
		return reconcileErr.Reason
	}
	return ReasonInvalidSchedulingConfiguration
}

// SchedulerProvider defines the interface for managing pod group resources
type SchedulerProvider interface {
	// ReconcileScheduling creates controller-owned scheduling prerequisites
	// before the leader StatefulSet is allowed to create Pods. Providers that
	// use the legacy pod-driven lifecycle may implement this as a no-op.
	ReconcileScheduling(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, replicas int32, revision string) error

	// CreatePodGroupIfNotExists creates a PodGroup if it doesn't exist, called by pod controller
	CreatePodGroupIfNotExists(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leaderPod *corev1.Pod) error

	// InjectPodGroupMetadata sets pod meta for PodGroup association, called by webhook
	InjectPodGroupMetadata(pod *corev1.Pod) error
}

// GetPodGroupName returns the name of the PodGroup for a given LeaderWorkerSet name, group index and revision (leaderworkerset.sigs.k8s.io/template-revision-hash).
// An example can be like "lws-1-dd6699c7c", where "lws" is the LeaderWorkerSet name, "1" is the group index, and "dd6699c7c" is the revision hash.
func GetPodGroupName(lwsName, groupIndex, revision string) string {
	return fmt.Sprintf(PodGroupNameFmt, lwsName, groupIndex, revision)
}

// GetRolePodGroupName returns a revision-aware leader or worker PodGroup name.
func GetRolePodGroupName(lwsName, groupIndex, role, revision string) string {
	return fmt.Sprintf(RolePodGroupNameFmt, lwsName, groupIndex, role, revision)
}

// GetLWSGroupName returns the stable whole-LWS PodGroup name.
func GetLWSGroupName(lwsName string) string {
	return fmt.Sprintf(LWSGroupNameFmt, lwsName)
}

// ProviderType defines the type of scheduler provider
type ProviderType string

// NewSchedulerProvider creates a new scheduler provider based on the type
func NewSchedulerProvider(providerType ProviderType, client client.Client) (SchedulerProvider, error) {
	switch providerType {
	case Volcano:
		return NewVolcanoProvider(client), nil
	case Kubernetes:
		return NewKubernetesProvider(client), nil
	default:
		return nil, fmt.Errorf("unsupported scheduler provider type %s, the supported provider list is %v", providerType, SupportedSchedulerProviders.UnsortedList())
	}
}
